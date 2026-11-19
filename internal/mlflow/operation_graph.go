package mlflow

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"

	"github.com/kubeflow/hub/pkg/api"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func graphRoot(operation Operation) string {
	if operation.AggregateOperationUUID != nil {
		return *operation.AggregateOperationUUID
	}
	return operation.OperationUUID
}

// semanticGraphHash excludes generated identities and public presentation times,
// while retaining original typed metadata, desired values, and graph topology.
func semanticGraphHash(root Operation, children []Operation) (string, error) {
	nodes := append([]Operation{root}, children...)
	names := map[string]string{}
	for _, op := range nodes {
		names[op.OperationUUID] = fmt.Sprintf("%d/%s", op.ResourceID, op.Kind)
	}
	normalizeRef := func(id string) string {
		if name, ok := names[id]; ok {
			return name
		}
		return id
	}
	type intent struct {
		ResourceID int32
		Kind       string
		Revision   int64
		Generation int64
		Candidate  any
	}
	intents := make([]intent, 0, len(nodes))
	for _, op := range nodes {
		candidate, err := decodeMutationCandidate(op.CandidateJSON)
		if err != nil {
			return "", err
		}
		value := intent{ResourceID: op.ResourceID, Kind: op.Kind, Revision: op.ExpectedRevision, Generation: op.Generation}
		if candidate == nil {
			raw, err := canonicalDesiredResource([]byte(op.CandidateJSON))
			if err != nil {
				return "", err
			}
			value.Candidate = json.RawMessage(raw)
		} else {
			// Codec envelopes, read sets, and observed native projections are
			// execution evidence, not caller-desired state. Keep the original
			// payload immutable and normalize only this decoded hash view.
			candidate.CodecVersion = 0
			candidate.ReadRevisions = nil
			candidate.BindingReadRevisions = nil
			if op.Kind == OperationKindVersionReplaceChild {
				plan, err := legacyNativePlan(op)
				if err != nil {
					return "", err
				}
				if plan == nil {
					return "", fmt.Errorf("child %s has no recorded work: %w", op.OperationUUID, api.ErrConflict)
				}
				candidate.Objects = nil
				candidate.NativeWork = &NativeVersionPlan{LogicalVersionID: plan.LogicalVersionID, ModelName: plan.ModelName, Source: plan.Source, Generation: plan.Generation}
			} else {
				candidate.NativeWork = nil
			}
			for i := range candidate.Objects {
				change := &candidate.Objects[i]
				change.Snapshot.Resource, err = canonicalDesiredResource(change.Snapshot.Resource)
				if err != nil {
					return "", err
				}
				change.Snapshot.CodecVersion = 0
				change.Snapshot.TagKeys = nil
				change.Snapshot.OwnedTags = nil
			}
			for i := range candidate.Bindings {
				candidate.Bindings[i].ResultOperationUUID = normalizeRef(candidate.Bindings[i].ResultOperationUUID)
			}
			for i := range candidate.History {
				candidate.History[i].ResultOperationUUID = normalizeRef(candidate.History[i].ResultOperationUUID)
				candidate.History[i].Value.OperationUUID = normalizeRef(candidate.History[i].Value.OperationUUID)
			}
			sort.Slice(candidate.Objects, func(i, j int) bool { return candidate.Objects[i].ID < candidate.Objects[j].ID })
			sort.Slice(candidate.Links, func(i, j int) bool {
				a, b := candidate.Links[i], candidate.Links[j]
				if a.VersionID != b.VersionID {
					return a.VersionID < b.VersionID
				}
				return a.ArtifactID < b.ArtifactID
			})
			sort.Slice(candidate.Bindings, func(i, j int) bool {
				return candidate.Bindings[i].Value.VersionID < candidate.Bindings[j].Value.VersionID
			})
			sort.Slice(candidate.History, func(i, j int) bool {
				a, b := candidate.History[i].Value, candidate.History[j].Value
				if a.VersionID != b.VersionID {
					return a.VersionID < b.VersionID
				}
				return a.Generation < b.Generation
			})
			value.Candidate = candidate
		}
		intents = append(intents, value)
	}
	sort.Slice(intents, func(i, j int) bool {
		if intents[i].ResourceID != intents[j].ResourceID {
			return intents[i].ResourceID < intents[j].ResourceID
		}
		return intents[i].Kind < intents[j].Kind
	})
	data, err := json.Marshal(struct {
		Root  string
		Nodes []intent
	}{names[root.OperationUUID], intents})
	if err != nil {
		return "", err
	}
	return payloadHash(string(data)), nil
}

type graphDependencies struct {
	ids          map[int32]bool
	revisions    map[int32]int64
	bindings     map[int32]int64
	reserve      map[int32]bool
	aliasRepairs []*AliasRepairPlan
}

