-- Creation scope is separate from the retained zero-to-many version links.
-- Every legacy artifact retains scope zero and its existing links. IDs, links, snapshots and journals are untouched.
ALTER TABLE `MLflowObject`
  ADD COLUMN `artifact_scope_id` INT NOT NULL DEFAULT 0;
ALTER TABLE `MLflowObject`
  DROP INDEX `uq_mlflow_object_kind_parent_name`,
  ADD CONSTRAINT `uq_mlflow_object_kind_parent_scope_name`
    UNIQUE (`kind`, `parent_id`, `artifact_scope_id`, `name`),
  ADD CONSTRAINT `ck_mlflow_object_artifact_scope`
    CHECK (`artifact_scope_id` BETWEEN 0 AND 999999999 AND
           (`kind` = 'model-artifact' OR `artifact_scope_id` = 0));
