package db

const TableNameParentContext = "ParentContext"

// ParentContext mapped from table <ParentContext>
type ParentContext struct {
	ContextID       int64 `gorm:"column:context_id;primaryKey" json:"-"`
	ParentContextID int64 `gorm:"column:parent_context_id;primaryKey;index:idx_parentcontext_parent_context_id,priority:1" json:"-"`
}

// TableName ParentContext's table name
func (*ParentContext) TableName() string {
	return TableNameParentContext
}