func operationDependencies(nodes []Operation) (graphDependencies, error) {
	deps := graphDependencies{ids: map[int32]bool{}, revisions: map[int32]int64{}, bindings: map[int32]int64{}, reserve: map[int32]bool{}}
	set := func(target map[int32]int64, id int32, revision int64) error {
		if id <= 0 || revision < 0 {
			return fmt.Errorf("invalid graph dependency %d/%d: %w", id, revision, api.ErrBadRequest)
		}
		if prior, ok := target[id]; ok && prior != revision {
			return fmt.Errorf("inconsistent graph revision for %d: %w", id, api.ErrConflict)
		}
		target[id] = revision
		deps.ids[id] = true
		deps.reserve[id] = true
		return nil
	}
	for _, op := range nodes {
		if err := set(deps.revisions, op.ResourceID, op.ExpectedRevision); err != nil {
			return deps, err
		}
		candidate, err := decodeMutationCandidate(op.CandidateJSON)
		if err != nil {
			return deps, err
		}
		if op.Kind == OperationKindAliasRepair && (candidate == nil || candidate.AliasRepair == nil) {
			return deps, fmt.Errorf("alias repair has no typed work order: %w", api.ErrBadRequest)
		}
		for _, id := range candidateResourceIDs(op.ResourceID, candidate) {
			deps.ids[id] = true
		}
		if op.ExecutionPlanJSON != nil {
			var plan nativeExecutionPlan
			if err := json.Unmarshal([]byte(*op.ExecutionPlanJSON), &plan); err != nil {
				return deps, err
			}
			if plan.ProjectionFrozen {
				for id, revision := range plan.ReadRevisions {
					if err := set(deps.revisions, id, revision); err != nil {
						return deps, err
					}
				}
				for id, revision := range plan.BindingReadRevisions {
					if err := set(deps.bindings, id, revision); err != nil {
						return deps, err
					}
				}
			}
		}
		if candidate == nil {
			continue
		}
		if candidate.AliasRepair != nil {
			if op.Kind != OperationKindAliasRepair || op.ResourceID != candidate.AliasRepair.VersionID || op.Generation != candidate.AliasRepair.Desired.Generation {
				return deps, fmt.Errorf("alias repair operation disagrees with its work order: %w", api.ErrConflict)
			}
			deps.aliasRepairs = append(deps.aliasRepairs, candidate.AliasRepair)
		}
		if candidate.NativeWork != nil {
			deps.reserve[candidate.NativeWork.LogicalVersionID] = true
		}
		for _, change := range candidate.Objects {
			if err := set(deps.revisions, change.ID, change.ExpectedRevision); err != nil {
				return deps, err
			}
		}
		for id, revision := range candidate.ReadRevisions {
			if err := set(deps.revisions, id, revision); err != nil {
				return deps, err
			}
		}
		for id, revision := range candidate.BindingReadRevisions {
			if err := set(deps.bindings, id, revision); err != nil {
				return deps, err
			}
		}
		for _, change := range candidate.Bindings {
			if err := set(deps.bindings, change.Value.VersionID, change.ExpectedRevision); err != nil {
				return deps, err
			}
			if change.Value.PrimaryArtifactID != nil {
				deps.ids[*change.Value.PrimaryArtifactID] = true
				deps.reserve[*change.Value.PrimaryArtifactID] = true
			}
		}
		for _, link := range candidate.Links {
			deps.reserve[link.ArtifactID] = true
			deps.reserve[link.VersionID] = true
		}
		for _, history := range candidate.History {
			deps.reserve[history.Value.VersionID] = true
		}
	}
	return deps, nil
}

func sortedResourceIDs(ids map[int32]bool) []int32 {
	result := make([]int32, 0, len(ids))
	for id := range ids {
		result = append(result, id)
	}
	slices.Sort(result)
	return result
}

func lockGraphObjects(tx *gorm.DB, deps graphDependencies) (map[int32]Object, error) {
	objects := make(map[int32]Object, len(deps.ids))
	for _, id := range sortedResourceIDs(deps.ids) {
		object, err := lockResource(tx, id)
		if err != nil {
			return nil, err
		}
		objects[id] = object
	}
	for id, revision := range deps.revisions {
		if objects[id].Revision != revision {
			return nil, fmt.Errorf("graph resource %d is at revision %d, not %d: %w", id, objects[id].Revision, revision, api.ErrConflict)
		}
	}
	if err := verifyGraphBindings(tx, deps.bindings); err != nil {
		return nil, err
	}
	for _, plan := range deps.aliasRepairs {
		if err := verifyAliasRepairDependencies(tx, plan, objects); err != nil {
			return nil, err
		}
	}
	return objects, nil
}

