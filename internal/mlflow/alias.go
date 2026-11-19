package mlflow

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// Adapter tag key prefixes and the alias prefix. Everything under
// "rhoai." is adapter-owned: user properties colliding with it are
// escaped, and user code must never overwrite lifecycle or operation
// tags.
const (
	reservedTagPrefix     = "rhoai."
	escapedTagPrefix      = "rhoai.custom."
	artifactTagPrefix     = "rhoai.artifact."
	additionalTagPrefix   = "rhoai.artifacts."
	artifactRecordPrefix  = "rhoai.artifactRecord."
	adapterAliasPrefix    = "rhoai-"
	aliasSlugSeparator    = '-'
	maxAliasSlugLength    = 160
	maxTagKeyRunes        = 250
	maxTagValueRunes      = 5000
	fallbackAliasSlug     = "version"
	normalizedAliasPrefix = adapterAliasPrefix
)

// NormalizedAlias derives the deterministic adapter-owned model-version
// alias for a logical version name: "rhoai-" + at most 160 ASCII
// characters of a lowercase slug + "-" + the full 64 lowercase hex
// characters of SHA-256 of the original UTF-8 name. Runs outside
// [a-z0-9_-] collapse to single "-", separators are trimmed, and an
// empty slug falls back to "version". The result is at most 231
// characters, avoids MLflow's reserved "latest"/"v123" alias forms, and
// distinguishes punctuation, case, and Unicode collisions while the
// exact original name travels in the rhoai.versionName tag.
func NormalizedAlias(versionName string) string {
	slug := aliasSlug(versionName)
	if len(slug) > maxAliasSlugLength {
		slug = strings.Trim(slug[:maxAliasSlugLength], string(aliasSlugSeparator))
	}
	if slug == "" {
		slug = fallbackAliasSlug
	}
	digest := sha256.Sum256([]byte(versionName))
	return normalizedAliasPrefix + slug + "-" + hex.EncodeToString(digest[:])
}

// aliasSlug lowercases the name and converts every run of characters
// outside [a-z0-9_-] to a single "-", trimming leading and trailing
// separators.
func aliasSlug(versionName string) string {
	lowered := strings.ToLower(versionName)
	var builder strings.Builder
	inSeparator := false
	for _, r := range lowered {
		switch {
		case r == '_' || r == '-' || (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z'):
			builder.WriteRune(r)
			inSeparator = false
		default:
			// Collapse runs of disallowed characters ( including
			// non-ASCII runes, which occupy more than one byte but a
			// single rune position ) into one separator.
			if !inSeparator && builder.Len() > 0 {
				builder.WriteRune(aliasSlugSeparator)
			}
			inSeparator = true
		}
	}
	return strings.Trim(builder.String(), string(aliasSlugSeparator))
}
