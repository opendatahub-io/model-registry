CREATE TEMPORARY TABLE mlflow_disposition_down_guard (unsafe INT NOT NULL CHECK (unsafe = 0));
INSERT INTO mlflow_disposition_down_guard (unsafe)
SELECT 1 WHERE EXISTS (SELECT 1 FROM `MLflowNativeDisposition`)
 OR EXISTS (SELECT 1 FROM `MLflowOperation` WHERE `failure_code` IN ('OPERATOR_SELECTED_NATIVE', 'ORPHAN_RETAINED')
   OR CHAR_LENGTH(`native_result`) > 512
   OR `native_result` REGEXP '"explicitSelection"[[:space:]]*:[[:space:]]*true');
DROP TABLE mlflow_disposition_down_guard;
ALTER TABLE `MLflowOperation` MODIFY COLUMN `native_result` VARCHAR(512) NULL;
DROP TABLE `MLflowNativeDisposition`;