func verifyGraphBindings(tx *gorm.DB, revisions map[int32]int64) error {
	ids := map[int32]bool{}
	for id := range revisions {
		ids[id] = true
	}
	for _, id := range sortedResourceIDs(ids) {
		var binding VersionBinding
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("version_id = ?", id).First(&binding).Error
		if err == gorm.ErrRecordNotFound && revisions[id] == 0 {
			continue
		}
		if err != nil {
			return mapDBError(err)
		}
		if binding.Revision != revisions[id] {
			return fmt.Errorf("graph binding %d is at revision %d, not %d: %w", id, binding.Revision, revisions[id], api.ErrConflict)
		}
	}
	return nil
}

// PrepareOperationGraph reserves and inserts the complete work graph in one
// short SQL transaction. No native calls occur while these locks are held.
func (s *Store) PrepareOperationGraph(ctx context.Context, root Operation, children []Operation) (Operation, []Operation, error) {
	members := map[string]bool{root.OperationUUID: true}
	for _, child := range children {
		members[child.OperationUUID] = true
	}
	for _, node := range append([]Operation{root}, children...) {
		candidate, err := decodeMutationCandidate(node.CandidateJSON)
		if err != nil {
			return Operation{}, nil, err
		}
		if candidate == nil || candidate.CodecVersion < 3 {
			continue
		}
		for _, binding := range candidate.Bindings {
			if binding.ResultOperationUUID != "" && !members[binding.ResultOperationUUID] {
				return Operation{}, nil, fmt.Errorf("result operation %s is outside the prepared graph: %w", binding.ResultOperationUUID, api.ErrConflict)
			}
		}
		for _, history := range candidate.History {
			if history.ResultOperationUUID != "" && !members[history.ResultOperationUUID] {
				return Operation{}, nil, fmt.Errorf("result operation %s is outside the prepared graph: %w", history.ResultOperationUUID, api.ErrConflict)
			}
		}
	}
	hash, err := semanticGraphHash(root, children)
	if err != nil {
		return Operation{}, nil, err
	}
	nodes := append([]Operation{root}, children...)
	deps, err := operationDependencies(nodes)
	if err != nil {
		return Operation{}, nil, err
	}
	var persisted Operation
	var persistedChildren []Operation
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		objects, err := lockGraphObjects(tx, deps)
		if err != nil {
			return err
		}
		var reservations []IntentResource
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("resource_id IN ?", sortedResourceIDs(deps.reserve)).Find(&reservations).Error; err != nil {
			return mapDBError(err)
		}
		rootID := root.OperationUUID
		root.AggregateOperationUUID = nil
		retained := map[int32]bool{}
		// An explicit operator retry retains the original aggregate reservation.
		if root.ParentOperationUUID != nil {
			var parent Operation
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("operation_uuid = ?", *root.ParentOperationUUID).First(&parent).Error; err != nil {
				return mapDBError(err)
			}
			if parent.State != OperationFailed || parent.SupersededByOperationUUID != nil || parent.ResourceID != root.ResourceID || parent.ExpectedRevision != root.ExpectedRevision {
				return fmt.Errorf("retry parent is not superseded and current: %w", api.ErrConflict)
			}
			rootID = graphRoot(parent)
			root.AggregateOperationUUID = new(rootID)
		}
		for _, reservation := range reservations {
			if root.ParentOperationUUID != nil && reservation.RootOperationUUID == rootID {
				retained[reservation.ResourceID] = true
				continue
			}
			var existing Operation
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("operation_uuid = ?", reservation.RootOperationUUID).First(&existing).Error; err != nil {
				return mapDBError(err)
			}
			selected, err := resolveAttempt(tx, existing.OperationUUID)
			if err != nil {
				return err
			}
			if existing.RequestHash != hash || selected.State == OperationSucceeded || selected.State == OperationFailed {
				return fmt.Errorf("resource %d is reserved by graph %s: %w", reservation.ResourceID, reservation.RootOperationUUID, api.ErrConflict)
			}
			// Join the complete recorded graph, returning its actual UUIDs.
			persisted = selected
			if err := tx.Where("aggregate_operation_uuid = ? AND parent_operation_uuid IS NULL", existing.OperationUUID).Order("resource_id, operation_uuid").Find(&persistedChildren).Error; err != nil {
				return mapDBError(err)
			}
			return nil
		}
		// Unhydrated legacy work also blocks preparation. Startup hydration installs
		// all reservations, but never admit conflicting work if called before it.
		var active []Operation
		if err := tx.Where("resource_id IN ? AND state IN ?", sortedResourceIDs(deps.reserve), []OperationState{OperationPending, OperationRunning}).Find(&active).Error; err != nil {
			return mapDBError(err)
		}
		for _, existing := range active {
			if existing.ExpectedRevision != objects[existing.ResourceID].Revision {
				continue
			}
			return fmt.Errorf("resource %d has unreserved operation %s: %w", existing.ResourceID, existing.OperationUUID, api.ErrConflict)
		}
		insert := func(op *Operation) error {
			var sequence int64
			if err := tx.Model(&Operation{}).Select("COALESCE(MAX(request_sequence), 0)").Where("resource_id = ? AND expected_revision = ? AND kind = ?", op.ResourceID, op.ExpectedRevision, op.Kind).Scan(&sequence).Error; err != nil {
				return mapDBError(err)
			}
			op.RequestHash = hash
			op.RequestSequence = sequence + 1
			op.State = OperationPending
			op.Step = StepPrepared
			op.FencingToken = 0
			op.Attempts = 0
			op.CreateTimeSinceEpoch = nowMillis()
			op.LastUpdateTimeSinceEpoch = op.CreateTimeSinceEpoch
			return mapDBError(tx.Create(op).Error)
		}
		if err := insert(&root); err != nil {
			return err
		}
		for i := range children {
			children[i].AggregateOperationUUID = new(rootID)
			if err := insert(&children[i]); err != nil {
				return err
			}
		}
		for _, id := range sortedResourceIDs(deps.reserve) {
			if retained[id] {
				continue
			}
			if err := tx.Create(&IntentResource{ResourceID: id, RootOperationUUID: rootID, ExpectedRevision: objects[id].Revision}).Error; err != nil {
				return mapDBError(err)
			}
		}
		if root.ParentOperationUUID != nil {
			if err := tx.Model(&Operation{}).Where("operation_uuid = ?", *root.ParentOperationUUID).Update("superseded_by_operation_uuid", root.OperationUUID).Error; err != nil {
				return mapDBError(err)
			}
		}
		persisted = root
		persistedChildren = children
		return nil
	})
	if err != nil {
		return Operation{}, nil, fmt.Errorf("preparing operation graph %s: %w", root.OperationUUID, err)
	}
	return persisted, persistedChildren, nil
}

