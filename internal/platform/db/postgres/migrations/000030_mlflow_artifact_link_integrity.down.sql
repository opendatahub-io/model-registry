-- Remove the artifact-link foreign keys.
ALTER TABLE "MLflowArtifactLink"
  DROP CONSTRAINT "fk_mlflow_link_artifact",
  DROP CONSTRAINT "fk_mlflow_link_version";
