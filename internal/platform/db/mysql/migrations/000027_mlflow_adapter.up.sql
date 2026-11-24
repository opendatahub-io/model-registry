-- Compatibility store for the MLflow model-registry backend (1/6).
-- Singleton adapter identity: binds this compatibility database to one
-- MLflow registry target and records codec/schema versions.
CREATE TABLE `MLflowAdapter` (
    `id` INT NOT NULL AUTO_INCREMENT PRIMARY KEY,
    `adapter_uuid` VARCHAR(36) NOT NULL,
    `target_fingerprint` VARCHAR(256) NOT NULL,
    `schema_version` INT NOT NULL,
    `codec_version` INT NOT NULL,
    `last_refresh_time` BIGINT NULL,
    CONSTRAINT `uq_mlflow_adapter_uuid` UNIQUE (`adapter_uuid`)
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_bin;
