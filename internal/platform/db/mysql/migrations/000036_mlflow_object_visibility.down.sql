-- Refuse to drop the visibility column while reserved rows remain: doing so
-- would expose unpublished (failed or in-flight) native creations as
-- successful resources. The guard insert duplicates any reserved row's
-- (kind, parent_id, name) identity, so the unique key rejects the migration
-- before any schema change; the operator must resolve or remove the
-- reserved rows first. No data is ever deleted.
INSERT INTO `MLflowObject`
  (`kind`, `parent_id`, `name`, `create_time_since_epoch`, `last_update_time_since_epoch`, `committed_json`, `revision`, `deleted`)
SELECT `kind`, `parent_id`, `name`, 0, 0, 'visibility-down-guard', 0, 0
FROM `MLflowObject` WHERE `visibility` = 'RESERVED';
ALTER TABLE `MLflowObject` DROP COLUMN `visibility`;
