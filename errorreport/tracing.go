package errorreport

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	mrand "math/rand"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Tracing: W3C trace context (traceparent) propagation and spans for
// Flaggr's Performance page. A trace is sampled once, where it starts
// (TracesSampleRate, or the caller's traceparent flag), and every span of it
// follows that decision. Spans are sent as events of kind "span" with the
// trace in data (traceId, spanId, parentSpanId, op, requestId, sampleRate),
// the conventions Flaggr reads.

type traceKey struct{}

type traceContext struct {
	traceID string
	spanID  string
	sampled bool
	// localDecision: this process sampled the trace (by TracesSampleRate), not a caller's traceparent.
	localDecision bool
}

func traceFrom(ctx context.Context) (traceContext, bool) {
	if ctx == nil {
		return traceContext{}, false
	}
	tc, ok := ctx.Value(traceKey{}).(traceContext)
	return tc, ok
}

func randomHex(bytes int) string {
	b := make([]byte, bytes)
	if _, err := rand.Read(b); err != nil {
		for i := range b {
			b[i] = byte(mrand.Intn(256))
		}
	}
	return hex.EncodeToString(b)
}

func isHex(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return strings.Trim(s, "0") != ""
}

// parseTraceparent reads a W3C traceparent header ("00-<32 hex>-<16 hex>-<2 hex>").
func parseTraceparent(header string) (traceID, parentID string, sampled, ok bool) {
	parts := strings.Split(strings.TrimSpace(header), "-")
	if len(parts) < 4 || len(parts[0]) != 2 || parts[0] == "ff" || !isHex(parts[1], 32) || !isHex(parts[2], 16) || len(parts[3]) != 2 {
		return "", "", false, false
	}
	flags, err := hex.DecodeString(parts[3])
	if err != nil {
		return "", "", false, false
	}
	return parts[1], parts[2], flags[0]&1 == 1, true
}

func traceparent(tc traceContext) string {
	flags := "00"
	if tc.sampled {
		flags = "01"
	}
	return "00-" + tc.traceID + "-" + tc.spanID + "-" + flags
}

// Span is one timed operation of a trace. End it once; a span of an unsampled
// trace records nothing.
type Span struct {
	r        *Reporter
	ctx      traceContext
	parentID string
	name     string
	op       string
	start    time.Time

	mu     sync.Mutex
	ended  bool
	status int
	failed bool
	method string
	route  string
	data   map[string]any
}

func (r *Reporter) newSpan(tc traceContext, parentID, name, op string) *Span {
	return &Span{r: r, ctx: tc, parentID: parentID, name: name, op: op, start: time.Now(), data: map[string]any{}}
}

// StartSpan starts a child span of the trace in ctx (a request Middleware
// traced, or another span) and returns it with a context carrying it. Without
// a trace in ctx it starts a new one, sampled by TracesSampleRate.
func (r *Reporter) StartSpan(ctx context.Context, name, op string) (*Span, context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	parent, ok := traceFrom(ctx)
	tc := traceContext{traceID: parent.traceID, spanID: randomHex(8), sampled: parent.sampled, localDecision: parent.localDecision}
	if !ok {
		tc = traceContext{traceID: randomHex(16), spanID: tc.spanID, sampled: r.sampleTrace(), localDecision: true}
	}
	span := r.newSpan(tc, parent.spanID, name, op)
	return span, context.WithValue(ctx, traceKey{}, tc)
}

// TraceID is the span's trace id.
func (s *Span) TraceID() string { return s.ctx.traceID }

// SetData adds an attribute shown with the span.
func (s *Span) SetData(key string, value any) {
	s.mu.Lock()
	s.data[key] = value
	s.mu.Unlock()
}

// SetError marks the span failed (err may be nil to mark it without one).
func (s *Span) SetError(err error) {
	s.mu.Lock()
	s.failed = true
	if err != nil {
		s.data["error.message"] = cut(err.Error(), maxShort)
	}
	s.mu.Unlock()
}

// SetStatus records an HTTP status; 500 and above mark the span failed.
func (s *Span) SetStatus(code int) {
	s.mu.Lock()
	s.status = code
	if code >= 500 {
		s.failed = true
	}
	s.mu.Unlock()
}

