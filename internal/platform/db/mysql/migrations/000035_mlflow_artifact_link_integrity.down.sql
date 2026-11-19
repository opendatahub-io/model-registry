-- Remove the artifact-link foreign keys.
ALTER TABLE `MLflowArtifactLink`
  DROP FOREIGN KEY `fk_mlflow_link_artifact`,
  DROP FOREIGN KEY `fk_mlflow_link_version`;
