-- Enforce referential integrity on MLflowArtifactLink (F9): every link must
-- reference an existing MLflowObject row. Upgrades fail with a clear
-- foreign-key error when orphaned links exist; the operator must repair or
-- remove them -- this migration never deletes rows or renumbers resources.
-- PostgreSQL text comparison is already exact, so no collation change is
-- needed on this engine.
ALTER TABLE "MLflowArtifactLink"
  ADD CONSTRAINT "fk_mlflow_link_artifact" FOREIGN KEY ("artifact_id") REFERENCES "MLflowObject" ("id"),
  ADD CONSTRAINT "fk_mlflow_link_version" FOREIGN KEY ("version_id") REFERENCES "MLflowObject" ("id");
