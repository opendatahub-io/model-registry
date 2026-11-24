package mlflow

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	platformfilter "github.com/kubeflow/hub/internal/platform/db/filter"
	"github.com/kubeflow/hub/pkg/api"
	"github.com/kubeflow/hub/pkg/openapi"
)

// Sort keys accepted by the platform's pagination scope, mapped to the
// object columns they order. Unknown names fall back to id ordering, as
// the platform's allowed-column map does.
var queryOrderByColumns = map[string]string{
	"id":               "id",
	"ID":               "id",
	"CREATE_TIME":      "createTimeSinceEpoch",
	"LAST_UPDATE_TIME": "lastUpdateTimeSinceEpoch",
}

func normalizeOrderBy(orderBy *string) string {
	column, ok := queryOrderByColumns[strings.TrimSpace(derefString(orderBy))]
	if !ok {
		return "id"
	}
	return column
}

func normalizeSortOrder(sortOrder *string) string {
	switch strings.TrimSpace(derefString(sortOrder)) {
	case "DESC":
		return "DESC"
	default:
		return "ASC"
	}
}

// sortKeyValue is the numeric ordering key of one object.
func sortKeyValue(object Object, orderBy string) int64 {
	switch orderBy {
	case "createTimeSinceEpoch":
		return object.CreateTimeSinceEpoch
	case "lastUpdateTimeSinceEpoch":
		return object.LastUpdateTimeSinceEpoch
	default:
		return int64(object.ID)
	}
}

// sortObjects orders objects by the sort key with the id as tiebreaker,
// both in the requested direction, matching the keyset predicate the
// page cursor encodes.
func sortObjects(objects []Object, orderBy, sortOrder string) {
	descending := sortOrder == "DESC"
	sort.Slice(objects, func(i, j int) bool {
		left, right := sortKeyValue(objects[i], orderBy), sortKeyValue(objects[j], orderBy)
		if left != right {
			return (left < right) != descending
		}
		return (objects[i].ID < objects[j].ID) != descending
	})
}

// parseFilterQuery parses the REST filter query. An absent or blank
// query is no filter; a malformed query is a bad request.
func parseFilterQuery(filterQuery *string) (*platformfilter.FilterExpression, error) {
	query := strings.TrimSpace(derefString(filterQuery))
	if query == "" {
		return nil, nil
	}
	expr, err := platformfilter.Parse(query)
	if err != nil {
		return nil, fmt.Errorf("parsing the filter query %q: %w: %w", query, err, api.ErrBadRequest)
	}
	return expr, nil
}

// refreshForQuery refreshes the registry view before a read, so a query
// answers from a current observation. A failed refresh fails the query
// rather than answering from stale data.
func (s *Service) refreshForQuery(ctx context.Context) error {
	if err := s.Refresh(ctx); err != nil {
		return fmt.Errorf("refreshing the registry view before the query: %w: %w", err, api.ErrUnavailable)
	}
	return nil
}

// objectQuery is one list request's scope: the object kind, an object
// parent (registered model for versions), or an artifact link parent
// (model version for artifacts).
type objectQuery struct {
	kind      ObjectKind
	parentID  *int32
	versionID *int32
}

// parentScopeKey renders the scope a page cursor is bound to.
func (q objectQuery) parentScopeKey() string {
	if q.parentID != nil {
		return strconv.FormatInt(int64(*q.parentID), 10)
	}
	if q.versionID != nil {
		return "version:" + strconv.FormatInt(int64(*q.versionID), 10)
	}
	return ""
}

// versionLinkedArtifacts returns the artifact ids linked to one version.
func (s *Service) versionLinkedArtifacts(ctx context.Context, versionID int32) (map[int32]bool, error) {
	var links []ArtifactLink
	err := s.store.DB().WithContext(ctx).Where("version_id = ?", versionID).Find(&links).Error
	if err != nil {
		return nil, mapDBError(err)
	}
	linked := make(map[int32]bool, len(links))
	for _, link := range links {
		linked[link.ArtifactID] = true
	}
	return linked, nil
}

