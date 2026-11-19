-- Compatibility store for the MLflow model-registry backend (5/6).
-- Immutable record of every native generation ever bound to a logical
-- version. Retained history prevents imports from re-creating superseded
-- replacements as new logical versions.
CREATE TABLE `MLflowVersionHistory` (
    `id` BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
    `version_id` INT NOT NULL,
    `generation` BIGINT NOT NULL,
    `native_model_name` VARCHAR(512) NOT NULL,
    `native_version_number` VARCHAR(32) NOT NULL,
    `source` LONGTEXT NOT NULL,
    `operation_uuid` VARCHAR(36) NOT NULL,
    `superseded` TINYINT(1) NOT NULL DEFAULT 0,
    `superseded_time` BIGINT NULL,
    CONSTRAINT `uq_mlflow_history_native` UNIQUE (`native_model_name`, `native_version_number`),
    CONSTRAINT `uq_mlflow_history_version_generation` UNIQUE (`version_id`, `generation`)
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_bin;
