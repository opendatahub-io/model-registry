-- Rebuild the old exact-name key first. MySQL's atomic ALTER refuses a
-- collapse before removing the scoped key/column; no row is deleted or merged.
ALTER TABLE `MLflowObject`
  ADD CONSTRAINT `uq_mlflow_object_kind_parent_name` UNIQUE (`kind`, `parent_id`, `name`),
  DROP INDEX `uq_mlflow_object_kind_parent_scope_name`,
  DROP CHECK `ck_mlflow_object_artifact_scope`,
  DROP COLUMN `artifact_scope_id`;