// queryObjects runs the full list pipeline: refresh, scope, filter,
// sort, keyset-resume, and size. It returns the page's objects and the
// next page token, nil on the final page.
func (s *Service) queryObjects(ctx context.Context, q objectQuery,
	opts api.ListOptions) ([]Object, *string, error) {
	if err := s.refreshForQuery(ctx); err != nil {
		return nil, nil, err
	}

	objects, err := s.store.ListObjects(ctx, q.kind, q.parentID)
	if err != nil {
		return nil, nil, err
	}

	if q.versionID != nil {
		linked, err := s.versionLinkedArtifacts(ctx, *q.versionID)
		if err != nil {
			return nil, nil, err
		}
		scoped := make([]Object, 0, len(objects))
		for _, object := range objects {
			if linked[object.ID] {
				scoped = append(scoped, object)
			}
		}
		objects = scoped
	}

	orderBy := normalizeOrderBy(opts.OrderBy)
	sortOrder := normalizeSortOrder(opts.SortOrder)

	expr, err := parseFilterQuery(opts.FilterQuery)
	if err != nil {
		return nil, nil, err
	}
	if expr != nil {
		matching := make([]Object, 0, len(objects))
		for _, object := range objects {
			match, err := EvaluateFilter(expr, object, objects)
			if err != nil {
				return nil, nil, err
			}
			if match {
				matching = append(matching, object)
			}
		}
		objects = matching
	}

	sortObjects(objects, orderBy, sortOrder)

	shape := PageCursor{
		Version:    pageCursorVersion,
		Kind:       string(q.kind),
		ParentID:   q.parentScopeKey(),
		OrderBy:    orderBy,
		SortOrder:  sortOrder,
		FilterHash: FilterHash(opts.FilterQuery),
	}

	if token := strings.TrimSpace(derefString(opts.NextPageToken)); token != "" {
		lastID, cursor, err := DecodePageToken(token, shape)
		if err != nil {
			return nil, nil, err
		}
		lastValue, err := strconv.ParseInt(cursor.LastValue, 10, 64)
		if err != nil {
			return nil, nil, fmt.Errorf("the page cursor value %q is not numeric: %w",
				cursor.LastValue, api.ErrBadRequest)
		}
		objects = objectsAfterCursor(objects, orderBy, sortOrder, lastID, lastValue)
	}

	pageSize := int32(0)
	if opts.PageSize != nil {
		pageSize = *opts.PageSize
	}
	if pageSize < 0 {
		return nil, nil, fmt.Errorf("the page size %d is negative: %w", pageSize, api.ErrBadRequest)
	}

	// A positive page size keeps one row beyond the page to detect a
	// continuation, then truncates, as the platform repository does.
	hasMore := pageSize > 0 && int64(len(objects)) > int64(pageSize)
	if hasMore {
		objects = objects[:pageSize]
	}

	var next *string
	if hasMore && len(objects) > 0 {
		last := objects[len(objects)-1]
		token, err := EncodePageToken(last.ID, PageCursor{
			Version:    shape.Version,
			Kind:       shape.Kind,
			ParentID:   shape.ParentID,
			OrderBy:    orderBy,
			SortOrder:  sortOrder,
			LastValue:  strconv.FormatInt(sortKeyValue(last, orderBy), 10),
			FilterHash: shape.FilterHash,
		})
		if err != nil {
			return nil, nil, err
		}
		next = &token
	}
	return objects, next, nil
}

// objectsAfterCursor applies the keyset predicate: objects strictly
// after the cursor position in the current sort order.
func objectsAfterCursor(objects []Object, orderBy, sortOrder string, lastID int32, lastValue int64) []Object {
	descending := sortOrder == "DESC"
	after := func(value int64, id int32) bool {
		if value != lastValue {
			if descending {
				return value < lastValue
			}
			return value > lastValue
		}
		if descending {
			return id < lastID
		}
		return id > lastID
	}
	remaining := make([]Object, 0, len(objects))
	for _, object := range objects {
		if after(sortKeyValue(object, orderBy), object.ID) {
			remaining = append(remaining, object)
		}
	}
	return remaining
}

// GetRegisteredModelByParams finds the committed registered model
// matching the supplied name or external id.
func (s *Service) GetRegisteredModelByParams(name *string,
	externalId *string) (*openapi.RegisteredModel, error) {
	if name == nil && externalId == nil {
		return nil, fmt.Errorf("invalid parameters call, supply either name or externalId: %w", api.ErrBadRequest)
	}
	if err := s.refreshForQuery(s.ctx); err != nil {
		return nil, err
	}
	object, err := s.store.FindObject(s.ctx, KindRegisteredModel, name, nil, externalId)
	if err != nil {
		return nil, translateFindError(err, "registered models", name, nil, externalId)
	}
	return modelView(object)
}

// GetRegisteredModels returns the committed registered models, filtered,
// ordered, and sized by the list options.
func (s *Service) GetRegisteredModels(listOptions api.ListOptions) (*openapi.RegisteredModelList, error) {
	objects, next, err := s.queryObjects(s.ctx, objectQuery{kind: KindRegisteredModel}, listOptions)
	if err != nil {
		return nil, err
	}
	list := &openapi.RegisteredModelList{
		NextPageToken: derefString(next),
		PageSize:      derefInt32(listOptions.PageSize),
		Size:          int32(len(objects)),
		Items:         make([]openapi.RegisteredModel, 0, len(objects)),
	}
	for _, object := range objects {
		model, err := modelView(object)
		if err != nil {
			return nil, err
		}
		list.Items = append(list.Items, *model)
	}
	return list, nil
}

