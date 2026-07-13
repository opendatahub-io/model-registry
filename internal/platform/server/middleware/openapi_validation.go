package middleware

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"

	"github.com/golang/glog"
)

// RouteDefinition describes a single API route with its HTTP method and URL pattern.
type RouteDefinition struct {
	Method  string
	Pattern string
}

// compiledRoute holds a pre-compiled regex for a URL pattern and the set of
// HTTP methods allowed on that pattern.
type compiledRoute struct {
	regex   *regexp.Regexp
	methods map[string]bool
	pattern string
}

// OpenAPIValidationMiddleware returns middleware that validates incoming HTTP
// requests against the supplied route definitions. It enforces:
//   - 405 Method Not Allowed for unsupported HTTP methods on known routes
//   - 400 Bad Request for missing Content-Type header on body-bearing requests
//   - 415 Unsupported Media Type for unrecognized Content-Type values
func OpenAPIValidationMiddleware(routes []RouteDefinition, next http.Handler) http.Handler {
	compiled := compileRoutes(routes)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// OPTIONS requests are handled by CORS middleware further down the
		// chain and must not be blocked here.
		if r.Method == http.MethodOptions {
			next.ServeHTTP(w, r)
			return
		}

		// Match the request path against known API route patterns.
		matched := matchRoute(compiled, r.URL.Path)
		if matched == nil {
			// No pattern matched -- let the downstream router handle it (404).
			next.ServeHTTP(w, r)
			return
		}

		// Validate HTTP method.
		if !matched.methods[r.Method] {
			allowed := sortedMethods(matched.methods)
			w.Header().Set("Allow", strings.Join(allowed, ", "))
			writeOpenAPIError(w, http.StatusMethodNotAllowed, "Method Not Allowed",
				fmt.Sprintf("Method %s is not allowed for %s. Allowed: %s",
					r.Method, r.URL.Path, strings.Join(allowed, ", ")))
			return
		}

		// For body-bearing methods, validate Content-Type header.
		if isBodyMethod(r.Method) && hasRequestBody(r) {
			ct := r.Header.Get("Content-Type")
			if ct == "" {
				writeOpenAPIError(w, http.StatusBadRequest, "Bad Request",
					"Content-Type header is required for requests with a body")
				return
			}
			if !isAcceptableContentType(ct) {
				writeOpenAPIError(w, http.StatusUnsupportedMediaType,
					"Unsupported Media Type",
					fmt.Sprintf("Content-Type %q is not supported. Expected application/json", ct))
				return
			}
		}

		next.ServeHTTP(w, r)
	})
}

// compileRoutes groups route definitions by pattern and compiles each pattern
// to a regular expression. Patterns are sorted longest-first so that more
// specific patterns match before shorter ones.
func compileRoutes(routes []RouteDefinition) []compiledRoute {
	byPattern := make(map[string]map[string]bool)
	order := make([]string, 0)

	for _, r := range routes {
		if _, exists := byPattern[r.Pattern]; !exists {
			byPattern[r.Pattern] = make(map[string]bool)
			order = append(order, r.Pattern)
		}
		byPattern[r.Pattern][r.Method] = true
	}

	// Sort longest-first so more specific patterns win.
	sort.Slice(order, func(i, j int) bool {
		return len(order[i]) > len(order[j])
	})

	compiled := make([]compiledRoute, 0, len(byPattern))
	for _, pattern := range order {
		compiled = append(compiled, compiledRoute{
			regex:   patternToRegex(pattern),
			methods: byPattern[pattern],
			pattern: pattern,
		})
	}
	return compiled
}

// matchRoute returns the first compiled route whose regex matches path.
func matchRoute(routes []compiledRoute, path string) *compiledRoute {
	for i := range routes {
		if routes[i].regex.MatchString(path) {
			return &routes[i]
		}
	}
	return nil
}

// patternToRegex converts a chi-style URL pattern to a compiled regular
// expression. It handles:
//   - {param} path parameters -> [^/]+
//   - * catch-all wildcards   -> .*
//   - literal path segments   (properly escaped)
func patternToRegex(pattern string) *regexp.Regexp {
	var buf strings.Builder
	buf.WriteString("^")

	for i := 0; i < len(pattern); {
		switch {
		case pattern[i] == '{':
			j := strings.IndexByte(pattern[i:], '}')
			if j < 0 {
				// Malformed -- treat remainder as literal.
				buf.WriteString(regexp.QuoteMeta(pattern[i:]))
				i = len(pattern)
			} else {
				buf.WriteString("[^/]+")
				i += j + 1
			}
		case pattern[i] == '*':
			buf.WriteString(".*")
			i++
		default:
			end := i
			for end < len(pattern) && pattern[end] != '{' && pattern[end] != '*' {
				end++
			}
			buf.WriteString(regexp.QuoteMeta(pattern[i:end]))
			i = end
		}
	}

	buf.WriteString("$")
	return regexp.MustCompile(buf.String())
}

// isBodyMethod returns true for HTTP methods that may carry a request body.
func isBodyMethod(method string) bool {
	return method == http.MethodPost ||
		method == http.MethodPut ||
		method == http.MethodPatch
}

// hasRequestBody returns true when the request appears to carry a body
// (non-zero Content-Length, or chunked transfer encoding).
func hasRequestBody(r *http.Request) bool {
	return r.ContentLength > 0 || r.ContentLength == -1
}

// isAcceptableContentType checks whether ct is one of the JSON media types
// accepted by the API.
func isAcceptableContentType(ct string) bool {
	mediaType := strings.TrimSpace(ct)
	if idx := strings.IndexByte(mediaType, ';'); idx != -1 {
		mediaType = strings.TrimSpace(mediaType[:idx])
	}
	mediaType = strings.ToLower(mediaType)
	return mediaType == "application/json" ||
		mediaType == "application/merge-patch+json"
}

// sortedMethods returns the allowed methods in deterministic sorted order
// (used for the Allow response header).
func sortedMethods(methods map[string]bool) []string {
	result := make([]string, 0, len(methods))
	for m := range methods {
		result = append(result, m)
	}
	sort.Strings(result)
	return result
}

// writeOpenAPIError writes a JSON error response with the given HTTP status
// code. The response body has the same shape as existing validation errors:
//
//	{"code": "<status text>", "message": "<details>"}
func writeOpenAPIError(w http.ResponseWriter, statusCode int, code, message string) {
	glog.Warningf("OpenAPI validation: %d %s: %s", statusCode, code, message)

	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	w.WriteHeader(statusCode)
	if err := json.NewEncoder(w).Encode(map[string]string{
		"code":    code,
		"message": message,
	}); err != nil {
		glog.Errorf("Error encoding JSON error response: %v", err)
	}
}
