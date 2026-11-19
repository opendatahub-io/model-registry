ALTER TABLE "MLflowObject"
    DROP CONSTRAINT "uq_mlflow_object_scope_external_id",
    DROP COLUMN "external_scope";