// End records the span (once) when its trace is sampled.
func (s *Span) End() {
	s.mu.Lock()
	if s.ended {
		s.mu.Unlock()
		return
	}
	s.ended = true
	duration := int(time.Since(s.start).Milliseconds())
	data := map[string]any{}
	for k, v := range s.data {
		data[k] = v
	}
	status, failed, method, route := s.status, s.failed, s.method, s.route
	s.mu.Unlock()
	if !s.ctx.sampled {
		return
	}
	data["traceId"] = s.ctx.traceID
	data["spanId"] = s.ctx.spanID
	if s.parentID != "" {
		data["parentSpanId"] = s.parentID
	}
	if s.op != "" {
		data["op"] = s.op
	}
	if failed {
		data["error"] = true
	}
	// The rate this trace survived, so Flaggr can estimate request counts (a
	// caller's decision has an unknown rate: none is sent, each span counts once).
	if rate := s.r.opts.TracesSampleRate; s.ctx.localDecision && rate > 0 && rate < 1 {
		data["sampleRate"] = rate
	}
	data["sdk"] = "flaggr-go"
	s.r.recordSpan(wireEvent{
		Kind:        "span",
		ServiceID:   s.r.opts.ServiceID,
		Environment: cut(s.r.opts.Environment, maxShort),
		Name:        cut(s.name, maxShort),
		Route:       cut(route, maxShort),
		Method:      cut(method, maxShort),
		StatusCode:  status,
		DurationMs:  &duration,
		Release:     cut(s.r.opts.Release, maxShort),
		Timestamp:   s.start.UTC().Format(time.RFC3339Nano),
		Data:        data,
	})
}

func (r *Reporter) sampleTrace() bool {
	rate := r.opts.TracesSampleRate
	return rate > 0 && (rate >= 1 || mrand.Float64() < rate)
}

func (r *Reporter) recordSpan(span wireEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopped {
		return
	}
	now := time.Now()
	cutoff := now.Add(-time.Minute)
	for len(r.spanSentAt) > 0 && r.spanSentAt[0].Before(cutoff) {
		r.spanSentAt = r.spanSentAt[1:]
	}
	if len(r.spanSentAt) >= maxSpansPerMinute {
		return
	}
	r.spanSentAt = append(r.spanSentAt, now)
	r.spans = append(r.spans, span)
	if len(r.spans) > maxSpanQueue {
		r.spans = r.spans[len(r.spans)-maxSpanQueue:]
	}
}

// startRequestSpan continues the request's traceparent (or starts a trace)
// and returns the request span and a request whose context carries it.
func (r *Reporter) startRequestSpan(req *http.Request) (*Span, *http.Request) {
	tc := traceContext{spanID: randomHex(8)}
	parentID := ""
	if traceID, parent, sampled, ok := parseTraceparent(req.Header.Get("traceparent")); ok {
		tc.traceID, parentID, tc.sampled = traceID, parent, sampled
	} else {
		tc.traceID, tc.sampled, tc.localDecision = randomHex(16), r.sampleTrace(), true
	}
	span := r.newSpan(tc, parentID, req.Method+" "+req.URL.Path, "http.server")
	span.method = req.Method
	span.route = req.URL.Path
	if id := req.Header.Get("X-Request-Id"); id != "" {
		span.data["requestId"] = cut(id, maxShort)
	}
	return span, req.WithContext(context.WithValue(req.Context(), traceKey{}, tc))
}

// statusRecorder remembers the status a handler wrote.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (w *statusRecorder) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusRecorder) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach the underlying writer (Flush, Hijack).
func (w *statusRecorder) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// Transport wraps an http.RoundTripper (nil for http.DefaultTransport): each
// request whose context carries a trace gets a traceparent header, so the
// service it calls continues the trace, and is recorded as an "http.client"
// span when the trace is sampled.
func (r *Reporter) Transport(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return roundTripper(func(req *http.Request) (*http.Response, error) {
		parent, ok := traceFrom(req.Context())
		if !ok {
			return base.RoundTrip(req)
		}
		span := r.newSpan(traceContext{traceID: parent.traceID, spanID: randomHex(8), sampled: parent.sampled, localDecision: parent.localDecision}, parent.spanID, req.Method+" "+req.URL.Host+req.URL.Path, "http.client")
		span.method = req.Method
		span.route = req.URL.Host + req.URL.Path
		out := req.Clone(req.Context())
		out.Header.Set("traceparent", traceparent(span.ctx))
		resp, err := base.RoundTrip(out)
		if err != nil {
			span.SetError(err)
		} else {
			span.SetStatus(resp.StatusCode)
		}
		span.End()
		return resp, err
	})
}

type roundTripper func(*http.Request) (*http.Response, error)

func (f roundTripper) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }
