-- Compatibility store for the MLflow model-registry backend (6/6).
-- Durable operation journal. The candidate payload is an immutable
-- snapshot separate from the committed object, so rejected native writes
-- never change visible state. Lease fields support multi-replica recovery
-- with fencing tokens.
CREATE TABLE `MLflowOperation` (
    `operation_uuid` VARCHAR(36) NOT NULL PRIMARY KEY,
    `resource_id` INT NOT NULL,
    `kind` VARCHAR(64) NOT NULL,
    `expected_revision` BIGINT NOT NULL,
    `candidate_json` LONGTEXT NOT NULL,
    `payload_hash` VARCHAR(64) NOT NULL,
    `generation` BIGINT NOT NULL,
    `step` VARCHAR(64) NOT NULL,
    `state` VARCHAR(32) NOT NULL,
    `native_result` VARCHAR(512) NULL,
    `parent_operation_uuid` VARCHAR(36) NULL,
    `lease_owner` VARCHAR(128) NULL,
    `lease_expires_since_epoch` BIGINT NULL,
    `fencing_token` BIGINT NOT NULL DEFAULT 0,
    `attempts` INT NOT NULL DEFAULT 0,
    `next_retry_since_epoch` BIGINT NULL,
    `failure_code` VARCHAR(128) NULL,
    `create_time_since_epoch` BIGINT NOT NULL,
    `last_update_time_since_epoch` BIGINT NOT NULL,
    CONSTRAINT `uq_mlflow_operation_resource_revision_kind` UNIQUE (`resource_id`, `expected_revision`, `kind`),
    INDEX `idx_mlflow_operation_runnable` (`state`, `next_retry_since_epoch`)
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_bin;
