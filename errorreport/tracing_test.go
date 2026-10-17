package errorreport

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func spansOf(events []map[string]any) []map[string]any {
	var out []map[string]any
	for _, e := range events {
		if e["kind"] == "span" {
			out = append(out, e)
		}
	}
	return out
}

func TestMiddlewareRecordsASampledRequestSpanNamedByItsPattern(t *testing.T) {
	in, srv := newIngest(t, http.StatusAccepted)
	r := newReporter(srv, Options{TracesSampleRate: 1, Release: "api@2", ServiceID: "svc-1"})
	mux := http.NewServeMux()
	mux.HandleFunc("GET /orders/{id}", func(w http.ResponseWriter, req *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	req := httptest.NewRequest(http.MethodGet, "/orders/42?token=x", nil)
	req.Header.Set("X-Request-Id", "req-7")
	r.Middleware(mux).ServeHTTP(httptest.NewRecorder(), req)
	r.Close()

	spans := spansOf(in.events())
	if len(spans) != 1 {
		t.Fatalf("spans = %v", spans)
	}
	span := spans[0]
	data := span["data"].(map[string]any)
	// Go 1.23+ names the span by the mux pattern; before it, by the path.
	route := "/orders/42"
	if patternNaming {
		route = "/orders/{id}"
	}
	if span["name"] != "GET "+route || span["route"] != route || span["method"] != "GET" ||
		span["statusCode"] != float64(418) || span["serviceId"] != "svc-1" || span["release"] != "api@2" {
		t.Errorf("span = %v", span)
	}
	if _, ok := span["durationMs"].(float64); !ok {
		t.Errorf("durationMs missing: %v", span)
	}
	if len(data["traceId"].(string)) != 32 || len(data["spanId"].(string)) != 16 || data["op"] != "http.server" || data["requestId"] != "req-7" {
		t.Errorf("data = %v", data)
	}
	if _, ok := data["parentSpanId"]; ok {
		t.Error("a request without a traceparent is a root span")
	}
	if _, ok := data["sampleRate"]; ok {
		t.Error("rate 1 sends no sampleRate")
	}
}

func TestMiddlewareContinuesASampledCallersTraceEvenAtRateZero(t *testing.T) {
	in, srv := newIngest(t, http.StatusAccepted)
	r := newReporter(srv, Options{})
	req := httptest.NewRequest(http.MethodPost, "/pay", nil)
	req.Header.Set("traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
	r.Middleware(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.CaptureError(errors.New("declined"), WithRequest(req))
	})).ServeHTTP(httptest.NewRecorder(), req)
	r.Close()

	events := in.events()
	spans := spansOf(events)
	if len(spans) != 1 {
		t.Fatalf("spans = %v", spans)
	}
	data := spans[0]["data"].(map[string]any)
	if data["traceId"] != "4bf92f3577b34da6a3ce929d0e0e4736" || data["parentSpanId"] != "00f067aa0ba902b7" {
		t.Errorf("span data = %v", data)
	}
	var errData map[string]any
	for _, e := range events {
		if e["kind"] == "error" {
			errData = e["data"].(map[string]any)
		}
	}
	if errData["traceId"] != "4bf92f3577b34da6a3ce929d0e0e4736" || errData["spanId"] != data["spanId"] {
		t.Errorf("the error should carry the request's trace: %v", errData)
	}
}

func TestUnsampledRequestsRecordNoSpanButErrorsStillCarryTheTrace(t *testing.T) {
	in, srv := newIngest(t, http.StatusAccepted)
	r := newReporter(srv, Options{TracesSampleRate: 0})
	r.Middleware(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.CaptureError(errors.New("boom"), WithRequest(req))
	})).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	r.Close()
	events := in.events()
	if len(spansOf(events)) != 0 {
		t.Errorf("spans recorded at rate 0: %v", events)
	}
	if len(events) != 1 || events[0]["data"].(map[string]any)["traceId"] == nil {
		t.Errorf("events = %v", events)
	}
}

