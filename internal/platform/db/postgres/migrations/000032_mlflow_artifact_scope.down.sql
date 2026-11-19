BEGIN;
-- Refuse a destructive collapse while the scoped schema is still intact.
ALTER TABLE "MLflowObject"
  ADD CONSTRAINT "uq_mlflow_object_kind_parent_name" UNIQUE ("kind", "parent_id", "name");
ALTER TABLE "MLflowObject"
  DROP CONSTRAINT "uq_mlflow_object_kind_parent_scope_name",
  DROP CONSTRAINT "ck_mlflow_object_artifact_scope",
  DROP COLUMN "artifact_scope_id";
COMMIT;
