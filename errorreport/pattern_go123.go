//go:build go1.23

package errorreport

import (
	"net/http"
	"strings"
)

// routePattern is the ServeMux pattern that served the request ("/orders/{id}"),
// without its method, or "" when none did (Request.Pattern, Go 1.23+).
func routePattern(r *http.Request) string {
	pattern := r.Pattern
	if i := strings.IndexByte(pattern, ' '); i >= 0 {
		pattern = strings.TrimSpace(pattern[i+1:])
	}
	return pattern
}
