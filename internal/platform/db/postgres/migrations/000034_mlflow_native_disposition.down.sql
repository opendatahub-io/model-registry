DO $$ BEGIN
 IF EXISTS (SELECT 1 FROM "MLflowNativeDisposition")
 OR EXISTS (SELECT 1 FROM "MLflowOperation" WHERE "failure_code" IN ('OPERATOR_SELECTED_NATIVE', 'ORPHAN_RETAINED')
   OR CHAR_LENGTH("native_result") > 512
   OR "native_result" ~ '"explicitSelection"[[:space:]]*:[[:space:]]*true') THEN
  RAISE EXCEPTION 'cannot downgrade retained MLflow native resolution evidence';
 END IF;
END $$;
ALTER TABLE "MLflowOperation" ALTER COLUMN "native_result" TYPE VARCHAR(512);
DROP TABLE "MLflowNativeDisposition";