// lockOperationGraph locks all consumed objects in global order before any
// journal row, and validates durable ownership and every guarded revision.
func lockOperationGraph(tx *gorm.DB, operationID string) (Operation, map[int32]Object, error) {
	var operation Operation
	if err := tx.Where("operation_uuid = ?", operationID).First(&operation).Error; err != nil {
		return operation, nil, mapDBError(err)
	}
	rootID := graphRoot(operation)
	var nodes []Operation
	if err := tx.Where("operation_uuid = ? OR aggregate_operation_uuid = ?", rootID, rootID).Find(&nodes).Error; err != nil {
		return operation, nil, mapDBError(err)
	}
	deps, err := operationDependencies(nodes)
	if err != nil {
		return operation, nil, err
	}
	var reservations []IntentResource
	if err := tx.Where("root_operation_uuid = ?", rootID).Find(&reservations).Error; err != nil {
		return operation, nil, mapDBError(err)
	}
	for _, r := range reservations {
		deps.ids[r.ResourceID] = true
		deps.revisions[r.ResourceID] = r.ExpectedRevision
	}
	objects, err := lockGraphObjects(tx, deps)
	if err != nil {
		return operation, nil, err
	}
	// Locking reads avoid MySQL's transaction snapshot hiding a just-committed
	// reservation or lease after waiting on the resource locks.
	var current []IntentResource
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("resource_id IN ?", sortedResourceIDs(deps.reserve)).Find(&current).Error; err != nil {
		return operation, nil, mapDBError(err)
	}
	owned := map[int32]bool{}
	for _, r := range current {
		if r.RootOperationUUID != rootID || r.ExpectedRevision != objects[r.ResourceID].Revision {
			return operation, nil, fmt.Errorf("graph %s lost resource %d: %w", rootID, r.ResourceID, api.ErrConflict)
		}
		owned[r.ResourceID] = true
	}
	for id := range deps.reserve {
		if !owned[id] {
			return operation, nil, fmt.Errorf("graph %s has no reservation for %d: %w", rootID, id, api.ErrConflict)
		}
	}
	root, err := lockOperationForPublication(tx, rootID)
	if err != nil {
		return operation, nil, err
	}
	if (root.State == OperationSucceeded || root.State == OperationFailed) && root.SupersededByOperationUUID == nil {
		return operation, nil, fmt.Errorf("aggregate %s is terminal: %w", rootID, api.ErrConflict)
	}
	if root.FailureCode != nil && *root.FailureCode == "LEGACY_GRAPH_QUARANTINED" {
		return operation, nil, fmt.Errorf("aggregate %s is quarantined: %w", rootID, api.ErrConflict)
	}
	locked, err := lockOperationForPublication(tx, operationID)
	if err != nil {
		return operation, nil, err
	}
	if locked.SupersededByOperationUUID != nil || (locked.FailureCode != nil && *locked.FailureCode == "LEGACY_GRAPH_QUARANTINED") {
		return operation, nil, fmt.Errorf("operation %s cannot dispatch: %w", operationID, api.ErrConflict)
	}
	return locked, objects, nil
}

