//go:build !go1.23

package errorreport

import "net/http"

// patternNaming: before Go 1.23 requests are named by their path.
const patternNaming = false

// routePattern needs Request.Pattern (Go 1.23): before it, requests are named by path.
func routePattern(*http.Request) string { return "" }
