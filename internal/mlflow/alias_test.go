package mlflow_test

import (
	"regexp"
	"strings"
	"testing"

	"github.com/kubeflow/hub/internal/mlflow"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var aliasPattern = regexp.MustCompile(`^rhoai-[a-z0-9_-]{1,160}-[0-9a-f]{64}$`)

// TestAliasNormalization covers the alias algorithm's required cases:
// ordinary versions, reserved-looking names, Unicode, punctuation-only
// and long names, and collisions the slug alone would merge.
func TestAliasNormalization(t *testing.T) {
	cases := map[string]string{
		"1.0.0":        "1-0-0",
		"latest":       "latest",
		"v123":         "v123",
		"V1":           "v1",
		"a.b":          "a-b",
		"a-b":          "a-b",
		"héllo":        "h-llo", // the non-ASCII rune collapses to one separator
		"1.0.0-beta.1": "1-0-0-beta-1",
	}

	for name, slug := range cases {
		alias := mlflow.NormalizedAlias(name)
		assert.Regexp(t, aliasPattern, alias, "alias for %q", name)
		assert.Contains(t, alias, "rhoai-"+slug+"-", "alias for %q", name)
	}

	// Punctuation-only names fall back to "version".
	assert.Regexp(t, aliasPattern, mlflow.NormalizedAlias("..."))
	assert.Contains(t, mlflow.NormalizedAlias("..."), "rhoai-version-")
	assert.Regexp(t, aliasPattern, mlflow.NormalizedAlias(""))

	// a.b and a-b share a slug but differ by digest; V1 and v1 differ
	// only by digest because the original name is hashed as UTF-8.
	require.NotEqual(t, mlflow.NormalizedAlias("a.b"), mlflow.NormalizedAlias("a-b"))
	require.NotEqual(t, mlflow.NormalizedAlias("V1"), mlflow.NormalizedAlias("v1"))

	// The alias is deterministic.
	require.Equal(t, mlflow.NormalizedAlias("1.0.0"), mlflow.NormalizedAlias("1.0.0"))
}

// TestAliasLengthBound proves long names stay within 255 characters.
func TestAliasLengthBound(t *testing.T) {
	for _, length := range []int{1, 100, 160, 500, 5000} {
		alias := mlflow.NormalizedAlias(strings.Repeat("a", length))
		assert.LessOrEqual(t, len(alias), 255, "alias for %d characters", length)
		assert.Regexp(t, aliasPattern, alias)
	}

	// A name that is all separators after truncation still yields the
	// fallback slug rather than a dangling separator.
	alias := mlflow.NormalizedAlias(strings.Repeat("-", 500))
	assert.Regexp(t, aliasPattern, alias)
	assert.Contains(t, alias, "rhoai-version-")
}
