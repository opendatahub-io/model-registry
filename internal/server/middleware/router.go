package middleware

import (
	"net/http"

	platformmw "github.com/kubeflow/hub/internal/platform/server/middleware"
	"github.com/kubeflow/hub/internal/server/openapi"
)

// WrapWithValidation wraps the auto-generated router with custom validation
// middleware. It applies, from outermost to innermost:
//  1. OpenAPI validation (method, Content-Type header checks)
//  2. Null-byte validation (query params and body)
//  3. The chi router produced by the code-generated route definitions
func WrapWithValidation(routers ...openapi.Router) http.Handler {
	// Create the auto-generated chi router.
	baseRouter := openapi.NewRouter(routers...)

	// Collect route definitions so the OpenAPI middleware knows which
	// methods are valid for each path pattern.
	var routeDefs []platformmw.RouteDefinition
	for _, r := range routers {
		for _, route := range r.OrderedRoutes() {
			routeDefs = append(routeDefs, platformmw.RouteDefinition{
				Method:  route.Method,
				Pattern: route.Pattern,
			})
		}
	}

	// Chain: OpenAPI validation -> null-byte validation -> chi router.
	withNullByteCheck := platformmw.ValidationMiddleware(baseRouter)
	return platformmw.OpenAPIValidationMiddleware(routeDefs, withNullByteCheck)
}
