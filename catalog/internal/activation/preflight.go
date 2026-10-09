// Package activation validates required image content before datastore writes.
// A successful preflight is not evidence of successful database activation.
package activation

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/kubeflow/hub/catalog/internal/catalog/agentcatalog"
	"github.com/kubeflow/hub/catalog/internal/catalog/basecatalog"
	"github.com/kubeflow/hub/catalog/internal/catalog/mcpcatalog"
	"github.com/kubeflow/hub/catalog/internal/catalog/modelcatalog"
	"github.com/kubeflow/hub/catalog/internal/catalog/serving_runtimecatalog"
)

// Failure is the failure-only termination-message contract consumed by the
// operator. It intentionally does not report activation success.
type Failure struct {
	Stage   string `json:"stage"`
	Reason  string `json:"reason"`
	Message string `json:"message"`
}

func (f *Failure) Error() string { return f.Stage + ": " + f.Reason + ": " + f.Message }

// WriteTerminationMessage writes a bounded JSON failure to the Kubernetes
// termination-message file. An empty path disables reporting for standalone use.
func (f *Failure) WriteTerminationMessage(path string) error {
	if path == "" {
		return nil
	}
	bounded := *f
	// JSON escaping can expand one rune to six bytes. Leave space for the
	// envelope within Kubernetes' 4 KiB termination-message limit.
	if message := []rune(bounded.Message); len(message) > 512 {
		bounded.Message = string(message[:512])
	}
	data, err := json.Marshal(&bounded)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}

// Preflight validates explicitly required sources and benchmarks before any
// plugin initialization or leader writes. Other configuration paths are never
// read, merged, or modified here.
func Preflight(requiredConfigs, benchmarkPaths []string, requireBenchmarks bool) *Failure {
	for _, path := range requiredConfigs {
		if err := validateSources(path); err != nil {
			return &Failure{Stage: "Loading", Reason: "CatalogContentInvalid", Message: err.Error()}
		}
	}
	if requireBenchmarks {
		if err := modelcatalog.ValidateRequiredPerformanceMetrics(benchmarkPaths); err != nil {
			return &Failure{Stage: "Loading", Reason: "BenchmarkContentInvalid", Message: err.Error()}
		}
	}
	return nil
}

func validateSources(path string) error {
	config, err := basecatalog.ReadSourceConfig(path)
	if err != nil {
		return fmt.Errorf("required source configuration %s: %w", path, err)
	}
	validate := func(id, sourceType string, enabled bool, properties map[string]any, fn func(string) error) error {
		if !enabled {
			return nil
		}
		if sourceType != "yaml" {
			return fmt.Errorf("required source %s: type %q is not a shipped YAML provider", id, sourceType)
		}
		dataPath, ok := properties["yamlCatalogPath"].(string)
		if !ok || dataPath == "" {
			return fmt.Errorf("required source %s: yamlCatalogPath is missing", id)
		}
		if !filepath.IsAbs(dataPath) {
			dataPath = filepath.Join(filepath.Dir(path), dataPath)
		}
		if err := fn(dataPath); err != nil {
			return fmt.Errorf("required source %s: %w", id, err)
		}
		return nil
	}
	for _, source := range config.GetModelCatalogs() {
		if err := validate(source.Id, source.Type, source.Enabled == nil || *source.Enabled, source.Properties, modelcatalog.ValidateRequiredYAML); err != nil {
			return err
		}
	}
	for _, source := range config.MCPCatalogs {
		if err := validate(source.ID, source.Type, source.IsEnabled(), source.Properties, mcpcatalog.ValidateRequiredYAML); err != nil {
			return err
		}
	}
	for _, source := range config.AgentCatalogs {
		if err := validate(source.ID, source.Type, source.IsEnabled(), source.Properties, agentcatalog.ValidateRequiredYAML); err != nil {
			return err
		}
	}
	for _, source := range config.ServingRuntimeCatalogs {
		if err := validate(source.ID, source.Type, source.IsEnabled(), source.Properties, serving_runtimecatalog.ValidateRequiredYAML); err != nil {
			return err
		}
	}
	for _, source := range config.SkillCatalogs {
		if source.IsEnabled() {
			return fmt.Errorf("required source %s: shipped skill validation is not supported", source.ID)
		}
	}
	return nil
}
