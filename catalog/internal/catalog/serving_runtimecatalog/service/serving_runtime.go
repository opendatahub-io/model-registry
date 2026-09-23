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

var ErrServingRuntimeNotFound = errors.New("serving_runtime not found")

// ServingRuntimeRepositoryImpl implements ServingRuntimeRepository using GORM.
type ServingRuntimeRepositoryImpl struct {
	*service.GenericRepository[models.ServingRuntime, schema.Context, schema.ContextProperty, *models.ServingRuntimeListOptions]
}

// NewServingRuntimeRepository creates a new ServingRuntimeRepository.
func NewServingRuntimeRepository(db *gorm.DB, typeID int32) models.ServingRuntimeRepository {
	r := &ServingRuntimeRepositoryImpl{}

	r.GenericRepository = service.NewGenericRepository(service.GenericRepositoryConfig[models.ServingRuntime, schema.Context, schema.ContextProperty, *models.ServingRuntimeListOptions]{
		DB:                      db,
		TypeID:                  typeID,
		EntityToSchema:          mapServingRuntimeToSchema,
		SchemaToEntity:          mapSchemaToServingRuntime,
		EntityToProperties:      mapServingRuntimeToProperties,
		NotFoundError:           ErrServingRuntimeNotFound,
		EntityName:              "serving_runtime",
		PropertyFieldName:       "context_id",
		ApplyListFilters:        applyServingRuntimeListFilters,
		CreatePaginationToken:   r.createServingRuntimePaginationToken,
		ApplyCustomOrdering:     r.applyServingRuntimeCustomOrdering,
		IsNewEntity:             func(entity models.ServingRuntime) bool { return entity.GetID() == nil },
		HasCustomProperties:     func(entity models.ServingRuntime) bool { return entity.GetCustomProperties() != nil },
		EntityMappingFuncs:      newServingRuntimeEntityMappings(),
		PreserveHistoricalTimes: true,
		DeleteMissingProperties: true,
	})

	return r
}

// Save creates or updates a serving_runtime, ensuring the TypeID is set so the
// entity can later be found by List/Get queries.
func (r *ServingRuntimeRepositoryImpl) Save(entity models.ServingRuntime) (models.ServingRuntime, error) {
	config := r.GetConfig()
	if entity.GetTypeID() == nil && config.TypeID > 0 {
		entity.SetTypeID(config.TypeID)
	}
	return r.GenericRepository.Save(entity, nil)
}

// List returns a paginated list of serving_runtimes.
func (r *ServingRuntimeRepositoryImpl) List(listOptions *models.ServingRuntimeListOptions) (*dbmodels.ListWrapper[models.ServingRuntime], error) {
	return r.GenericRepository.List(listOptions)
}

func mapServingRuntimeToSchema(entity models.ServingRuntime) schema.Context {
	attrs := entity.GetAttributes()
	ctx := schema.Context{}
	if typeID := entity.GetTypeID(); typeID != nil {
		ctx.TypeID = *typeID
	}
	if entity.GetID() != nil {
		ctx.ID = *entity.GetID()
	}
	if attrs != nil {
		if attrs.Name != nil {
			ctx.Name = *attrs.Name
		}
		ctx.ExternalID = attrs.ExternalID
		if attrs.CreateTimeSinceEpoch != nil {
			ctx.CreateTimeSinceEpoch = *attrs.CreateTimeSinceEpoch
		}
		if attrs.LastUpdateTimeSinceEpoch != nil {
			ctx.LastUpdateTimeSinceEpoch = *attrs.LastUpdateTimeSinceEpoch
		}
	}
	return ctx
}