// GetModelVersionByParams finds the committed model version matching
// the supplied name and registered model, or the external id.
func (s *Service) GetModelVersionByParams(versionName *string, registeredModelId *string,
	externalId *string) (*openapi.ModelVersion, error) {
	if (versionName == nil || registeredModelId == nil) && externalId == nil {
		return nil, fmt.Errorf("invalid parameters call, supply either (name and registeredModelId), or externalId: %w", api.ErrBadRequest)
	}
	var parentID *int32
	if registeredModelId != nil {
		id, err := parseResourceID(*registeredModelId)
		if err != nil {
			return nil, fmt.Errorf("registered model id %q is not a valid id: %w",
				*registeredModelId, api.ErrBadRequest)
		}
		parentID = &id
	}
	if err := s.refreshForQuery(s.ctx); err != nil {
		return nil, err
	}
	object, err := s.store.FindObject(s.ctx, KindModelVersion, versionName, parentID, externalId)
	if err != nil {
		return nil, translateFindError(err, "model versions", versionName, registeredModelId, externalId)
	}
	return versionView(object)
}

// GetModelVersions returns the committed model versions, filtered,
// ordered, and sized by the list options. A registered model id scopes
// the versions to one model.
func (s *Service) GetModelVersions(listOptions api.ListOptions,
	registeredModelId *string) (*openapi.ModelVersionList, error) {
	query := objectQuery{kind: KindModelVersion}
	if registeredModelId != nil {
		id, err := parseResourceID(*registeredModelId)
		if err != nil {
			return nil, fmt.Errorf("registered model id %q is not a valid id: %w",
				*registeredModelId, api.ErrBadRequest)
		}
		query.parentID = &id
	}
	objects, next, err := s.queryObjects(s.ctx, query, listOptions)
	if err != nil {
		return nil, err
	}
	list := &openapi.ModelVersionList{
		NextPageToken: derefString(next),
		PageSize:      derefInt32(listOptions.PageSize),
		Size:          int32(len(objects)),
		Items:         make([]openapi.ModelVersion, 0, len(objects)),
	}
	for _, object := range objects {
		version, err := versionView(object)
		if err != nil {
			return nil, err
		}
		list.Items = append(list.Items, *version)
	}
	return list, nil
}

// findModelArtifactByParams finds the committed model artifact matching
// every supplied predicate. A parent resource id is a model version
// whose artifact links scope the search.
func (s *Service) findModelArtifactByParams(artifactName *string, parentResourceId *string,
	externalId *string) (*openapi.ModelArtifact, error) {
	var versionID *int32
	if parentResourceId != nil {
		id, err := parseResourceID(*parentResourceId)
		if err != nil {
			return nil, fmt.Errorf("parent resource id %q is not a valid id: %w",
				*parentResourceId, api.ErrBadRequest)
		}
		versionID = &id
	}
	if err := s.refreshForQuery(s.ctx); err != nil {
		return nil, err
	}
	if artifactName != nil && versionID != nil {
		object, err := s.store.FindArtifactInVersion(s.ctx, *artifactName, *versionID)
		if err != nil {
			return nil, translateFindError(err, "artifacts", artifactName, parentResourceId, externalId)
		}
		if externalId != nil && derefString(object.ExternalID) != *externalId {
			return nil, translateFindError(api.ErrNotFound, "artifacts", artifactName, parentResourceId, externalId)
		}
		return artifactView(object)
	}
	objects, err := s.store.ListObjects(s.ctx, KindModelArtifact, nil)
	if err != nil {
		return nil, err
	}
	if versionID != nil {
		linked, err := s.versionLinkedArtifacts(s.ctx, *versionID)
		if err != nil {
			return nil, err
		}
		scoped := objects[:0]
		for _, object := range objects {
			if linked[object.ID] {
				scoped = append(scoped, object)
			}
		}
		objects = scoped
	}
	matching := objects[:0]
	for _, object := range objects {
		if artifactName != nil && object.Name != *artifactName {
			continue
		}
		if externalId != nil && derefString(object.ExternalID) != *externalId {
			continue
		}
		matching = append(matching, object)
	}
	switch len(matching) {
	case 0:
		return nil, fmt.Errorf("no artifacts found for name=%v, parentResourceId=%v, externalId=%v: %w",
			derefString(artifactName), derefString(parentResourceId), derefString(externalId), api.ErrNotFound)
	case 1:
		return artifactView(matching[0])
	default:
		return nil, fmt.Errorf("multiple artifacts found for name=%v, parentResourceId=%v, externalId=%v: %w",
			derefString(artifactName), derefString(parentResourceId), derefString(externalId), api.ErrNotFound)
	}
}

