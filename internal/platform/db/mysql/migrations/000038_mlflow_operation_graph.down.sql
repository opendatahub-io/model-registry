CREATE TEMPORARY TABLE mlflow_graph_down_guard (unsafe INT NOT NULL CHECK (unsafe = 0));
INSERT INTO mlflow_graph_down_guard (unsafe)
SELECT 1 WHERE EXISTS (SELECT 1 FROM `MLflowIntentResource`)
 OR EXISTS (SELECT 1 FROM `MLflowOperation` WHERE `aggregate_operation_uuid` IS NOT NULL OR `superseded_by_operation_uuid` IS NOT NULL OR `execution_plan_json` IS NOT NULL)
 OR EXISTS (SELECT 1 FROM `MLflowOperation` GROUP BY `resource_id`, `expected_revision`, `kind` HAVING COUNT(*) > 1)
 OR EXISTS (SELECT 1 FROM `MLflowOperation` WHERE COALESCE(JSON_EXTRACT(`candidate_json`, '$.codecVersion'), 0) > 2)
 OR EXISTS (SELECT 1 FROM `MLflowObject` WHERE COALESCE(JSON_EXTRACT(`committed_json`, '$.codecVersion'), 0) > 2);
DROP TABLE mlflow_graph_down_guard;
DROP TABLE `MLflowIntentResource`;
DROP INDEX `idx_mlflow_operation_aggregate` ON `MLflowOperation`;
ALTER TABLE `MLflowOperation`
 ADD CONSTRAINT `uq_mlflow_operation_resource_revision_kind` UNIQUE (`resource_id`, `expected_revision`, `kind`),
 DROP INDEX `uq_mlflow_operation_request`,
 DROP COLUMN `request_hash`,
 DROP COLUMN `request_sequence`,
 DROP COLUMN `aggregate_operation_uuid`,
 DROP COLUMN `superseded_by_operation_uuid`,
 DROP COLUMN `execution_plan_json`;
UPDATE `MLflowAdapter` SET `schema_version` = 1, `codec_version` = 2;
