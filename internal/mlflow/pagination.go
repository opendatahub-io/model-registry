package mlflow

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"strconv"
	"strings"

	"github.com/kubeflow/hub/pkg/api"
)

// Page-cursor tuning: the payload version and the maximum encoded token
// length, matching the platform cursor limit.
const (
	pageCursorVersion = 1
	maxPageTokenChars = 1024
)

// PageCursor is the validated data cursor bound to one query shape: the
// kind, parent scope, sort, and filter it was created for. A token from
// a different shape is rejected rather than silently resuming at an
// unrelated position. This is data validation, not tamper resistance:
// no separately persisted signing key backs it.
type PageCursor struct {
	Version    int
	Kind       string
	ParentID   string
	OrderBy    string
	SortOrder  string
	LastValue  string
	FilterHash string
}

// pageCursorPayload is the versioned JSON carried inside the token.
type pageCursorPayload struct {
	Version    int    `json:"v"`
	Kind       string `json:"kind"`
	ParentID   string `json:"parent,omitempty"`
	OrderBy    string `json:"orderBy,omitempty"`
	SortOrder  string `json:"sortOrder,omitempty"`
	LastValue  string `json:"lastValue,omitempty"`
	FilterHash string `json:"filterHash,omitempty"`
}

// FilterHash binds a cursor to its filter query. An absent filter and
// an empty filter query are the same shape.
func FilterHash(filterQuery *string) string {
	if filterQuery == nil || strings.TrimSpace(*filterQuery) == "" {
		return "empty"
	}
	digest := fnv.New64a()
	_, _ = digest.Write([]byte(strings.TrimSpace(*filterQuery)))
	return strconv.FormatUint(digest.Sum64(), 16)
}

// EncodePageToken renders one validated cursor as standard base64 of
// `<last-id>:<base64url payload>`, so the platform's legacy cursor
// decoder can still recover the last id and value.
func EncodePageToken(lastID int32, cursor PageCursor) (string, error) {
	if lastID <= 0 {
		return "", fmt.Errorf("a page token requires a positive last id, got %d: %w",
			lastID, api.ErrBadRequest)
	}
	payload, err := json.Marshal(pageCursorPayload(cursor))
	if err != nil {
		return "", fmt.Errorf("serializing the page cursor: %w", err)
	}
	encoded := fmt.Sprintf("%d:%s", lastID,
		base64.RawURLEncoding.EncodeToString(payload))
	token := base64.StdEncoding.EncodeToString([]byte(encoded))
	if len(token) > maxPageTokenChars {
		return "", fmt.Errorf("the page token exceeds %d characters: %w",
			maxPageTokenChars, api.ErrBadRequest)
	}
	return token, nil
}

// DecodePageToken parses and validates a page token produced by
// EncodePageToken. Malformed structure, an unknown payload version, a
// nonpositive id, or a shape that does not match the current request is
// a bad request, never a silently resumed page.
func DecodePageToken(token string, shape PageCursor) (int32, PageCursor, error) {
	if len(token) > maxPageTokenChars {
		return 0, PageCursor{}, fmt.Errorf("the page token exceeds %d characters: %w",
			maxPageTokenChars, api.ErrBadRequest)
	}
	decoded, err := base64.StdEncoding.DecodeString(token)
	if err != nil {
		return 0, PageCursor{}, fmt.Errorf("the page token is not valid base64: %w", api.ErrBadRequest)
	}
	parts := strings.SplitN(string(decoded), ":", 2)
	if len(parts) != 2 {
		return 0, PageCursor{}, fmt.Errorf("the page token has no cursor payload: %w", api.ErrBadRequest)
	}
	lastID, err := strconv.ParseInt(parts[0], 10, 32)
	if err != nil || lastID <= 0 {
		return 0, PageCursor{}, fmt.Errorf("the page token carries no positive last id: %w", api.ErrBadRequest)
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return 0, PageCursor{}, fmt.Errorf("the page cursor payload is not valid base64url: %w", api.ErrBadRequest)
	}
	var parsed pageCursorPayload
	if err := json.Unmarshal(payload, &parsed); err != nil {
		return 0, PageCursor{}, fmt.Errorf("the page cursor payload is not valid JSON: %w", api.ErrBadRequest)
	}
	if parsed.Version != pageCursorVersion {
		return 0, PageCursor{}, fmt.Errorf("the page cursor payload version %d is unknown: %w",
			parsed.Version, api.ErrBadRequest)
	}
	cursor := PageCursor(parsed)
	if cursor.Kind != shape.Kind || cursor.ParentID != shape.ParentID ||
		cursor.FilterHash != shape.FilterHash || cursor.OrderBy != shape.OrderBy ||
		cursor.SortOrder != shape.SortOrder {
		return 0, PageCursor{}, fmt.Errorf("the page token belongs to a different query shape: %w",
			api.ErrBadRequest)
	}
	return int32(lastID), cursor, nil
}
