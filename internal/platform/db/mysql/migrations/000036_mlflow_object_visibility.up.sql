-- Publication visibility for MLflowObject: rows reserved for native-backed
-- creates stay invisible to public reads until publication commits them, so
-- a rejected native create never appears as a successful resource. Existing
-- rows are committed by definition of the upgrade.
ALTER TABLE `MLflowObject`
  ADD COLUMN `visibility` VARCHAR(16) NOT NULL DEFAULT 'COMMITTED';
