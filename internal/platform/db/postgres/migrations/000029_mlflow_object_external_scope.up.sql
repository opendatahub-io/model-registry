-- Enforce the reference backend's external-ID uniqueness scope on
-- MLflowObject: external IDs are unique across registered models and
-- model versions jointly ( they share the reference Context table ) and
-- separately within model artifacts. A stored generated column collapses
-- the kind into the two scopes so one unique key covers both rules;
-- NULL external IDs remain unrestricted on both engines.
ALTER TABLE "MLflowObject"
    ADD COLUMN "external_scope" VARCHAR(16) GENERATED ALWAYS AS (
        CASE WHEN "kind" = 'model-artifact' THEN 'artifact' ELSE 'context' END
    ) STORED,
    ADD CONSTRAINT "uq_mlflow_object_scope_external_id" UNIQUE ("external_scope", "external_id");
