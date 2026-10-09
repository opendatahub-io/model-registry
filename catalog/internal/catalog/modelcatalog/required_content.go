package modelcatalog

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// ValidateRequiredYAML validates shipped data without invoking providers or
// writing to the database. User-configured providers retain their existing
// partial-availability behavior.
func ValidateRequiredYAML(path string) error {
	catalog, err := (&yamlModelProvider{path: path}).read()
	if err != nil {
		return err
	}
	if catalog.Models == nil {
		return fmt.Errorf("%s: required models list is missing", path)
	}
	names := make(map[string]bool, len(catalog.Models))
	for i, model := range catalog.Models {
		if strings.TrimSpace(model.Name) == "" || names[model.Name] {
			return fmt.Errorf("%s: model %d has an empty or duplicate name", path, i)
		}
		names[model.Name] = true
		for j, artifact := range model.Artifacts {
			if artifact == nil {
				return fmt.Errorf("%s: model %s artifact %d is null", path, model.Name, j)
			}
		}
	}
	return nil
}

// ValidateRequiredPerformanceMetrics checks every shipped benchmark file,
// including data for models not selected by the catalog. It must run before any
// leader ingestion, so malformed later files cannot cause a partial update.
func ValidateRequiredPerformanceMetrics(paths []string) error {
	if len(paths) == 0 {
		return fmt.Errorf("required benchmark paths are not configured")
	}
	for _, root := range paths {
		info, err := os.Stat(root)
		if err != nil {
			return fmt.Errorf("required benchmark directory %s: %w", root, err)
		}
		if !info.IsDir() {
			return fmt.Errorf("required benchmark path %s is not a directory", root)
		}
		metadataCount := 0
		ids := map[string]bool{}
		err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() {
				return nil
			}
			switch entry.Name() {
			case "metadata.json":
				data, err := os.ReadFile(path)
				if err != nil {
					return fmt.Errorf("%s: %w", path, err)
				}
				metadata, err := parseMetadataJSON(data)
				if err != nil {
					return fmt.Errorf("%s: %w", path, err)
				}
				id := strings.ToLower(metadata.ID)
				if strings.TrimSpace(id) == "" || ids[id] {
					return fmt.Errorf("%s: empty or duplicate benchmark model ID %q", path, metadata.ID)
				}
				ids[id] = true
				metadataCount++
			case "evaluations.ndjson", "performance.ndjson", "security-evaluations.ndjson":
				if _, err := os.Stat(filepath.Join(filepath.Dir(path), "metadata.json")); err != nil {
					return fmt.Errorf("%s: benchmark metadata is required: %w", path, err)
				}
				return validateRequiredMetricsFile(path)
			}
			return nil
		})
		if err != nil {
			return fmt.Errorf("required benchmarks in %s: %w", root, err)
		}
		if metadataCount == 0 {
			return fmt.Errorf("required benchmark directory %s contains no model metadata", root)
		}
	}
	return nil
}

func validateRequiredMetricsFile(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	scanner := bufio.NewScanner(file)
	line := 0
	for scanner.Scan() {
		line++
		data := scanner.Bytes()
		if strings.TrimSpace(string(data)) == "" {
			continue
		}
		var err error
		switch filepath.Base(path) {
		case "evaluations.ndjson":
			var record evaluationRecord
			err = json.Unmarshal(data, &record)
			if err == nil && (strings.TrimSpace(record.ModelID) == "" || strings.TrimSpace(record.Benchmark) == "") {
				err = fmt.Errorf("model_id and benchmark are required")
			}
		case "performance.ndjson":
			var record performanceRecord
			err = json.Unmarshal(data, &record)
			if err == nil && (strings.TrimSpace(record.ModelID) == "" || strings.TrimSpace(record.ID) == "") {
				err = fmt.Errorf("model_id and id (or config_id) are required")
			}
		case "security-evaluations.ndjson":
			var record securityEvaluationRecord
			err = json.Unmarshal(data, &record)
			if err == nil && (strings.TrimSpace(record.ModelID) == "" || strings.TrimSpace(record.ID) == "") {
				err = fmt.Errorf("model_id and id are required")
			}
		}
		if err != nil {
			return fmt.Errorf("%s line %d: %w", path, line, err)
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("%s line %d: %w", path, line+1, err)
	}
	return nil
}
