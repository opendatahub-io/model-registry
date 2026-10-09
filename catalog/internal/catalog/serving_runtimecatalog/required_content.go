package serving_runtimecatalog

// ValidateRequiredYAML applies all-or-nothing validation to shipped runtime
// families without changing partial validation of administrator sources.
func ValidateRequiredYAML(path string) error {
	_, err := loadServingRuntimesFromYAML(path)
	return err
}
