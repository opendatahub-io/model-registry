-- Compatibility store for the MLflow model-registry backend.
--
-- These tables hold Kubeflow identities, pending entities, typed metadata,
-- artifact relationships, and the operation journal that cannot be
-- recovered from native MLflow tags alone. They coexist with the EmbedMD
-- tables in the same database; see docs/mlflow-backend.md.

-- Singleton adapter identity: binds this compatibility database to one
-- MLflow registry target and records codec/schema versions.
CREATE TABLE "MLflowAdapter" (
    "id" SERIAL PRIMARY KEY,
    "adapter_uuid" VARCHAR(36) NOT NULL,
    "target_fingerprint" VARCHAR(256) NOT NULL,
    "schema_version" INT NOT NULL,
    "codec_version" INT NOT NULL,
    "last_refresh_time" BIGINT NULL,
    CONSTRAINT "uq_mlflow_adapter_uuid" UNIQUE ("adapter_uuid")
);

-- One row per logical Kubeflow resource (registered model, model version,
-- model artifact). parent_id uses 0 (not NULL) for parentless resources so
-- the kind/parent/name uniqueness is enforceable on both engines.
CREATE TABLE "MLflowObject" (
    "id" SERIAL PRIMARY KEY,
    "kind" VARCHAR(32) NOT NULL,
    "parent_id" INT NOT NULL DEFAULT 0,
    "name" VARCHAR(512) NOT NULL,
    "external_id" VARCHAR(512) NULL,
    "create_time_since_epoch" BIGINT NOT NULL,
    "last_update_time_since_epoch" BIGINT NOT NULL,
    "committed_json" TEXT NOT NULL,
    "revision" BIGINT NOT NULL,
    "deleted" BOOLEAN NOT NULL DEFAULT FALSE,
    "native_model_name" VARCHAR(512) NULL,
    CONSTRAINT "uq_mlflow_object_kind_parent_name" UNIQUE ("kind", "parent_id", "name")
);

CREATE INDEX "idx_mlflow_object_parent_kind" ON "MLflowObject" ("parent_id", "kind");
CREATE INDEX "idx_mlflow_object_external_id" ON "MLflowObject" ("external_id");
CREATE INDEX "idx_mlflow_object_deleted" ON "MLflowObject" ("deleted");
CREATE INDEX "idx_mlflow_object_native_model_name" ON "MLflowObject" ("native_model_name");

-- Artifact-to-version relationships; one artifact may link to many
-- versions. The primary artifact selection lives in MLflowVersionBinding.
CREATE TABLE "MLflowArtifactLink" (
    "artifact_id" INT NOT NULL,
    "version_id" INT NOT NULL,
    PRIMARY KEY ("artifact_id", "version_id")
);

CREATE INDEX "idx_mlflow_artifact_link_version" ON "MLflowArtifactLink" ("version_id");

-- Materialization state of a logical model version. Native identity is
-- NULL while the version is pending; once bound it is unique.
CREATE TABLE "MLflowVersionBinding" (
    "id" SERIAL PRIMARY KEY,
    "version_id" INT NOT NULL,
    "model_id" INT NOT NULL,
    "primary_artifact_id" INT NULL,
    "phase" VARCHAR(32) NOT NULL,
    "native_model_name" VARCHAR(512) NULL,
    "native_version_number" VARCHAR(32) NULL,
    "active_generation" BIGINT NOT NULL,
    "desired_generation" BIGINT NOT NULL,
    "alias" VARCHAR(255) NOT NULL,
    "revision" BIGINT NOT NULL,
    CONSTRAINT "uq_mlflow_binding_version" UNIQUE ("version_id"),
    CONSTRAINT "uq_mlflow_binding_native" UNIQUE ("native_model_name", "native_version_number")
);

CREATE INDEX "idx_mlflow_binding_model" ON "MLflowVersionBinding" ("model_id");
CREATE INDEX "idx_mlflow_binding_phase" ON "MLflowVersionBinding" ("phase");

-- Immutable record of every native generation ever bound to a logical
-- version. Retained history prevents imports from re-creating superseded
-- replacements as new logical versions.
CREATE TABLE "MLflowVersionHistory" (
    "id" BIGSERIAL PRIMARY KEY,
    "version_id" INT NOT NULL,
    "generation" BIGINT NOT NULL,
    "native_model_name" VARCHAR(512) NOT NULL,
    "native_version_number" VARCHAR(32) NOT NULL,
    "source" TEXT NOT NULL,
    "operation_uuid" VARCHAR(36) NOT NULL,
    "superseded" BOOLEAN NOT NULL DEFAULT FALSE,
    "superseded_time" BIGINT NULL,
    CONSTRAINT "uq_mlflow_history_native" UNIQUE ("native_model_name", "native_version_number"),
    CONSTRAINT "uq_mlflow_history_version_generation" UNIQUE ("version_id", "generation")
);

-- Durable operation journal. The candidate payload is an immutable
-- snapshot separate from the committed object, so rejected native writes
-- never change visible state. Lease fields support multi-replica recovery
-- with fencing tokens.
CREATE TABLE "MLflowOperation" (
    "operation_uuid" VARCHAR(36) NOT NULL PRIMARY KEY,
    "resource_id" INT NOT NULL,
    "kind" VARCHAR(64) NOT NULL,
    "expected_revision" BIGINT NOT NULL,
    "candidate_json" TEXT NOT NULL,
    "payload_hash" VARCHAR(64) NOT NULL,
    "generation" BIGINT NOT NULL,
    "step" VARCHAR(64) NOT NULL,
    "state" VARCHAR(32) NOT NULL,
    "native_result" VARCHAR(512) NULL,
    "parent_operation_uuid" VARCHAR(36) NULL,
    "lease_owner" VARCHAR(128) NULL,
    "lease_expires_since_epoch" BIGINT NULL,
    "fencing_token" BIGINT NOT NULL DEFAULT 0,
    "attempts" INT NOT NULL DEFAULT 0,
    "next_retry_since_epoch" BIGINT NULL,
    "failure_code" VARCHAR(128) NULL,
    "create_time_since_epoch" BIGINT NOT NULL,
    "last_update_time_since_epoch" BIGINT NOT NULL,
    CONSTRAINT "uq_mlflow_operation_resource_revision_kind" UNIQUE ("resource_id", "expected_revision", "kind")
);

CREATE INDEX "idx_mlflow_operation_runnable" ON "MLflowOperation" ("state", "next_retry_since_epoch");
