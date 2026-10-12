package modelcatalog

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/kubeflow/hub/catalog/internal/catalog/basecatalog"
	"github.com/kubeflow/hub/catalog/internal/catalog/modelcatalog/models"
)

// ImportRequired imports one prevalidated source synchronously. The caller owns
// the transaction: every model, artifact and benchmark must succeed before the
// complete candidate can be committed or exposed to readers.
func ImportRequired(ctx context.Context, path string, source *basecatalog.ModelSource, services Services, metrics *PerformanceMetricsLoader) error {
	catalog, err := (&yamlModelProvider{path: path}).read()
	if err != nil {
		return err
	}
	filter, err := yamlModelFilter(source)
	if err != nil {
		return err
	}
	loader := &ModelLoader{}
	for _, entry := range catalog.Models {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !filter.Allows(entry.Name) {
			continue
		}
		record := entry.ToModelProviderRecord()
		if record.Error != nil {
			return record.Error
		}
		loader.setModelSourceID(record.Model, source.Id)
		model, err := services.CatalogModelRepository.Save(record.Model)
		if err != nil {
			return fmt.Errorf("model %q: %w", entry.Name, err)
		}
		if model.GetID() == nil {
			return fmt.Errorf("model %q has no persisted ID", entry.Name)
		}
		for _, artifact := range record.Artifacts {
			switch {
			case artifact.CatalogModelArtifact != nil:
				value, ok := artifact.CatalogModelArtifact.(models.CatalogModelArtifact)
				if !ok {
					return fmt.Errorf("model %q has an invalid artifact", entry.Name)
				}
				_, err = services.CatalogModelArtifactRepository.Save(value, model.GetID())
			case artifact.CatalogMetricsArtifact != nil:
				value, ok := artifact.CatalogMetricsArtifact.(models.CatalogMetricsArtifact)
				if !ok {
					return fmt.Errorf("model %q has an invalid metrics artifact", entry.Name)
				}
				_, err = services.CatalogMetricsArtifactRepository.Save(value, model.GetID())
			default:
				return fmt.Errorf("model %q has an empty artifact", entry.Name)
			}
			if err != nil {
				return fmt.Errorf("model %q artifact: %w", entry.Name, err)
			}
		}
		if err := metrics.LoadRequired(ctx, record); err != nil {
			return fmt.Errorf("model %q benchmarks: %w", entry.Name, err)
		}
	}
	return nil
}

// LoadRequired propagates metadata writes as well as metric writes. The ordinary
// optional benchmark loader retains its partial-availability policy.
func (pml *PerformanceMetricsLoader) LoadRequired(ctx context.Context, record ModelProviderRecord) error {
	if pml == nil {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return pml.loadRequired(record)
}

func (pml *PerformanceMetricsLoader) loadRequired(record ModelProviderRecord) error {
	name := *record.Model.GetAttributes().Name
	dir, found := pml.modelDirCache[strings.ToLower(DisplayNameFromStoredName(name))]
	if !found {
		return nil // Benchmarks are optional for an individual model.
	}
	data, err := os.ReadFile(filepath.Join(dir, "metadata.json"))
	if err != nil {
		return err
	}
	metadata, err := parseMetadataJSON(data)
	if err != nil {
		return err
	}
	model, err := pml.modelRepo.GetByName(name)
	if err != nil {
		return err
	}
	if err := enrichCatalogModelFromMetadata(model, metadata, pml.modelRepo); err != nil {
		return err
	}
	id := model.GetID()
	evaluations, err := readRequiredMetrics[evaluationRecord](filepath.Join(dir, "evaluations.ndjson"))
	if err != nil {
		return err
	}
	if len(evaluations) > 0 {
		artifact := createAccuracyMetricsArtifact(evaluations, *id, pml.metricsArtifactTypeID, metadata.OverallAccuracy, nil, nil)
		if _, err := pml.metricsArtifactRepo.Save(artifact, id); err != nil {
			return err
		}
	}
	performance, err := readRequiredMetrics[performanceRecord](filepath.Join(dir, "performance.ndjson"))
	if err != nil {
		return err
	}
	for _, value := range performance {
		if _, err := pml.metricsArtifactRepo.Save(createPerformanceArtifact(value, *id, pml.metricsArtifactTypeID, nil, nil), id); err != nil {
			return err
		}
	}
	security, err := readRequiredMetrics[securityEvaluationRecord](filepath.Join(dir, "security-evaluations.ndjson"))
	if err != nil {
		return err
	}
	for _, value := range security {
		if _, err := pml.metricsArtifactRepo.Save(createSecurityArtifact(value, *id, pml.metricsArtifactTypeID, nil, nil), id); err != nil {
			return err
		}
	}
	return nil
}

func readRequiredMetrics[T any](path string) ([]T, error) {
	file, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	var records []T
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		if strings.TrimSpace(scanner.Text()) == "" {
			continue
		}
		var value T
		if err := json.Unmarshal(scanner.Bytes(), &value); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		records = append(records, value)
	}
	return records, scanner.Err()
}
