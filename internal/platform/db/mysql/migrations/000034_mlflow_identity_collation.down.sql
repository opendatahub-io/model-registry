-- Revert identity columns to the original PAD SPACE collation. The unique
-- index is rebuilt during the ALTER: if any rows would collapse to the same
-- (kind, parent_id, name) or (external_scope, external_id) identity under
-- PAD SPACE comparison, MySQL rejects the change with a duplicate-entry
-- error and no data is lost. The operator must resolve or remove the
-- colliding identities before retrying; this migration never deletes rows.
ALTER TABLE `MLflowObject`
  MODIFY `name` VARCHAR(512) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL,
  MODIFY `external_id` VARCHAR(512) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NULL,
  MODIFY `native_model_name` VARCHAR(512) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NULL;
