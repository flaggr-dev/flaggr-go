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
//
// It also traces the request: it continues the caller's traceparent (or
// starts a trace), puts the trace on the request's context (errors captured
// WithRequest(r) or WithContext(r.Context()) carry it, StartSpan and
// Transport continue it), and, when the trace is sampled (TracesSampleRate),
// records the request as a span named by its ServeMux pattern
// ("GET /orders/{id}"), or its path when no pattern served it.
func (r *Reporter) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		span, traced := r.startRequestSpan(req)
		rec := &statusRecorder{ResponseWriter: w}
		finish := func(status int) {
			if pattern := routePattern(traced); pattern != "" {
				span.route = pattern
				span.name = traced.Method + " " + pattern
			}
			if status == 0 {
				status = http.StatusOK
			}
			span.SetStatus(status)
			span.End()
		}
		defer func() {
			value := recover()
			if value == nil {
				finish(rec.status)
				return
			}
			if err, ok := value.(error); !ok || !errors.Is(err, http.ErrAbortHandler) {
				r.CapturePanic(value, WithRequest(traced), WithStatusCode(http.StatusInternalServerError))
				finish(http.StatusInternalServerError)
				r.Flush(time.Second)
			} else {
				finish(rec.status)
			}
			panic(value)
		}()
		next.ServeHTTP(rec, traced)
	})
}
