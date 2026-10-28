package httpd

import "net/http"

// Local filesystem locations are available to exact desktop renderer origins
// and native clients, never arbitrary browser origins on app APIs.
func documentReadOriginMiddleware(origins []string) func(http.Handler) http.Handler {
	return localOriginMiddleware(origins, "/api/v1/local-documents", "Origin is not allowed to resolve local document paths")
}
