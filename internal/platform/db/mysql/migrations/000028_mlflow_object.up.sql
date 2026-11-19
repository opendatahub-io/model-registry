-- Compatibility store for the MLflow model-registry backend (2/6).
-- One row per logical Kubeflow resource (registered model, model version,
-- model artifact). parent_id uses 0 (not NULL) for parentless resources so
-- the kind/parent/name uniqueness is enforceable on MySQL.
CREATE TABLE `MLflowObject` (
    `id` INT NOT NULL AUTO_INCREMENT PRIMARY KEY,
    `kind` VARCHAR(32) NOT NULL,
    `parent_id` INT NOT NULL DEFAULT 0,
    `name` VARCHAR(512) NOT NULL,
    `external_id` VARCHAR(512) NULL,
    `create_time_since_epoch` BIGINT NOT NULL,
    `last_update_time_since_epoch` BIGINT NOT NULL,
    `committed_json` LONGTEXT NOT NULL,
    `revision` BIGINT NOT NULL,
    `deleted` TINYINT(1) NOT NULL DEFAULT 0,
    `native_model_name` VARCHAR(512) NULL,
    CONSTRAINT `uq_mlflow_object_kind_parent_name` UNIQUE (`kind`, `parent_id`, `name`),
    INDEX `idx_mlflow_object_parent_kind` (`parent_id`, `kind`),
    INDEX `idx_mlflow_object_external_id` (`external_id`),
    INDEX `idx_mlflow_object_deleted` (`deleted`),
    INDEX `idx_mlflow_object_native_model_name` (`native_model_name`)
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_bin;
