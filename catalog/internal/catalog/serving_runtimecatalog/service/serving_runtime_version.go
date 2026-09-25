package service

import (
	"errors"
	"fmt"

	"github.com/kubeflow/hub/catalog/internal/catalog/serving_runtimecatalog/models"
	"github.com/kubeflow/hub/catalog/internal/db/pagination"
	"github.com/kubeflow/hub/internal/platform/db/dbutil"
	dbmodels "github.com/kubeflow/hub/internal/platform/db/entity"
	service "github.com/kubeflow/hub/internal/platform/db/repository"
	"github.com/kubeflow/hub/internal/platform/db/schema"
	"github.com/kubeflow/hub/internal/platform/db/scopes"
	"github.com/kubeflow/hub/internal/platform/db/utils"
	"gorm.io/gorm"
)

var ErrServingRuntimeVersionNotFound = errors.New("serving_runtime_version not found")

// ServingRuntimeVersionRepositoryImpl implements ServingRuntimeVersionRepository using GORM.
type ServingRuntimeVersionRepositoryImpl struct {
	*service.GenericRepository[models.ServingRuntimeVersion, schema.Artifact, schema.ArtifactProperty, *models.ServingRuntimeVersionListOptions]
}

// NewServingRuntimeVersionRepository creates a new ServingRuntimeVersionRepository.
func NewServingRuntimeVersionRepository(db *gorm.DB, typeID int32) models.ServingRuntimeVersionRepository {
	r := &ServingRuntimeVersionRepositoryImpl{}

	r.GenericRepository = service.NewGenericRepository(service.GenericRepositoryConfig[models.ServingRuntimeVersion, schema.Artifact, schema.ArtifactProperty, *models.ServingRuntimeVersionListOptions]{
		DB:                      db,
		TypeID:                  typeID,
		EntityToSchema:          mapServingRuntimeVersionToSchema,
		SchemaToEntity:          mapSchemaToServingRuntimeVersion,
		EntityToProperties:      mapServingRuntimeVersionToProperties,
		NotFoundError:           ErrServingRuntimeVersionNotFound,
		EntityName:              "serving_runtime_version",
		PropertyFieldName:       "artifact_id",
		ApplyListFilters:        applyServingRuntimeVersionListFilters,
		CreatePaginationToken:   r.createServingRuntimeVersionPaginationToken,
		ApplyCustomOrdering:     r.applyServingRuntimeVersionCustomOrdering,
		IsNewEntity:             func(entity models.ServingRuntimeVersion) bool { return entity.GetID() == nil },
		HasCustomProperties:     func(entity models.ServingRuntimeVersion) bool { return entity.GetCustomProperties() != nil },
		EntityMappingFuncs:      newServingRuntimeVersionEntityMappings(),
		PreserveHistoricalTimes: true,
		DeleteMissingProperties: true,
	})

	return r
}

// Save creates or updates a serving_runtime_version, ensuring the TypeID is set so
// the entity can later be found by List/Get queries.
func (r *ServingRuntimeVersionRepositoryImpl) Save(entity models.ServingRuntimeVersion, parentResourceID *int32) (models.ServingRuntimeVersion, error) {
	config := r.GetConfig()
	if entity.GetTypeID() == nil && config.TypeID > 0 {
		entity.SetTypeID(config.TypeID)
	}
	return r.GenericRepository.Save(entity, parentResourceID)
}

// List returns a paginated list of serving_runtime_versions.
func (r *ServingRuntimeVersionRepositoryImpl) List(listOptions *models.ServingRuntimeVersionListOptions) (*dbmodels.ListWrapper[models.ServingRuntimeVersion], error) {
	return r.GenericRepository.List(listOptions)
}

func mapServingRuntimeVersionToSchema(entity models.ServingRuntimeVersion) schema.Artifact {
	attrs := entity.GetAttributes()
	art := schema.Artifact{}
	if typeID := entity.GetTypeID(); typeID != nil {
		art.TypeID = *typeID
	}
	if entity.GetID() != nil {
		art.ID = *entity.GetID()
	}
	if attrs != nil {
		art.Name = attrs.Name
		art.ExternalID = attrs.ExternalID
		if attrs.CreateTimeSinceEpoch != nil {
			art.CreateTimeSinceEpoch = *attrs.CreateTimeSinceEpoch
		}
		if attrs.LastUpdateTimeSinceEpoch != nil {
			art.LastUpdateTimeSinceEpoch = *attrs.LastUpdateTimeSinceEpoch
		}
	}
	return art
}

