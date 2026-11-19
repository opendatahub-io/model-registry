ALTER TABLE `MLflowOperation` MODIFY COLUMN `native_result` TEXT NULL;
CREATE TABLE `MLflowNativeDisposition` (
 `native_model_name` VARCHAR(512) CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_bin NOT NULL,
 `native_version_number` VARCHAR(64) CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_bin NOT NULL,
 `adapter_uuid` VARCHAR(36) NOT NULL,
 `version_id` INT NOT NULL,
 `operation_uuid` VARCHAR(36) NOT NULL,
 `generation` BIGINT NOT NULL,
 `disposition` VARCHAR(32) NOT NULL,
 `resolving_operation_uuid` VARCHAR(36) NOT NULL,
 `resolved_time_since_epoch` BIGINT NOT NULL,
 PRIMARY KEY (`native_model_name`, `native_version_number`),
 CONSTRAINT `ck_mlflow_native_disposition` CHECK (`disposition` IN ('UNSELECTED_DUPLICATE', 'ORPHAN_RETAINED'))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;
CREATE INDEX `idx_mlflow_native_disposition_operation` ON `MLflowNativeDisposition` (`operation_uuid`);
