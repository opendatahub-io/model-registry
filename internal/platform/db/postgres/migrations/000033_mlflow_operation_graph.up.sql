-- Operation graphs require an exclusive binary upgrade. Old binaries do not
-- understand graph reservations or codec 3 and must not share this store.
ALTER TABLE "MLflowOperation"
 ADD COLUMN "request_hash" VARCHAR(64) NOT NULL DEFAULT '',
 ADD COLUMN "request_sequence" BIGINT NOT NULL DEFAULT 0,
 ADD COLUMN "aggregate_operation_uuid" VARCHAR(36) NULL,
 ADD COLUMN "superseded_by_operation_uuid" VARCHAR(36) NULL,
 ADD COLUMN "execution_plan_json" TEXT NULL,
 DROP CONSTRAINT "uq_mlflow_operation_resource_revision_kind",
 ADD CONSTRAINT "uq_mlflow_operation_request" UNIQUE ("resource_id", "expected_revision", "kind", "request_sequence");
CREATE INDEX "idx_mlflow_operation_aggregate" ON "MLflowOperation" ("aggregate_operation_uuid");
CREATE TABLE "MLflowIntentResource" (
 "resource_id" INT NOT NULL PRIMARY KEY,
 "root_operation_uuid" VARCHAR(36) NOT NULL,
 "expected_revision" BIGINT NOT NULL,
 CONSTRAINT "fk_mlflow_intent_resource" FOREIGN KEY ("resource_id") REFERENCES "MLflowObject" ("id"),
 CONSTRAINT "fk_mlflow_intent_root" FOREIGN KEY ("root_operation_uuid") REFERENCES "MLflowOperation" ("operation_uuid")
);
CREATE INDEX "idx_mlflow_intent_root" ON "MLflowIntentResource" ("root_operation_uuid");
UPDATE "MLflowAdapter" SET "schema_version" = 2, "codec_version" = 3;