func mapSchemaToServingRuntimeVersion(schemaEntity schema.Artifact, props []schema.ArtifactProperty) models.ServingRuntimeVersion {
	entity := &models.ServingRuntimeVersionImpl{
		ID:     &schemaEntity.ID,
		TypeID: &schemaEntity.TypeID,
		Attributes: &models.ServingRuntimeVersionAttributes{
			Name:                     schemaEntity.Name,
			ExternalID:               schemaEntity.ExternalID,
			CreateTimeSinceEpoch:     &schemaEntity.CreateTimeSinceEpoch,
			LastUpdateTimeSinceEpoch: &schemaEntity.LastUpdateTimeSinceEpoch,
		},
	}

	properties := []dbmodels.Properties{}
	customProperties := []dbmodels.Properties{}
	for _, prop := range props {
		mapped := service.MapArtifactPropertyToProperties(prop)
		if prop.IsCustomProperty {
			customProperties = append(customProperties, mapped)
		} else {
			properties = append(properties, mapped)
		}
	}
	entity.Properties = &properties
	entity.CustomProperties = &customProperties
	return entity
}

func mapServingRuntimeVersionToProperties(entity models.ServingRuntimeVersion, entityID int32) []schema.ArtifactProperty {
	var properties []schema.ArtifactProperty
	if entity.GetProperties() != nil {
		for _, prop := range *entity.GetProperties() {
			properties = append(properties, service.MapPropertiesToArtifactProperty(prop, entityID, false))
		}
	}
	if entity.GetCustomProperties() != nil {
		for _, prop := range *entity.GetCustomProperties() {
			properties = append(properties, service.MapPropertiesToArtifactProperty(prop, entityID, true))
		}
	}
	return properties
}

func applyServingRuntimeVersionListFilters(query *gorm.DB, listOptions *models.ServingRuntimeVersionListOptions) *gorm.DB {
	// Scope to versions attributed to a specific ServingRuntime parent.
	if listOptions.ParentResourceID != nil {
		query = query.Joins(utils.BuildAttributionJoin(query)).
			Where(utils.GetColumnRef(query, &schema.Attribution{}, "context_id")+" = ?", *listOptions.ParentResourceID).
			Select(utils.GetTableName(query, &schema.Artifact{}) + ".*")
	}

	// Filter by source_id when provided.
	if listOptions.SourceIDs != nil && len(*listOptions.SourceIDs) > 0 {
		propTable := utils.GetTableName(query, &schema.ArtifactProperty{})
		artifactTable := utils.GetTableName(query, &schema.Artifact{})
		subQuery := query.Session(&gorm.Session{NewDB: true}).
			Table(propTable).
			Select("artifact_id").
			Where("name = ? AND is_custom_property = ? AND string_value IN ?", "source_id", false, *listOptions.SourceIDs)
		query = query.Where(artifactTable+".id IN (?)", subQuery)
	}

	return query
}

func (r *ServingRuntimeVersionRepositoryImpl) createServingRuntimeVersionPaginationToken(lastItem schema.Artifact, listOptions *models.ServingRuntimeVersionListOptions) string {
	if listOptions.GetOrderBy() == "NAME" {
		return pagination.CreateNamePaginationToken(lastItem.ID, lastItem.Name)
	}
	return r.CreateDefaultPaginationToken(lastItem, listOptions)
}

// ServingRuntimeVersionOrderByColumns are the allowed orderBy columns for serving_runtime_versions.
var ServingRuntimeVersionOrderByColumns = map[string]string{
	"ID":               "id",
	"CREATE_TIME":      "create_time_since_epoch",
	"LAST_UPDATE_TIME": "last_update_time_since_epoch",
	"NAME":             "name",
	"id":               "id",
}