func TestStartSpanAndTransportContinueTheTrace(t *testing.T) {
	in, srv := newIngest(t, http.StatusAccepted)
	var gotParent string
	downstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		gotParent = req.Header.Get("traceparent")
	}))
	defer downstream.Close()
	r := newReporter(srv, Options{TracesSampleRate: 1})
	client := &http.Client{Transport: r.Transport(nil)}

	r.Middleware(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		span, ctx := r.StartSpan(req.Context(), "load cart", "db.query")
		span.SetData("rows", 3)
		call, _ := http.NewRequestWithContext(ctx, http.MethodGet, downstream.URL+"/prices", nil)
		resp, err := client.Do(call)
		if err == nil {
			resp.Body.Close()
		}
		span.End()
	})).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/cart", nil))
	r.Close()

	spans := spansOf(in.events())
	if len(spans) != 3 {
		t.Fatalf("spans = %d", len(spans))
	}
	byOp := map[string]map[string]any{}
	for _, s := range spans {
		byOp[s["data"].(map[string]any)["op"].(string)] = s["data"].(map[string]any)
	}
	server, db, client2 := byOp["http.server"], byOp["db.query"], byOp["http.client"]
	if db["parentSpanId"] != server["spanId"] || client2["parentSpanId"] != db["spanId"] || db["rows"] != float64(3) {
		t.Errorf("tree: server=%v db=%v client=%v", server, db, client2)
	}
	for _, s := range []map[string]any{db, client2} {
		if s["traceId"] != server["traceId"] {
			t.Errorf("trace ids differ: %v", s)
		}
	}
	if !strings.HasPrefix(gotParent, "00-"+server["traceId"].(string)+"-"+client2["spanId"].(string)+"-01") {
		t.Errorf("downstream traceparent = %q", gotParent)
	}
}

func TestAHandlerPanicFailsTheSpanAndTheErrorCarriesTheTrace(t *testing.T) {
	in, srv := newIngest(t, http.StatusAccepted)
	r := newReporter(srv, Options{TracesSampleRate: 1})
	func() {
		defer func() { _ = recover() }()
		r.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") })).
			ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", nil))
	}()
	r.Close()
	events := in.events()
	spans := spansOf(events)
	if len(spans) != 1 || spans[0]["statusCode"] != float64(500) || spans[0]["data"].(map[string]any)["error"] != true {
		t.Fatalf("spans = %v", spans)
	}
	for _, e := range events {
		if e["kind"] == "error" && e["data"].(map[string]any)["traceId"] != spans[0]["data"].(map[string]any)["traceId"] {
			t.Errorf("error trace = %v", e["data"])
		}
	}
}

func TestBatchesAreSplitAtTheIngestsCap(t *testing.T) {
	in, srv := newIngest(t, http.StatusAccepted)
	r := newReporter(srv, Options{TracesSampleRate: 1, FlushInterval: time.Hour})
	for i := 0; i < 250; i++ {
		span, _ := r.StartSpan(nil, "work", "task")
		span.End()
	}
	r.Close()
	in.mu.Lock()
	defer in.mu.Unlock()
	if len(in.batches) != 3 {
		t.Fatalf("batches = %d", len(in.batches))
	}
	for _, b := range in.batches {
		if len(b.Events) > maxBatch {
			t.Errorf("batch of %d", len(b.Events))
		}
	}
}

func TestParseTraceparent(t *testing.T) {
	if id, parent, sampled, ok := parseTraceparent("00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"); !ok || !sampled || id == "" || parent == "" {
		t.Error("valid sampled header")
	}
	if _, _, sampled, ok := parseTraceparent("00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-00"); !ok || sampled {
		t.Error("valid unsampled header")
	}
	for _, bad := range []string{"", "garbage", "00-00000000000000000000000000000000-00f067aa0ba902b7-01", "ff-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01", "00-4BF92F3577B34DA6A3CE929D0E0E4736-00f067aa0ba902b7-01"} {
		if _, _, _, ok := parseTraceparent(bad); ok {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestAnUnsampledCallersTraceIsSampledAtTheServicesRate(t *testing.T) {
	in, srv := newIngest(t, http.StatusAccepted)
	r := newReporter(srv, Options{TracesSampleRate: 1})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-00")
	r.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).ServeHTTP(httptest.NewRecorder(), req)
	r.Close()
	spans := spansOf(in.events())
	if len(spans) != 1 || spans[0]["data"].(map[string]any)["traceId"] != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Fatalf("spans = %v", spans)
	}
}