func mapSchemaToServingRuntime(schemaEntity schema.Context, props []schema.ContextProperty) models.ServingRuntime {
	entity := &models.ServingRuntimeImpl{
		ID:     &schemaEntity.ID,
		TypeID: &schemaEntity.TypeID,
		Attributes: &models.ServingRuntimeAttributes{
			Name:                     &schemaEntity.Name,
			ExternalID:               schemaEntity.ExternalID,
			CreateTimeSinceEpoch:     &schemaEntity.CreateTimeSinceEpoch,
			LastUpdateTimeSinceEpoch: &schemaEntity.LastUpdateTimeSinceEpoch,
		},
	}

	properties := []dbmodels.Properties{}
	customProperties := []dbmodels.Properties{}
	for _, prop := range props {
		mapped := service.MapContextPropertyToProperties(prop)
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

func mapServingRuntimeToProperties(entity models.ServingRuntime, entityID int32) []schema.ContextProperty {
	var properties []schema.ContextProperty
	if entity.GetProperties() != nil {
		for _, prop := range *entity.GetProperties() {
			properties = append(properties, service.MapPropertiesToContextProperty(prop, entityID, false))
		}
	}
	if entity.GetCustomProperties() != nil {
		for _, prop := range *entity.GetCustomProperties() {
			properties = append(properties, service.MapPropertiesToContextProperty(prop, entityID, true))
		}
	}
	return properties
}

func applyServingRuntimeListFilters(query *gorm.DB, listOptions *models.ServingRuntimeListOptions) *gorm.DB {
	// Filter by name (matched against the unqualified base_name property) when provided.
	if listOptions.Name != nil {
		contextTable := utils.GetTableName(query.Statement.DB, &schema.Context{})
		propertyTable := utils.GetTableName(query.Statement.DB, &schema.ContextProperty{})
		query = query.Where(
			fmt.Sprintf("EXISTS (SELECT 1 FROM %s cp WHERE cp.context_id = %s.id AND cp.name = 'base_name' AND cp.string_value LIKE ?)",
				propertyTable, contextTable),
			listOptions.Name,
		)
	}

	// Filter by source_id when provided.
	if listOptions.SourceIDs != nil && len(*listOptions.SourceIDs) > 0 {
		propTable := utils.GetTableName(query, &schema.ContextProperty{})
		contextTable := utils.GetTableName(query, &schema.Context{})
		subQuery := query.Session(&gorm.Session{NewDB: true}).
			Table(propTable).
			Select("context_id").
			Where("name = ? AND string_value IN ?", "source_id", *listOptions.SourceIDs)
		query = query.Where(contextTable+".id IN (?)", subQuery)
	}

	return query
}

func (r *ServingRuntimeRepositoryImpl) createServingRuntimePaginationToken(lastItem schema.Context, listOptions *models.ServingRuntimeListOptions) string {
	if listOptions.GetOrderBy() == "NAME" {
		return pagination.CreateNamePaginationToken(lastItem.ID, &lastItem.Name)
	}
	return r.CreateDefaultPaginationToken(lastItem, listOptions)
}

// ServingRuntimeOrderByColumns are the allowed orderBy columns for serving_runtimes.
var ServingRuntimeOrderByColumns = map[string]string{
	"ID":               "id",
	"CREATE_TIME":      "create_time_since_epoch",
	"LAST_UPDATE_TIME": "last_update_time_since_epoch",
	"NAME":             "name",
	"id":               "id",
}

func (r *ServingRuntimeRepositoryImpl) applyServingRuntimeCustomOrdering(query *gorm.DB, listOptions *models.ServingRuntimeListOptions) *gorm.DB {
	db := r.GetConfig().DB
	contextTable := utils.GetTableName(db, &schema.Context{})
	orderBy := listOptions.GetOrderBy()

	if orderBy == "NAME" {
		return pagination.ApplyNameOrdering(query, contextTable, listOptions.GetSortOrder(), listOptions.GetNextPageToken(), listOptions.GetPageSize(), false)
	}

	return r.ApplyStandardPagination(query, listOptions, []models.ServingRuntime{})
}

// ApplyStandardPagination overrides the base implementation to pass the OrderByColumns map.
func (r *ServingRuntimeRepositoryImpl) ApplyStandardPagination(query *gorm.DB, listOptions *models.ServingRuntimeListOptions, entities any) *gorm.DB {
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

	return query.Scopes(scopes.PaginateWithOptions(entities, pag, r.GetConfig().DB, "Context", ServingRuntimeOrderByColumns))
}

func (r *ServingRuntimeRepositoryImpl) DeleteBySource(sourceID string) error {
	config := r.GetConfig()
	tableName := utils.GetTableName(config.DB, &schema.Context{})
	propTableName := utils.GetTableName(config.DB, &schema.ContextProperty{})

	subQuery := config.DB.Table(tableName).
		Select(tableName+".id").
		Joins("INNER JOIN "+propTableName+" ON "+
			tableName+".id = "+propTableName+".context_id").
		Where(propTableName+".name = ? AND "+
			propTableName+".string_value = ? AND "+
			tableName+".type_id = ?",
			"source_id", sourceID, config.TypeID)

	return config.DB.Where("id IN (?)", subQuery).Delete(&schema.Context{}).Error
}

func (r *ServingRuntimeRepositoryImpl) DeleteByID(id int32) error {
	config := r.GetConfig()
	result := config.DB.Where("id = ? AND type_id = ?", id, config.TypeID).Delete(&schema.Context{})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return fmt.Errorf("%w: id %d", config.NotFoundError, id)
	}
	return nil
}

func (r *ServingRuntimeRepositoryImpl) GetDistinctSourceIDs() ([]string, error) {
	config := r.GetConfig()
	var sourceIDs []string

	propTableName := utils.GetTableName(config.DB, &schema.ContextProperty{})
	tableName := utils.GetTableName(config.DB, &schema.Context{})

	err := config.DB.Table(propTableName+" cp").
		Select("DISTINCT cp.string_value").
		Joins("INNER JOIN "+tableName+" c ON cp.context_id = c.id").
		Where("cp.name = ? AND c.type_id = ?", "source_id", config.TypeID).
		Pluck("string_value", &sourceIDs).Error

	if err != nil {
		err = dbutil.SanitizeDatabaseError(err)
		return nil, fmt.Errorf("error querying distinct source IDs: %w", err)
	}
	return sourceIDs, nil
}

func (r *ServingRuntimeRepositoryImpl) GetTypeID() int32 {
	return r.GetConfig().TypeID
}