func (r *ServingRuntimeVersionRepositoryImpl) applyServingRuntimeVersionCustomOrdering(query *gorm.DB, listOptions *models.ServingRuntimeVersionListOptions) *gorm.DB {
	db := r.GetConfig().DB
	artifactTable := utils.GetTableName(db, &schema.Artifact{})
	orderBy := listOptions.GetOrderBy()

	if orderBy == "NAME" {
		return pagination.ApplyNameOrdering(query, artifactTable, listOptions.GetSortOrder(), listOptions.GetNextPageToken(), listOptions.GetPageSize(), false)
	}

	return r.ApplyStandardPagination(query, listOptions, []models.ServingRuntimeVersion{})
}

// ApplyStandardPagination overrides the base implementation to pass the OrderByColumns map.
func (r *ServingRuntimeVersionRepositoryImpl) ApplyStandardPagination(query *gorm.DB, listOptions *models.ServingRuntimeVersionListOptions, entities any) *gorm.DB {
	pageSize := listOptions.GetPageSize()
	orderBy := listOptions.GetOrderBy()
	sortOrder := listOptions.GetSortOrder()
	nextPageToken := listOptions.GetNextPageToken()

	pag := &dbmodels.Pagination{
		PageSize:      &pageSize,
		OrderBy:       &orderBy,
		SortOrder:     &sortOrder,
		NextPageToken: &nextPageToken,
	}

	return query.Scopes(scopes.PaginateWithOptions(entities, pag, r.GetConfig().DB, "Artifact", ServingRuntimeVersionOrderByColumns))
}

func (r *ServingRuntimeVersionRepositoryImpl) DeleteBySource(sourceID string) error {
	config := r.GetConfig()
	tableName := utils.GetTableName(config.DB, &schema.Artifact{})
	propTableName := utils.GetTableName(config.DB, &schema.ArtifactProperty{})

	subQuery := config.DB.Table(tableName).
		Select(tableName+".id").
		Joins("INNER JOIN "+propTableName+" ON "+
			tableName+".id = "+propTableName+".artifact_id").
		Where(propTableName+".name = ? AND "+
			propTableName+".is_custom_property = ? AND "+
			propTableName+".string_value = ? AND "+
			tableName+".type_id = ?",
			"source_id", false, sourceID, config.TypeID)

	return config.DB.Where("id IN (?)", subQuery).Delete(&schema.Artifact{}).Error
}

func (r *ServingRuntimeVersionRepositoryImpl) DeleteByParentID(parentID int32) error {
	config := r.GetConfig()
	attributionTable := utils.GetTableName(config.DB, &schema.Attribution{})
	artifactTable := utils.GetTableName(config.DB, &schema.Artifact{})
	subQuery := config.DB.Table(attributionTable).
		Select(attributionTable+".artifact_id").
		Joins(fmt.Sprintf("INNER JOIN %s ON %s.artifact_id = %s.id", artifactTable, attributionTable, artifactTable)).
		Where(fmt.Sprintf("%s.context_id = ? AND %s.type_id = ?", attributionTable, artifactTable), parentID, config.TypeID)
	return config.DB.Where("id IN (?)", subQuery).Delete(&schema.Artifact{}).Error
}

func (r *ServingRuntimeVersionRepositoryImpl) DeleteByID(id int32) error {
	config := r.GetConfig()
	result := config.DB.Where("id = ? AND type_id = ?", id, config.TypeID).Delete(&schema.Artifact{})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return fmt.Errorf("%w: id %d", config.NotFoundError, id)
	}
	return nil
}

func (r *ServingRuntimeVersionRepositoryImpl) GetDistinctSourceIDs() ([]string, error) {
	config := r.GetConfig()
	var sourceIDs []string

	propTableName := utils.GetTableName(config.DB, &schema.ArtifactProperty{})
	tableName := utils.GetTableName(config.DB, &schema.Artifact{})

	err := config.DB.Table(propTableName+" cp").
		Select("DISTINCT cp.string_value").
		Joins("INNER JOIN "+tableName+" a ON cp.artifact_id = a.id").
		Where("cp.name = ? AND cp.is_custom_property = ? AND a.type_id = ?", "source_id", false, config.TypeID).
		Pluck("string_value", &sourceIDs).Error

	if err != nil {
		err = dbutil.SanitizeDatabaseError(err)
		return nil, fmt.Errorf("error querying distinct source IDs: %w", err)
	}
	return sourceIDs, nil
}

func (r *ServingRuntimeVersionRepositoryImpl) GetTypeID() int32 {
	return r.GetConfig().TypeID
}
