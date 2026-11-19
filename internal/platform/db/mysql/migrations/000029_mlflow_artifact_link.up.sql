-- Compatibility store for the MLflow model-registry backend (3/6).
-- Artifact-to-version relationships; one artifact may link to many
-- versions. The primary artifact selection lives in MLflowVersionBinding.
CREATE TABLE `MLflowArtifactLink` (
    `artifact_id` INT NOT NULL,
    `version_id` INT NOT NULL,
    PRIMARY KEY (`artifact_id`, `version_id`),
    INDEX `idx_mlflow_artifact_link_version` (`version_id`)
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_bin;
