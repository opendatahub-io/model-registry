-- Enforce exact byte-wise identity comparisons on MLflowObject (F7).
-- The table was created with utf8mb4_bin, which is PAD SPACE on MySQL 8:
-- 'name' and 'name ' compare equal, so PostgreSQL and MySQL disagreed on
-- identity. utf8mb4_0900_bin is the NO PAD binary collation ( available on
-- MySQL 8.0.17+ ), making trailing spaces and case significant on both
-- engines. Only the MLflow compatibility columns change; the server-wide
-- collation is untouched.
ALTER TABLE `MLflowObject`
  MODIFY `name` VARCHAR(512) CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_bin NOT NULL,
  MODIFY `external_id` VARCHAR(512) CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_bin NULL,
  MODIFY `native_model_name` VARCHAR(512) CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_bin NULL;
