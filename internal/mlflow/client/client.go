// Package client provides a typed, minimal REST transport for the MLflow
// Model Registry public API ( /api/2.0/mlflow/ ).
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	apiPrefix      = "/api/2.0/mlflow/"
	defaultTimeout = 30 * time.Second
	// maxResponseBytes bounds decoded response bodies; registry responses
	// are small JSON documents.
	maxResponseBytes = 32 << 20
)

// Client is a minimal MLflow registry REST client. It is safe for
// concurrent use. It performs no retries: mutation recovery is owned by
// the service's operation journal, and native model-version creation
// must never be retried blindly.
type Client struct {
	baseURL    *url.URL
	httpClient *http.Client
}

// New builds a client for the registry at baseURL. baseURL may include a
// path prefix ( for example a route-mounted server ) which is preserved
// on every request. A nil httpClient gets a default 30-second timeout.
//
// The client never follows redirects: a redirecting registry would forward
// the configured credentials to a different host. An injected client is
// cloned before its redirect policy is changed, so the caller's client is
// left untouched.
func New(baseURL string, httpClient *http.Client) (*Client, error) {
	parsed, err := ParseRegistryURL(baseURL)
	if err != nil {
		return nil, err
	}

	clone := &http.Client{}
	if httpClient != nil {
		*clone = *httpClient
	} else {
		clone.Timeout = defaultTimeout
	}
	clone.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Client{baseURL: parsed, httpClient: clone}, nil
}

// CloseIdleConnections closes the underlying transport's idle
// connections. The startup factory's cleanup calls it after the
// service workers have joined, so a shutting-down backend releases its
// registry connection pool promptly.
func (c *Client) CloseIdleConnections() {
	c.httpClient.CloseIdleConnections()
}

// callKind classifies a registry call for failure handling.
type callKind int

const (
	// readCall is a safe, retryable request with no side effects.
	readCall callKind = iota
	// mutationCall is a write whose failure is definitive: the request is
	// idempotent ( tags, aliases, description updates ) and may be retried.
	mutationCall
	// createCall is a non-idempotent write ( object creation ). Any failure
	// that cannot prove the object was not created is marked ambiguous.
	createCall
)

// do executes one registry request. When the request is a mutation and
// the failure occurred after the request was sent ( network error after
// write, reset connection ), the returned error is marked ambiguous.
// Create requests additionally treat any HTTP 5xx response as an unknown
// outcome: a gateway may return an error after the native side already
// applied the create. Read failures surface as typed, non-ambiguous 503
// errors so the REST layer maps them to Service Unavailable rather than
// Internal Server Error.
func (c *Client) do(ctx context.Context, method, path string, query url.Values, payload any, kind callKind, result any) error {
	mutation := kind != readCall
	target := *c.baseURL
	target.Path = strings.TrimSuffix(target.Path, "/") + apiPrefix + path
	if len(query) > 0 {
		target.RawQuery = query.Encode()
	}

	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("encoding mlflow %s request: %w", path, err)
		}
		body = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(ctx, method, target.String(), body)
	if err != nil {
		return fmt.Errorf("building mlflow %s request: %w", path, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		// A transport failure of a dispatched HTTP call may have been
		// applied server-side — the client cannot observe where in the
		// round trip it failed — so mutations are marked ambiguous and
		// must never be blindly retried.
		if mutation {
			return &Error{
				Status:    http.StatusServiceUnavailable,
				Message:   fmt.Sprintf("mlflow %s request outcome unknown: %v", path, sanitizeTransportError(err)),
				Ambiguous: true,
				Cause:     err,
			}
		}
		// A read has no side effect, so its failure is definitive — but the
		// registry dependency is unavailable, which the REST layer must map
		// to 503 rather than 500.
		return &Error{
			Status:  http.StatusServiceUnavailable,
			Message: fmt.Sprintf("mlflow %s request failed: %v", path, sanitizeTransportError(err)),
			Cause:   err,
		}
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		_ = resp.Body.Close()
	}()

	limited := io.LimitReader(resp.Body, maxResponseBytes+1)
	decoded, err := io.ReadAll(limited)
	if err != nil {
		if mutation {
			return &Error{
				Status:    http.StatusServiceUnavailable,
				Message:   fmt.Sprintf("mlflow %s response unreadable (status %d): %v", path, resp.StatusCode, err),
				Ambiguous: true,
			}
		}
		return &Error{
			Status:  http.StatusServiceUnavailable,
			Message: fmt.Sprintf("mlflow %s response unreadable (status %d): %v", path, resp.StatusCode, err),
			Cause:   err,
		}
	}
	if len(decoded) > maxResponseBytes {
		if mutation {
			return &Error{
				Status: http.StatusServiceUnavailable,
				Message: fmt.Sprintf("mlflow %s success response exceeds %d bytes (status %d); outcome unknown",
					path, maxResponseBytes, resp.StatusCode),
				Ambiguous: true,
			}
		}
		return &Error{
			Status:  http.StatusServiceUnavailable,
			Message: fmt.Sprintf("mlflow %s response exceeds %d bytes", path, maxResponseBytes),
		}
	}

	// Redirects are never followed ( see New ); with ErrUseLastResponse the
	// 3xx response itself is returned here. A redirected mutation has an
	// unknown outcome: the original request was dispatched exactly once,
	// but whether the registry applied it before redirecting is not
	// observable, so it stays ambiguous for journal reconciliation.
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		if mutation {
			return &Error{
				Status:    http.StatusServiceUnavailable,
				Message:   fmt.Sprintf("mlflow %s request was redirected; outcome unknown", path),
				Ambiguous: true,
			}
		}
		return &Error{
			Status:  resp.StatusCode,
			Message: "mlflow registry returned an unexpected redirect",
		}
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		apiErr := &Error{Status: resp.StatusCode}
		var errorBody struct {
			ErrorCode string `json:"error_code"`
			Message   string `json:"message"`
		}
		if json.Unmarshal(decoded, &errorBody) == nil && validErrorCode(errorBody.ErrorCode) {
			apiErr.Code = errorBody.ErrorCode
			// The pinned server uses INVALID_PARAMETER_VALUE for an absent
			// alias. Classify only this exact, request-specific read response;
			// malformed input, auth failures and other 400s still stop writes.
			alias := query.Get("alias")
			apiErr.missingAlias = method == http.MethodGet && path == "registered-models/alias" &&
				resp.StatusCode == http.StatusBadRequest && apiErr.Code == "INVALID_PARAMETER_VALUE" &&
				alias != "" && errorBody.Message == "Registered model alias "+alias+" not found."
		}
		// A non-idempotent create answered with a server-side failure may
		// already have been applied before the error was produced ( a
		// gateway timing out after the registry committed the write ); the
		// outcome is unknown, not a definitive rejection. Definitive client
		// errors ( 400/401/403/404, duplicate codes ) remain distinguishable.
		if kind == createCall && resp.StatusCode >= 500 {
			apiErr.Ambiguous = true
		}
		// The upstream message and body are deliberately dropped: they are
		// arbitrary prose that may contain credentials or signed URLs. Only
		// the validated error code and the HTTP status survive.
		return apiErr
	}

	if result == nil {
		return nil
	}
	if err := json.Unmarshal(decoded, result); err != nil {
		if mutation {
			// A 2xx response that cannot be decoded may describe an applied
			// mutation; the outcome is unknown, not failed.
			return &Error{
				Status: http.StatusServiceUnavailable,
				Message: fmt.Sprintf("mlflow %s success response undecodable (status %d); outcome unknown",
					path, resp.StatusCode),
				Ambiguous: true,
			}
		}
		return &Error{
			Status:  http.StatusServiceUnavailable,
			Message: fmt.Sprintf("mlflow %s success response undecodable (status %d)", path, resp.StatusCode),
			Cause:   err,
		}
	}
	return nil
}