// legacyNativePlan preserves a recorded child work order in the execution-plan
// column. It never changes the old candidate or its payload hash.
func legacyNativePlan(op Operation) (*NativeVersionPlan, error) {
	c, err := decodeMutationCandidate(op.CandidateJSON)
	if err != nil {
		return nil, err
	}
	if c != nil && c.NativeWork != nil {
		return c.NativeWork, nil
	}
	if op.ExecutionPlanJSON != nil {
		var plan NativeVersionPlan
		if err := json.Unmarshal([]byte(*op.ExecutionPlanJSON), &plan); err != nil {
			return nil, err
		}
		return &plan, nil
	}
	if op.Kind != OperationKindVersionReplaceChild {
		return nil, nil
	}
	if c == nil || len(c.Objects) != 1 {
		return nil, fmt.Errorf("child %s has no recorded work order: %w", op.OperationUUID, api.ErrConflict)
	}
	var payload replaceChildPayload
	if err := json.Unmarshal(c.Objects[0].Snapshot.Resource, &payload); err != nil {
		return nil, err
	}
	if payload.ModelName == "" || payload.Source == "" {
		return nil, fmt.Errorf("child %s has incomplete recorded work: %w", op.OperationUUID, api.ErrConflict)
	}
	return &NativeVersionPlan{LogicalVersionID: op.ResourceID, ModelName: payload.ModelName, Source: payload.Source, Generation: op.Generation, OwnedTags: map[string]string{tagLogicalID: strconv.Itoa(int(op.ResourceID)), tagOperation: op.OperationUUID}}, nil
}

// captureCandidateReads freezes the revisions of projection inputs before the
// immutable payload is serialized. Preparation checks them under ordered locks.
func (s *Store) captureCandidateReads(ctx context.Context, candidate *MutationCandidate) error {
	if candidate.ReadRevisions == nil {
		candidate.ReadRevisions = map[int32]int64{}
	}
	if candidate.BindingReadRevisions == nil {
		candidate.BindingReadRevisions = map[int32]int64{}
	}
	versions := map[int32]bool{}
	for _, link := range candidate.Links {
		versions[link.VersionID] = true
	}
	for _, binding := range candidate.Bindings {
		versions[binding.Value.VersionID] = true
	}
	for _, change := range candidate.Objects {
		if change.Snapshot.Kind == KindModelVersion {
			versions[change.ID] = true
		}
		if change.Snapshot.Kind == KindModelArtifact {
			var links []ArtifactLink
			if err := s.db.WithContext(ctx).Where("artifact_id = ?", change.ID).Find(&links).Error; err != nil {
				return mapDBError(err)
			}
			for _, link := range links {
				versions[link.VersionID] = true
			}
		}
		candidate.ReadRevisions[change.ID] = change.ExpectedRevision
	}
	for _, id := range candidateResourceIDs(0, candidate) {
		if id == 0 {
			continue
		}
		if _, ok := candidate.ReadRevisions[id]; ok {
			continue
		}
		object, err := s.GetObject(ctx, id)
		if err != nil {
			return err
		}
		if object.Kind != KindRegisteredModel {
			candidate.ReadRevisions[id] = object.Revision
		}
	}
	for _, id := range sortedResourceIDs(versions) {
		if _, captured := candidate.ReadRevisions[id]; !captured {
			var version Object
			if err := s.db.WithContext(ctx).Where("id = ?", id).First(&version).Error; err != nil {
				return mapDBError(err)
			}
			candidate.ReadRevisions[id] = version.Revision
		}
		var links []ArtifactLink
		if err := s.db.WithContext(ctx).Where("version_id = ?", id).Find(&links).Error; err != nil {
			return mapDBError(err)
		}
		for _, link := range links {
			if _, ok := candidate.ReadRevisions[link.ArtifactID]; !ok {
				artifact, err := s.GetObject(ctx, link.ArtifactID)
				if err != nil {
					return err
				}
				candidate.ReadRevisions[link.ArtifactID] = artifact.Revision
			}
		}
		var binding VersionBinding
		err := s.db.WithContext(ctx).Where("version_id = ?", id).First(&binding).Error
		if err != nil && err != gorm.ErrRecordNotFound {
			return mapDBError(err)
		}
		if _, captured := candidate.BindingReadRevisions[id]; !captured {
			candidate.BindingReadRevisions[id] = binding.Revision
		}
	}
	return nil
}

