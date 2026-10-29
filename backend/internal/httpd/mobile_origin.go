package httpd

import "net/http"

// Mobile controls disclose pairing credentials and require exact desktop origins.
func mobileControlOriginMiddleware(origins []string) func(http.Handler) http.Handler {
	return localOriginMiddleware(origins, "/api/v1/mobile", "Origin is not allowed to manage mobile access")
}
