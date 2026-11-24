-- Refuse to drop the visibility column while reserved rows remain: doing so
-- would expose unpublished (failed or in-flight) native creations as
-- successful resources. The operator must resolve or remove the reserved
-- rows first; no data is ever deleted.
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM "MLflowObject" WHERE "visibility" = 'RESERVED') THEN
    RAISE EXCEPTION 'cannot drop MLflowObject.visibility while RESERVED rows remain; resolve or remove them first';
  END IF;
END $$;
ALTER TABLE "MLflowObject" DROP COLUMN "visibility";