// HydrateLegacyOperationGraph reconstructs only edges present in immutable
// parent candidates. Uncertain or multiply referenced work is quarantined with
// all dispatch evidence intact; no native calls or inferred lost parents occur.
func (s *Store) HydrateLegacyOperationGraph(ctx context.Context) error {
	return s.db.WithContext(ctx).Transaction(hydrateLegacyOperationGraph)
}

func hydrateLegacyOperationGraph(tx *gorm.DB) error {
	var operations []Operation
	if err := tx.Order("operation_uuid").Find(&operations).Error; err != nil {
		return mapDBError(err)
	}
	byID := map[string]Operation{}
	references := map[string]map[string]bool{}
	for _, op := range operations {
		byID[op.OperationUUID] = op
		candidate, err := decodeMutationCandidate(op.CandidateJSON)
		if err != nil {
			return err
		}
		if candidate == nil {
			continue
		}
		add := func(id string) {
			if id != "" && id != op.OperationUUID {
				if references[id] == nil {
					references[id] = map[string]bool{}
				}
				references[id][op.OperationUUID] = true
			}
		}
		for _, binding := range candidate.Bindings {
			add(binding.ResultOperationUUID)
		}
		for _, history := range candidate.History {
			add(history.ResultOperationUUID)
		}
	}
	// Startup uses the same global object-before-journal order as mutations.
	// Only hydrate old rows; contemporary rows already have complete reservations.
	var legacy []Operation
	for _, op := range operations {
		if op.FailureCode != nil && *op.FailureCode == DispositionOrphanRetained {
			continue
		}
		completedRetry := op.Kind == OperationKindVersionCreateRetry && op.State == OperationSucceeded && op.ParentOperationUUID != nil
		if op.RequestHash == "" && (op.State == OperationPending || op.State == OperationRunning || op.Kind == OperationKindVersionReplaceChild || completedRetry) {
			legacy = append(legacy, op)
		}
	}
	if len(legacy) == 0 {
		return nil
	}
	var objects []Object
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Order("id ASC").Find(&objects).Error; err != nil {
		return mapDBError(err)
	}
	quarantine := func(id string) error {
		return mapDBError(tx.Model(&Operation{}).Where("operation_uuid = ?", id).Update("failure_code", "LEGACY_GRAPH_QUARANTINED").Error)
	}
	// Completed retries still explain late native arrivals from their failed
	// ancestors. Reconstruct only proven journal edges/hashes: published objects
	// may already be newer and must never be reserved or republished here.
	for _, op := range legacy {
		if op.Kind != OperationKindVersionCreateRetry || op.State != OperationSucceeded || op.ParentOperationUUID == nil {
			continue
		}
		chain, hash, err := legacyRetryChain(op, byID)
		if err != nil {
			if err := quarantine(op.OperationUUID); err != nil {
				return err
			}
			continue
		}
		if err := recordLegacyRetryChain(tx, chain, byID); err != nil {
			return err
		}
		for _, attempt := range chain {
			if err := tx.Model(&Operation{}).Where("operation_uuid = ?", attempt.OperationUUID).Update("request_hash", hash).Error; err != nil {
				return mapDBError(err)
			}
			current := byID[attempt.OperationUUID]
			current.RequestHash = hash
			byID[attempt.OperationUUID] = current
		}
	}
	for child, parents := range references {
		if _, exists := byID[child]; !exists {
			for parent := range parents {
				if err := quarantine(parent); err != nil {
					return err
				}
			}
		}
	}
	for _, op := range legacy {
		if op.Kind != OperationKindVersionReplaceChild {
			continue
		}
		parents := references[op.OperationUUID]
		if len(parents) == 0 && (op.Step == StepPrepared || op.Step == StepClaimed) && op.NativeResult == nil {
			if err := tx.Model(&Operation{}).Where("operation_uuid = ?", op.OperationUUID).Updates(map[string]any{"state": OperationFailed, "failure_code": "LEGACY_ORPHAN_BEFORE_DISPATCH", "lease_owner": nil, "lease_expires_since_epoch": nil}).Error; err != nil {
				return mapDBError(err)
			}
			op.State = OperationFailed
			byID[op.OperationUUID] = op
			continue
		}
		if len(parents) != 1 {
			if err := quarantine(op.OperationUUID); err != nil {
				return err
			}
			// A dispatched orphan still owns its known resource. Quarantine is
			// a dispatch stop, never permission for a conflicting mutation.
			var reservation IntentResource
			err := tx.Where("resource_id = ?", op.ResourceID).First(&reservation).Error
			if err == gorm.ErrRecordNotFound {
				if err := tx.Create(&IntentResource{ResourceID: op.ResourceID, RootOperationUUID: op.OperationUUID, ExpectedRevision: op.ExpectedRevision}).Error; err != nil {
					return mapDBError(err)
				}
			} else if err != nil {
				return mapDBError(err)
			} else if reservation.RootOperationUUID != op.OperationUUID {
				if err := quarantine(reservation.RootOperationUUID); err != nil {
					return err
				}
			}

			for parent := range parents {
				if err := quarantine(parent); err != nil {
					return err
				}
			}
			continue
		}
		for parent := range parents {
			if _, ok := byID[parent]; !ok {
				return fmt.Errorf("recorded graph parent %s missing: %w", parent, api.ErrConflict)
			}
			op.AggregateOperationUUID = new(parent)
			if err := tx.Model(&Operation{}).Where("operation_uuid = ?", op.OperationUUID).Update("aggregate_operation_uuid", parent).Error; err != nil {
				return mapDBError(err)
			}
		}
		plan, err := legacyNativePlan(op)
		if err != nil {
			if err := quarantine(op.OperationUUID); err != nil {
				return err
			}
			continue
		}
		if plan != nil {
			var adapter Adapter
			if err := tx.First(&adapter).Error; err == nil {
				plan.OwnedTags[tagAdapterUUID] = adapter.AdapterUUID
			} else if err != gorm.ErrRecordNotFound {
				return mapDBError(err)
			}
			encoded, err := json.Marshal(plan)
			if err != nil {
				return err
			}
			op.ExecutionPlanJSON = new(string(encoded))
			if err := tx.Model(&Operation{}).Where("operation_uuid = ?", op.OperationUUID).Update("execution_plan_json", string(encoded)).Error; err != nil {
				return mapDBError(err)
			}
		}
		byID[op.OperationUUID] = op
	}
	for _, old := range legacy {
		root := byID[old.OperationUUID]
		if root.AggregateOperationUUID != nil || root.State == OperationFailed || root.State == OperationSucceeded || root.Kind == OperationKindVersionReplaceChild {
			continue
		}
		children := []Operation{}
		for _, op := range byID {
			if op.AggregateOperationUUID != nil && *op.AggregateOperationUUID == root.OperationUUID {
				children = append(children, op)
			}
		}
		var retryChain []Operation
		var retryHash string
		// A retry is an attempt in the original aggregate, distinct from a child.
		if root.ParentOperationUUID != nil {
			var err error
			retryChain, retryHash, err = legacyRetryChain(root, byID)
			if err != nil {
				if err := quarantine(root.OperationUUID); err != nil {
					return err
				}
				continue
			}
			if err := recordLegacyRetryChain(tx, retryChain, byID); err != nil {
				return err
			}
			root = byID[root.OperationUUID]
		}
		deps, err := operationDependencies(append([]Operation{root}, children...))
		if err != nil {
			return err
		}
		locked, err := lockGraphObjects(tx, deps)
		if err != nil {
			if !errors.Is(err, api.ErrConflict) && !errors.Is(err, api.ErrNotFound) {
				return err
			}
			if err := quarantine(root.OperationUUID); err != nil {
				return err
			}
			known := map[int32]Object{}
			for _, object := range objects {
				known[object.ID] = object
			}
			for _, id := range sortedResourceIDs(deps.reserve) {
				object, exists := known[id]
				if !exists {
					continue
				}
				revision, explicit := deps.revisions[id]
				if !explicit {
					revision = object.Revision
				}
				var reservation IntentResource
				err := tx.Where("resource_id = ?", id).First(&reservation).Error
				if err == gorm.ErrRecordNotFound {
					if err := tx.Create(&IntentResource{ResourceID: id, RootOperationUUID: graphRoot(root), ExpectedRevision: revision}).Error; err != nil {
						return mapDBError(err)
					}
				} else if err != nil {
					return mapDBError(err)
				} else if reservation.RootOperationUUID != graphRoot(root) {
					if err := quarantine(reservation.RootOperationUUID); err != nil {
						return err
					}
				}
			}
			continue
		}
		hash, err := semanticGraphHash(root, children)
		if err != nil {
			return err
		}
		if retryHash != "" {
			hash = retryHash
		}
		for _, op := range append(append([]Operation{root}, children...), retryChain...) {
			if err := tx.Model(&Operation{}).Where("operation_uuid = ?", op.OperationUUID).Update("request_hash", hash).Error; err != nil {
				return mapDBError(err)
			}
			current := byID[op.OperationUUID]
			current.RequestHash = hash
			byID[op.OperationUUID] = current
		}
		for _, id := range sortedResourceIDs(deps.reserve) {
			var existing IntentResource
			err := tx.Where("resource_id = ?", id).First(&existing).Error
			if err == nil {
				if existing.RootOperationUUID != graphRoot(root) {
					if err := quarantine(root.OperationUUID); err != nil {
						return err
					}
					if err := quarantine(existing.RootOperationUUID); err != nil {
						return err
					}
				}
				continue
			}
			if err != gorm.ErrRecordNotFound {
				return mapDBError(err)
			}
			if err := tx.Create(&IntentResource{ResourceID: id, RootOperationUUID: graphRoot(root), ExpectedRevision: locked[id].Revision}).Error; err != nil {
				return mapDBError(err)
			}
		}
	}
	return nil
}

