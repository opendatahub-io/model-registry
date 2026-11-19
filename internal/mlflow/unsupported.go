package mlflow

import (
	"fmt"

	"github.com/kubeflow/hub/pkg/api"
	"github.com/kubeflow/hub/pkg/openapi"
)

// Unsupported resource families. The MLflow backend deliberately
// serves only registered models, model versions, and model artifacts;
// serving, inference-service, experiment, run, run-artifact, and
// metric-history methods return a stable 501 without side effects, and
// unsupported list methods error rather than pretending the registry is
// empty. Inference-service lookups are unsupported because the
// relationship itself has no MLflow representation.

func errServiceInit(reason string) error {
	return fmt.Errorf("creating mlflow backend service: %s: %w", reason, api.ErrBadRequest)
}

func errUnsupportedResource(resource string) error {
	return fmt.Errorf("mlflow backend does not support %s: %w", resource, api.ErrNotImplemented)
}

// REGISTERED MODEL / MODEL VERSION / MODEL ARTIFACT inference-service
// traversal.

func (*Service) GetRegisteredModelByInferenceService(string) (*openapi.RegisteredModel, error) {
	return nil, errUnsupportedResource("looking up a registered model by inference service")
}

func (*Service) GetModelVersionByInferenceService(string) (*openapi.ModelVersion, error) {
	return nil, errUnsupportedResource("looking up a model version by inference service")
}

func (*Service) GetModelArtifactByInferenceService(string) (*openapi.ModelArtifact, error) {
	return nil, errUnsupportedResource("looking up a model artifact by inference service")
}

// SERVING ENVIRONMENT.

func (*Service) UpsertServingEnvironment(*openapi.ServingEnvironment) (*openapi.ServingEnvironment, error) {
	return nil, errUnsupportedResource("serving environments")
}

func (*Service) GetServingEnvironmentById(string) (*openapi.ServingEnvironment, error) {
	return nil, errUnsupportedResource("serving environments")
}

func (*Service) GetServingEnvironmentByParams(*string, *string) (*openapi.ServingEnvironment, error) {
	return nil, errUnsupportedResource("serving environments")
}

func (*Service) GetServingEnvironments(api.ListOptions) (*openapi.ServingEnvironmentList, error) {
	return nil, errUnsupportedResource("serving environments")
}

// INFERENCE SERVICE.

func (*Service) UpsertInferenceService(*openapi.InferenceService) (*openapi.InferenceService, error) {
	return nil, errUnsupportedResource("inference services")
}

func (*Service) GetInferenceServiceById(string) (*openapi.InferenceService, error) {
	return nil, errUnsupportedResource("inference services")
}

func (*Service) GetInferenceServiceByParams(*string, *string, *string) (*openapi.InferenceService, error) {
	return nil, errUnsupportedResource("inference services")
}

func (*Service) GetInferenceServices(api.ListOptions, *string, *string) (*openapi.InferenceServiceList, error) {
	return nil, errUnsupportedResource("inference services")
}

// SERVE MODEL.

func (*Service) UpsertServeModel(*openapi.ServeModel, *string) (*openapi.ServeModel, error) {
	return nil, errUnsupportedResource("serve models")
}

func (*Service) GetServeModelById(string) (*openapi.ServeModel, error) {
	return nil, errUnsupportedResource("serve models")
}

func (*Service) GetServeModels(api.ListOptions, *string) (*openapi.ServeModelList, error) {
	return nil, errUnsupportedResource("serve models")
}

// EXPERIMENT.

func (*Service) UpsertExperiment(*openapi.Experiment) (*openapi.Experiment, error) {
	return nil, errUnsupportedResource("experiments")
}

func (*Service) GetExperimentById(string) (*openapi.Experiment, error) {
	return nil, errUnsupportedResource("experiments")
}

func (*Service) GetExperimentByParams(*string, *string) (*openapi.Experiment, error) {
	return nil, errUnsupportedResource("experiments")
}

func (*Service) GetExperiments(api.ListOptions) (*openapi.ExperimentList, error) {
	return nil, errUnsupportedResource("experiments")
}

// EXPERIMENT RUN.

func (*Service) UpsertExperimentRun(*openapi.ExperimentRun, *string) (*openapi.ExperimentRun, error) {
	return nil, errUnsupportedResource("experiment runs")
}

func (*Service) GetExperimentRunById(string) (*openapi.ExperimentRun, error) {
	return nil, errUnsupportedResource("experiment runs")
}

func (*Service) GetExperimentRunByParams(*string, *string, *string) (*openapi.ExperimentRun, error) {
	return nil, errUnsupportedResource("experiment runs")
}

func (*Service) GetExperimentRuns(api.ListOptions, *string) (*openapi.ExperimentRunList, error) {
	return nil, errUnsupportedResource("experiment runs")
}

// EXPERIMENT RUN ARTIFACTS.

func (*Service) UpsertExperimentRunArtifact(*openapi.Artifact, string) (*openapi.Artifact, error) {
	return nil, errUnsupportedResource("experiment run artifacts")
}

func (*Service) GetExperimentRunArtifacts(openapi.ArtifactTypeQueryParam, api.ListOptions, *string) (*openapi.ArtifactList, error) {
	return nil, errUnsupportedResource("experiment run artifacts")
}

// EXPERIMENT RUN METRIC HISTORY.

func (*Service) GetExperimentRunMetricHistory(*string, *string, api.ListOptions, *string) (*openapi.MetricList, error) {
	return nil, errUnsupportedResource("experiment run metric history")
}
