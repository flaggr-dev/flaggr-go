package errorreport

import (
	"errors"
	"net/http"
	"time"
)

// Middleware reports a panic in a handler as a fatal event with the request's
// method and path, then re-panics, so net/http logs it and closes the
// connection as it would have. http.ErrAbortHandler, the deliberate abort, is
// not reported.
func (r *Reporter) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		defer func() {
			value := recover()
			if value == nil {
				return
			}
			if err, ok := value.(error); !ok || !errors.Is(err, http.ErrAbortHandler) {
				r.CapturePanic(value, WithRequest(req), WithStatusCode(http.StatusInternalServerError))
				r.Flush(time.Second)
			}
			panic(value)
		}()
		next.ServeHTTP(w, req)
	})
}