func recordLegacyRetryChain(tx *gorm.DB, chain []Operation, operations map[string]Operation) error {
	ancestor := chain[0].OperationUUID
	for i, attempt := range chain {
		updates := map[string]any{}
		if i > 0 {
			attempt.AggregateOperationUUID = new(ancestor)
			updates["aggregate_operation_uuid"] = ancestor
		}
		if i+1 < len(chain) {
			attempt.SupersededByOperationUUID = new(chain[i+1].OperationUUID)
			updates["superseded_by_operation_uuid"] = *attempt.SupersededByOperationUUID
		}
		if err := tx.Model(&Operation{}).Where("operation_uuid = ?", attempt.OperationUUID).Updates(updates).Error; err != nil {
			return mapDBError(err)
		}
		operations[attempt.OperationUUID] = attempt
	}
	return nil
}

// Legacy retries recorded only their parent edge. Prove that every edge
// preserves the original intent before hydrating a common aggregate/hash.
// Kind and generated result references normalize to the original request;
// candidates, payload hashes, checkpoints, and lease evidence stay untouched.
func legacyRetryChain(latest Operation, operations map[string]Operation) ([]Operation, string, error) {
	chain := []Operation{latest}
	seen := map[string]bool{latest.OperationUUID: true}
	for current := latest; current.ParentOperationUUID != nil; {
		parent, exists := operations[*current.ParentOperationUUID]
		if !exists || seen[parent.OperationUUID] || current.Kind != OperationKindVersionCreateRetry || !isVersionCreateKind(parent.Kind) || parent.Kind == OperationKindVersionReplaceChild || parent.State != OperationFailed || parent.FailureCode == nil || *parent.FailureCode != FailureCodeOperatorRetry || parent.ResourceID != current.ResourceID || parent.ExpectedRevision != current.ExpectedRevision || parent.Generation != current.Generation || (parent.SupersededByOperationUUID != nil && *parent.SupersededByOperationUUID != current.OperationUUID) {
			return nil, "", fmt.Errorf("legacy retry has no verified parent edge: %w", api.ErrConflict)
		}
		seen[parent.OperationUUID] = true
		chain = append(chain, parent)
		current = parent
	}
	slices.Reverse(chain)
	original := chain[0]
	if original.Kind != OperationKindVersionMaterialize && original.Kind != OperationKindVersionReplace {
		return nil, "", fmt.Errorf("legacy retry has no original create intent: %w", api.ErrConflict)
	}
	hash, err := semanticGraphHash(original, nil)
	if err != nil {
		return nil, "", err
	}
	for _, attempt := range chain {
		canonical := attempt
		canonical.Kind = original.Kind
		candidateHash, err := semanticGraphHash(canonical, nil)
		if err != nil {
			return nil, "", err
		}
		if candidateHash != hash || (attempt.RequestHash != "" && attempt.RequestHash != hash) {
			return nil, "", fmt.Errorf("legacy retry changes the original intent: %w", api.ErrConflict)
		}
	}
	return chain, hash, nil
}

