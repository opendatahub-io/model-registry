ALTER TABLE "MLflowOperation" ALTER COLUMN "native_result" TYPE TEXT;
CREATE TABLE "MLflowNativeDisposition" (
 "native_model_name" VARCHAR(512) NOT NULL,
 "native_version_number" VARCHAR(64) NOT NULL,
 "adapter_uuid" VARCHAR(36) NOT NULL,
 "version_id" INT NOT NULL,
 "operation_uuid" VARCHAR(36) NOT NULL,
 "generation" BIGINT NOT NULL,
 "disposition" VARCHAR(32) NOT NULL,
 "resolving_operation_uuid" VARCHAR(36) NOT NULL,
 "resolved_time_since_epoch" BIGINT NOT NULL,
 PRIMARY KEY ("native_model_name", "native_version_number"),
 CONSTRAINT "ck_mlflow_native_disposition" CHECK ("disposition" IN ('UNSELECTED_DUPLICATE', 'ORPHAN_RETAINED'))
);
CREATE INDEX "idx_mlflow_native_disposition_operation" ON "MLflowNativeDisposition" ("operation_uuid");