// GetArtifactByParams finds the model artifact matching the supplied
// name and parent resource, or the external id.
func (s *Service) GetArtifactByParams(artifactName *string, parentResourceId *string,
	externalId *string) (*openapi.Artifact, error) {
	if (artifactName == nil || parentResourceId == nil) && externalId == nil {
		return nil, fmt.Errorf("invalid parameters call, supply either (artifactName and parentResourceId), or externalId: %w", api.ErrBadRequest)
	}
	view, err := s.findModelArtifactByParams(artifactName, parentResourceId, externalId)
	if err != nil {
		return nil, err
	}
	artifact := openapi.ModelArtifactAsArtifact(view)
	return &artifact, nil
}

// GetArtifacts returns the committed artifacts of the requested type,
// filtered, ordered, and sized by the list options. Only model
// artifacts exist in this backend; other types list empty.
func (s *Service) GetArtifacts(artifactType openapi.ArtifactTypeQueryParam, listOptions api.ListOptions,
	parentResourceId *string) (*openapi.ArtifactList, error) {
	list := &openapi.ArtifactList{Items: []openapi.Artifact{}}
	if artifactType != "" && artifactType != openapi.ARTIFACTTYPEQUERYPARAM_MODEL_ARTIFACT {
		return list, nil
	}
	views, next, err := s.listModelArtifactViews(listOptions, parentResourceId)
	if err != nil {
		return nil, err
	}
	list.NextPageToken = derefString(next)
	list.PageSize = derefInt32(listOptions.PageSize)
	list.Size = int32(len(views))
	for _, view := range views {
		list.Items = append(list.Items, openapi.ModelArtifactAsArtifact(&view))
	}
	return list, nil
}

// GetModelArtifactByParams finds the model artifact matching the
// supplied name and parent resource, or the external id.
func (s *Service) GetModelArtifactByParams(artifactName *string, parentResourceId *string,
	externalId *string) (*openapi.ModelArtifact, error) {
	if (artifactName == nil || parentResourceId == nil) && externalId == nil {
		return nil, fmt.Errorf("invalid parameters call, supply either (artifactName and parentResourceId), or externalId: %w", api.ErrBadRequest)
	}
	return s.findModelArtifactByParams(artifactName, parentResourceId, externalId)
}

// GetModelArtifacts returns the committed model artifacts, filtered,
// ordered, and sized by the list options. A parent resource id scopes
// the artifacts to one model version's links.
func (s *Service) GetModelArtifacts(listOptions api.ListOptions,
	parentResourceId *string) (*openapi.ModelArtifactList, error) {
	views, next, err := s.listModelArtifactViews(listOptions, parentResourceId)
	if err != nil {
		return nil, err
	}
	list := &openapi.ModelArtifactList{
		NextPageToken: derefString(next),
		PageSize:      derefInt32(listOptions.PageSize),
		Size:          int32(len(views)),
		Items:         make([]openapi.ModelArtifact, 0, len(views)),
	}
	list.Items = append(list.Items, views...)
	return list, nil
}

// listModelArtifactViews runs the artifact list pipeline and renders
// each committed object.
func (s *Service) listModelArtifactViews(listOptions api.ListOptions,
	parentResourceId *string) ([]openapi.ModelArtifact, *string, error) {
	query := objectQuery{kind: KindModelArtifact}
	if parentResourceId != nil {
		id, err := parseResourceID(*parentResourceId)
		if err != nil {
			return nil, nil, fmt.Errorf("parent resource id %q is not a valid id: %w",
				*parentResourceId, api.ErrBadRequest)
		}
		query.versionID = &id
	}
	objects, next, err := s.queryObjects(s.ctx, query, listOptions)
	if err != nil {
		return nil, nil, err
	}
	views := make([]openapi.ModelArtifact, 0, len(objects))
	for _, object := range objects {
		view, err := artifactView(object)
		if err != nil {
			return nil, nil, err
		}
		views = append(views, *view)
	}
	return views, next, nil
}

// translateFindError renders a failed params lookup with the platform's
// message shape: zero matches are not found, and ambiguous matches are
// reported as not found too, never as a conflict.
func translateFindError(err error, what string, name, parent, externalId *string) error {
	if errors.Is(err, api.ErrConflict) {
		return fmt.Errorf("multiple %s found for name=%v, parentResourceId=%v, externalId=%v: %w",
			what, derefString(name), derefString(parent), derefString(externalId), api.ErrNotFound)
	}
	if errors.Is(err, api.ErrNotFound) {
		return fmt.Errorf("no %s found for name=%v, parentResourceId=%v, externalId=%v: %w",
			what, derefString(name), derefString(parent), derefString(externalId), api.ErrNotFound)
	}
	return err
}

func derefInt32(value *int32) int32 {
	if value == nil {
		return 0
	}
	return *value
}