// Only the root or an explicitly selected root retry can publish and release
// ownership. Superseding a child never makes that attempt an aggregate root.
func requireAggregatePublisher(tx *gorm.DB, operation Operation) error {
	if operation.Kind == OperationKindVersionReplaceChild {
		return fmt.Errorf("child %s cannot publish its aggregate: %w", operation.OperationUUID, api.ErrConflict)
	}
	current := graphRoot(operation)
	seen := map[string]bool{}
	for current != operation.OperationUUID {
		if seen[current] {
			return fmt.Errorf("cyclic aggregate retry chain: %w", api.ErrConflict)
		}
		seen[current] = true
		var parent Operation
		if err := tx.Where("operation_uuid = ?", current).First(&parent).Error; err != nil {
			return mapDBError(err)
		}
		if parent.SupersededByOperationUUID == nil {
			return fmt.Errorf("operation %s is not the selected aggregate publisher: %w", operation.OperationUUID, api.ErrConflict)
		}
		current = *parent.SupersededByOperationUUID
	}
	return nil
}

// Canonicalize JSON maps without converting integers through float64. Only
// top-level server presentation times are excluded; identically named custom
// properties and every metadata discriminator/value remain part of the request.
func canonicalDesiredResource(raw []byte) (json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	if resource, ok := value.(map[string]any); ok {
		delete(resource, "createTimeSinceEpoch")
		delete(resource, "lastUpdateTimeSinceEpoch")
	}
	encoded, err := json.Marshal(value)
	return encoded, err
}
