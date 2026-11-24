package client

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/kubeflow/hub/pkg/api"
)

// ParseRegistryURL validates a registry target without exposing its contents
// in errors. Authentication belongs in files; query strings and fragments
// cannot form part of the registry's stable identity.
func ParseRegistryURL(raw string) (*url.URL, error) {
	invalid := func(category string) (*url.URL, error) {
		return nil, fmt.Errorf("invalid mlflow registry URL: %s: %w", category, api.ErrBadRequest)
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return invalid("parse failed")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return invalid("scheme must be http or https")
	}
	if parsed.Host == "" || parsed.Hostname() == "" || parsed.Opaque != "" {
		return invalid("host is required")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || strings.Contains(raw, "#") {
		return invalid("credentials, query strings, and fragments are forbidden")
	}
	return parsed, nil
}