// requireMutationIdentity validates that a successful create response
// identifies the created object. A success-shaped body missing the identity
// leaves the outcome unknown and must never be retried as a fresh create.
func requireMutationIdentity(path string, identified bool) error {
	if identified {
		return nil
	}
	return &Error{
		Status:    http.StatusServiceUnavailable,
		Message:   fmt.Sprintf("mlflow %s success response did not identify the created object; outcome unknown", path),
		Ambiguous: true,
	}
}

// searchQuery encodes a SearchRequest as URL query parameters: repeated
// order_by entries and single filter, max_results, and page_token values.
// Searches are GET requests and carry no JSON body.
func searchQuery(request SearchRequest) url.Values {
	query := url.Values{}
	if request.Filter != "" {
		query.Set("filter", request.Filter)
	}
	if request.MaxResults > 0 {
		query.Set("max_results", strconv.FormatInt(int64(request.MaxResults), 10))
	}
	for _, order := range request.OrderBy {
		query.Add("order_by", order)
	}
	if request.PageToken != "" {
		query.Set("page_token", request.PageToken)
	}
	return query
}

// sanitizeTransportError keeps credentials and full URLs out of error
// strings; only the error class survives.
func sanitizeTransportError(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "request timeout"
	}
	if errors.Is(err, context.Canceled) {
		return "request canceled"
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return "network error"
	}
	return "transport error"
}

// Registered models.

// CreateRegisteredModel creates a native registered model.
func (c *Client) CreateRegisteredModel(ctx context.Context, request CreateRegisteredModelRequest) (*RegisteredModel, error) {
	var response struct {
		RegisteredModel RegisteredModel `json:"registered_model"`
	}
	err := c.do(ctx, http.MethodPost, "registered-models/create", nil, request, createCall, &response)
	if err != nil {
		return nil, err
	}
	if err := requireMutationIdentity("registered-models/create", response.RegisteredModel.Name != ""); err != nil {
		return nil, err
	}
	return &response.RegisteredModel, nil
}

