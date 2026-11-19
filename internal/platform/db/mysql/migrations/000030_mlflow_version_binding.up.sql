-- Compatibility store for the MLflow model-registry backend (4/6).
-- Materialization state of a logical model version. Native identity is
-- NULL while the version is pending; once bound it is unique.
CREATE TABLE `MLflowVersionBinding` (
    `id` INT NOT NULL AUTO_INCREMENT PRIMARY KEY,
    `version_id` INT NOT NULL,
    `model_id` INT NOT NULL,
    `primary_artifact_id` INT NULL,
    `phase` VARCHAR(32) NOT NULL,
    `native_model_name` VARCHAR(512) NULL,
    `native_version_number` VARCHAR(32) NULL,
    `active_generation` BIGINT NOT NULL,
    `desired_generation` BIGINT NOT NULL,
    `alias` VARCHAR(255) NOT NULL,
    `revision` BIGINT NOT NULL,
    CONSTRAINT `uq_mlflow_binding_version` UNIQUE (`version_id`),
    CONSTRAINT `uq_mlflow_binding_native` UNIQUE (`native_model_name`, `native_version_number`),
    INDEX `idx_mlflow_binding_model` (`model_id`),
    INDEX `idx_mlflow_binding_phase` (`phase`)
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_bin;
