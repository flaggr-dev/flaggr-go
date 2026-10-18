package errorreport

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

// ingest is a stand-in for Flaggr's POST /api/ingest/server-events.
type ingest struct {
	mu      sync.Mutex
	status  int
	batches []struct {
		Auth   string
		Events []map[string]any
	}
}

func newIngest(t *testing.T, status int) (*ingest, *httptest.Server) {
	t.Helper()
	in := &ingest{status: status}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/ingest/server-events" {
			http.NotFound(w, r)
			return
		}
		var body struct {
			Events []map[string]any `json:"events"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode: %v", err)
		}
		in.mu.Lock()
		in.batches = append(in.batches, struct {
			Auth   string
			Events []map[string]any
		}{r.Header.Get("Authorization"), body.Events})
		in.mu.Unlock()
		w.WriteHeader(in.status)
	}))
	t.Cleanup(srv.Close)
	return in, srv
}

func (in *ingest) events() []map[string]any {
	in.mu.Lock()
	defer in.mu.Unlock()
	var out []map[string]any
	for _, b := range in.batches {
		out = append(out, b.Events...)
	}
	return out
}

func newReporter(srv *httptest.Server, opts Options) *Reporter {
	opts.AppURL = srv.URL
	if opts.Token == "" {
		opts.Token = "fgr_project_token"
	}
	return New(opts)
}

func TestCaptureErrorSendsTheEventWithItsContext(t *testing.T) {
	in, srv := newIngest(t, http.StatusAccepted)
	r := newReporter(srv, Options{Release: "api@1.2.3", Environment: "production", ServiceID: "svc-1"})
	defer r.Close()
	r.SetFlag("coupon-engine", "v2")
	r.SetTag("region", "syd")
	r.SetUser("user-7")
	r.AddBreadcrumb("db", "SELECT coupons", nil)

	_, statErr := os.Stat("/definitely/not/here")
	err := fmt.Errorf("load coupons: %w", statErr)
	r.CaptureError(err, WithExtra(map[string]any{"orderId": "o-1", "handled": "not overwritten"}), WithFingerprint("coupons"))
	if !r.Flush(2 * time.Second) {
		t.Fatal("flush timed out")
	}

	in.mu.Lock()
	auth := in.batches[0].Auth
	in.mu.Unlock()
	if auth != "Bearer fgr_project_token" {
		t.Errorf("Authorization = %q", auth)
	}
	events := in.events()
	if len(events) != 1 {
		t.Fatalf("events = %d", len(events))
	}
	e := events[0]
	want := map[string]any{
		"kind": "error", "serviceId": "svc-1", "environment": "production", "release": "api@1.2.3",
		"name": "*fs.PathError", "fingerprint": "custom_coupons",
	}
	for k, v := range want {
		if e[k] != v {
			t.Errorf("%s = %v, want %v", k, e[k], v)
		}
	}
	if !strings.Contains(e["message"].(string), "load coupons") {
		t.Errorf("message = %v", e["message"])
	}
	if _, err := time.Parse(time.RFC3339Nano, e["timestamp"].(string)); err != nil {
		t.Errorf("timestamp: %v", err)
	}
	data := e["data"].(map[string]any)
	if data["handled"] != true || data["level"] != "error" || data["userId"] != "user-7" || data["orderId"] != "o-1" {
		t.Errorf("data = %v", data)
	}
	if data["flags"].(map[string]any)["coupon-engine"] != "v2" || data["tags"].(map[string]any)["region"] != "syd" {
		t.Errorf("flags/tags = %v %v", data["flags"], data["tags"])
	}
	crumbs := data["breadcrumbs"].([]any)
	if len(crumbs) != 1 || crumbs[0].(map[string]any)["message"] != "SELECT coupons" {
		t.Errorf("breadcrumbs = %v", crumbs)
	}
	// The stack starts at the code that reported, not inside this package.
	stack := e["stack"].(string)
	if !strings.HasPrefix(stack, "goroutine ") || strings.Contains(stack, "errorreport.(*Reporter)") || !strings.Contains(stack, "TestCaptureErrorSendsTheEventWithItsContext") {
		t.Errorf("stack = %s", stack)
	}
}

func TestRecoverReportsThePanicAndRepanics(t *testing.T) {
	in, srv := newIngest(t, http.StatusAccepted)
	r := newReporter(srv, Options{})
	defer r.Close()

	repanicked := func() (value any) {
		defer func() { value = recover() }()
		func() {
			defer r.Recover()
			var m map[string]int
			m["boom"] = 1 // assignment to entry in nil map
		}()
		return nil
	}()
	if repanicked == nil {
		t.Fatal("Recover swallowed the panic")
	}
	events := in.events()
	if len(events) != 1 {
		t.Fatalf("events = %d", len(events))
	}
	e := events[0]
	data := e["data"].(map[string]any)
	if e["name"] != "panic" || !strings.Contains(e["message"].(string), "nil map") || data["level"] != "fatal" || data["handled"] != false {
		t.Errorf("event = %v", e)
	}
}

func TestMiddlewareReportsHandlerPanicsWithTheRequest(t *testing.T) {
	in, srv := newIngest(t, http.StatusAccepted)
	r := newReporter(srv, Options{})
	defer r.Close()
	handler := r.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("checkout exploded") }))

	func() {
		defer func() { _ = recover() }()
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/orders?token=secret", nil))
	}()
	func() {
		defer func() { _ = recover() }()
		abort := r.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic(http.ErrAbortHandler) }))
		abort.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	}()
	r.Flush(2 * time.Second)

	events := in.events()
	if len(events) != 1 {
		t.Fatalf("events = %d (the abort must not be reported)", len(events))
	}
	if events[0]["method"] != "POST" || events[0]["route"] != "/orders" || events[0]["statusCode"] != float64(500) {
		t.Errorf("event = %v", events[0])
	}
}

func TestARefusedTokenStopsSending(t *testing.T) {
	in, srv := newIngest(t, http.StatusForbidden)
	var logged []string
	r := newReporter(srv, Options{Logf: func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }})
	defer r.Close()
	r.CaptureError(errors.New("one"))
	r.Flush(2 * time.Second)
	r.CaptureError(errors.New("two"))
	r.Flush(2 * time.Second)
	if n := len(in.events()); n != 1 {
		t.Errorf("events = %d, want 1", n)
	}
	if len(logged) != 1 || !strings.Contains(logged[0], "project API token") {
		t.Errorf("logged = %v", logged)
	}
}

func TestDisabledOrTokenlessReportersSendNothing(t *testing.T) {
	in, srv := newIngest(t, http.StatusAccepted)
	t.Setenv("FLAGGR_ERRORS_TOKEN", "")
	for _, r := range []*Reporter{
		New(Options{AppURL: srv.URL}),
		newReporter(srv, Options{Disabled: true}),
	} {
		r.CaptureError(errors.New("x"))
		if !r.Flush(time.Second) {
			t.Error("flush on a stopped reporter should return at once")
		}
		r.Close()
	}
	if n := len(in.events()); n != 0 {
		t.Errorf("events = %d", n)
	}
}

func TestBeforeSendAndSampling(t *testing.T) {
	in, srv := newIngest(t, http.StatusAccepted)
	r := newReporter(srv, Options{BeforeSend: func(e *Event) *Event {
		if strings.Contains(e.Message, "secret") {
			return nil
		}
		e.Tags["edited"] = "yes"
		return e
	}})
	r.CaptureError(errors.New("leaked secret"))
	r.CaptureError(errors.New("kept"))
	r.Close()
	events := in.events()
	if len(events) != 1 || events[0]["message"] != "kept" || events[0]["data"].(map[string]any)["tags"].(map[string]any)["edited"] != "yes" {
		t.Errorf("events = %v", events)
	}
}

func TestFlushAfterCloseReturns(t *testing.T) {
	_, srv := newIngest(t, http.StatusAccepted)
	r := newReporter(srv, Options{})
	r.Close()
	r.Close()
	r.CaptureError(errors.New("after close"))
	if !r.Flush(time.Second) {
		t.Error("Flush after Close should return at once")
	}
}

func TestCutKeepsValidUTF8UnderTheCap(t *testing.T) {
	s := strings.Repeat("é", 1500) // 3000 bytes
	got := cut(s, maxText)
	if len(got) > maxText || !utf8.ValidString(got) || len(got) != 2000 {
		t.Errorf("len = %d valid = %v", len(got), utf8.ValidString(got))
	}
	if cut("short", 10) != "short" {
		t.Error("short strings are kept")
	}
}

func TestErrorNameUnwrapsFmtWrappers(t *testing.T) {
	err := fmt.Errorf("outer: %w", fmt.Errorf("inner: %w", &fs.PathError{Op: "open", Path: "x", Err: fs.ErrNotExist}))
	if got := errorName(err); got != "*fs.PathError" {
		t.Errorf("errorName = %q", got)
	}
	if got := errorName(errors.New("plain")); got != "*errors.errorString" {
		t.Errorf("errorName = %q", got)
	}
}

func TestIngestURLSendsWithoutAToken(t *testing.T) {
	in, srv := newIngest(t, http.StatusAccepted)
	t.Setenv("FLAGGR_ERRORS_TOKEN", "")
	r := New(Options{IngestURL: srv.URL + "/api/ingest/server-events"})
	r.CaptureError(errors.New("anonymous"))
	r.Close()
	in.mu.Lock()
	defer in.mu.Unlock()
	if len(in.batches) != 1 || in.batches[0].Auth != "" {
		t.Errorf("batches = %+v", in.batches)
	}
}

// A panic recovered in a handler that reports it from a helper (as a CLI's
// crash guard does) is attributed to the function that panicked, not to the
// handler.
func reportFromHelper(r *Reporter, value any) { r.CapturePanic(value) }

func panicsInHere() { var m map[string]int; m["x"] = 1 }

func TestCapturePanicStartsAtThePanic(t *testing.T) {
	in, srv := newIngest(t, http.StatusAccepted)
	r := newReporter(srv, Options{})
	func() {
		defer func() {
			if v := recover(); v != nil {
				reportFromHelper(r, v)
			}
		}()
		panicsInHere()
	}()
	r.Close()
	stack := in.events()[0]["stack"].(string)
	lines := strings.Split(stack, "\n")
	if !strings.HasPrefix(lines[1], "panic(") || !strings.Contains(lines[3], "panicsInHere") {
		t.Errorf("stack = %s", stack)
	}
	if strings.Contains(stack, "reportFromHelper") {
		t.Errorf("the handler's frames should be gone: %s", stack)
	}
}