// GetRegisteredModel fetches a native registered model by exact name.
func (c *Client) GetRegisteredModel(ctx context.Context, name string) (*RegisteredModel, error) {
	query := url.Values{"name": []string{name}}
	var response struct {
		RegisteredModel RegisteredModel `json:"registered_model"`
	}
	err := c.do(ctx, http.MethodGet, "registered-models/get", query, nil, readCall, &response)
	if err != nil {
		return nil, err
	}
	return &response.RegisteredModel, nil
}

// SearchRegisteredModels runs a paged registered-model search. Searches
// are GET requests with the search parameters URL-encoded as a query.
func (c *Client) SearchRegisteredModels(ctx context.Context, request SearchRequest) (*SearchRegisteredModelsResponse, error) {
	var response SearchRegisteredModelsResponse
	err := c.do(ctx, http.MethodGet, "registered-models/search", searchQuery(request), nil, readCall, &response)
	if err != nil {
		return nil, err
	}
	return &response, nil
}

// UpdateRegisteredModel changes a registered model's description.
func (c *Client) UpdateRegisteredModel(ctx context.Context, request UpdateRegisteredModelRequest) error {
	return c.do(ctx, http.MethodPatch, "registered-models/update", nil, request, mutationCall, nil)
}

// SetRegisteredModelTag sets one tag on a registered model.
func (c *Client) SetRegisteredModelTag(ctx context.Context, name, key, value string) error {
	return c.do(ctx, http.MethodPost, "registered-models/set-tag", nil, SetTagRequest{Name: name, Key: key, Value: value}, mutationCall, nil)
}

// DeleteRegisteredModelTag deletes one tag from a registered model.
func (c *Client) DeleteRegisteredModelTag(ctx context.Context, name, key string) error {
	return c.do(ctx, http.MethodDelete, "registered-models/delete-tag", nil, DeleteTagRequest{Name: name, Key: key}, mutationCall, nil)
}

// SetRegisteredModelAlias points an alias at a native version.
func (c *Client) SetRegisteredModelAlias(ctx context.Context, name, alias, version string) error {
	return c.do(ctx, http.MethodPost, "registered-models/alias", nil, SetAliasRequest{Name: name, Alias: alias, Version: version}, mutationCall, nil)
}

// GetRegisteredModelAlias returns the version an alias points at.
func (c *Client) GetRegisteredModelAlias(ctx context.Context, name, alias string) (*ModelVersion, error) {
	query := url.Values{"name": []string{name}, "alias": []string{alias}}
	var response GetAliasResponse
	err := c.do(ctx, http.MethodGet, "registered-models/alias", query, nil, readCall, &response)
	if err != nil {
		return nil, err
	}
	return &response.ModelVersion, nil
}

// Model versions.

// CreateModelVersion creates a native model version from a real source.
// The request never references a tracking run or experiment.
func (c *Client) CreateModelVersion(ctx context.Context, request CreateVersionRequest) (*ModelVersion, error) {
	var response struct {
		ModelVersion ModelVersion `json:"model_version"`
	}
	err := c.do(ctx, http.MethodPost, "model-versions/create", nil, request, createCall, &response)
	if err != nil {
		return nil, err
	}
	identified := response.ModelVersion.Name != "" && response.ModelVersion.Version != ""
	if err := requireMutationIdentity("model-versions/create", identified); err != nil {
		return nil, err
	}
	return &response.ModelVersion, nil
}

// GetModelVersion fetches a native model version by exact identity.
func (c *Client) GetModelVersion(ctx context.Context, name, version string) (*ModelVersion, error) {
	query := url.Values{"name": []string{name}, "version": []string{version}}
	var response struct {
		ModelVersion ModelVersion `json:"model_version"`
	}
	err := c.do(ctx, http.MethodGet, "model-versions/get", query, nil, readCall, &response)
	if err != nil {
		return nil, err
	}
	return &response.ModelVersion, nil
}

// SearchModelVersions runs a paged model-version search. Searches are GET
// requests with the search parameters URL-encoded as a query.
func (c *Client) SearchModelVersions(ctx context.Context, request SearchRequest) (*SearchVersionsResponse, error) {
	var response SearchVersionsResponse
	err := c.do(ctx, http.MethodGet, "model-versions/search", searchQuery(request), nil, readCall, &response)
	if err != nil {
		return nil, err
	}
	return &response, nil
}

// UpdateModelVersion changes a model version's description only.
func (c *Client) UpdateModelVersion(ctx context.Context, request UpdateVersionRequest) error {
	return c.do(ctx, http.MethodPatch, "model-versions/update", nil, request, mutationCall, nil)
}

// SetModelVersionTag sets one tag on a model version.
func (c *Client) SetModelVersionTag(ctx context.Context, name, version, key, value string) error {
	return c.do(ctx, http.MethodPost, "model-versions/set-tag", nil, SetTagRequest{Name: name, Version: version, Key: key, Value: value}, mutationCall, nil)
}

// DeleteModelVersionTag deletes one tag from a model version.
func (c *Client) DeleteModelVersionTag(ctx context.Context, name, version, key string) error {
	return c.do(ctx, http.MethodDelete, "model-versions/delete-tag", nil, DeleteTagRequest{Name: name, Version: version, Key: key}, mutationCall, nil)
}
