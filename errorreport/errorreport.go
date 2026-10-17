// Package errorreport sends a Go program's errors and panics to Flaggr, where
// they are grouped into issues alongside the flag variants that were active
// when they happened (the console's Issues page).
//
// Events go to the Flaggr app's POST /api/ingest/server-events with a project
// API token (an SDK key is refused there). They are sent in the background,
// in batches; call Flush or Close before the program exits.
//
//	reporter := errorreport.New(errorreport.Options{
//		Token:       os.Getenv("FLAGGR_ERRORS_TOKEN"),
//		Release:     "checkout-api@2.8.1",
//		Environment: "production",
//	})
//	defer reporter.Close()
//	defer reporter.Recover() // report a panic, then re-panic
//
//	if err := placeOrder(ctx); err != nil {
//		reporter.CaptureError(err, errorreport.WithTags(map[string]string{"step": "pay"}))
//	}
//
// Flaggr groups an error by its stack (the innermost frames of your own code,
// without line numbers), or by WithFingerprint when you give one. Messages and
// stacks are cut to 2,000 characters, and Flaggr removes tokens, email
// addresses and URL credentials before storing them.
package errorreport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"reflect"
	"runtime/debug"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// DefaultAppURL is the hosted Flaggr app, which receives errors.
const DefaultAppURL = "https://flaggr.dev"

const (
	ingestPath = "/api/ingest/server-events"
	// The ingest's caps: a longer message or stack rejects the event, a longer short field too.
	maxText  = 2000
	maxShort = 500
	// The most events queued, sent per batch, and sent per minute.
	maxQueue     = 100
	maxPerMinute = 60
	// The ingest's batch cap, the most spans queued, and spans sent per minute.
	maxBatch          = 100
	maxSpanQueue      = 1000
	maxSpansPerMinute = 3000
	maxCrumbs         = 50
)

// Level is how severe an event is.
type Level string

const (
	LevelFatal   Level = "fatal"
	LevelError   Level = "error"
	LevelWarning Level = "warning"
)

// Options configure a Reporter. Only Token is required.
type Options struct {
	// Token is the project API token (fgr_...). Default: the FLAGGR_ERRORS_TOKEN
	// environment variable. Without one the Reporter sends nothing.
	Token string
	// AppURL is the Flaggr app's URL (default https://flaggr.dev, or FLAGGR_APP_URL).
	AppURL string
	// IngestURL, when set, is the full URL events are posted to instead of
	// AppURL's /api/ingest/server-events, and Token becomes optional (sent
	// only when set). It is for an ingest that takes no token, such as the
	// one Flaggr's own CLI reports its crashes to.
	IngestURL string
	// Release is your program's version, e.g. "checkout-api@2.8.1" or a commit SHA.
	Release string
	// Environment is where it runs, e.g. "production".
	Environment string
	// ServiceID files errors under one of the project's services (its id, not its
	// slug). Left empty, errors have no service; an id that isn't one of the
	// token's project's services makes Flaggr reject the event.
	ServiceID string
	// SampleRate sends this share of events, from 0 to 1. Zero means 1 (send all).
	SampleRate float64
	// BeforeSend may change an event or return nil to drop it.
	BeforeSend func(*Event) *Event
	// HTTPClient sends the batches (default: a client with a 10 s timeout).
	HTTPClient *http.Client
	// FlushInterval is how often queued events are sent (default 2 s).
	FlushInterval time.Duration
	// Disabled turns the Reporter into a no-op (e.g. for an opt-out).
	Disabled bool
	// Logf reports why sending stopped (default: none). log.Printf fits.
	Logf func(format string, args ...any)
	// TracesSampleRate records this share of requests (0 to 1) as spans for
	// Flaggr's Performance page: Middleware's request spans, StartSpan's child
	// spans and Transport's outgoing calls. Zero (the default) records none,
	// but still continues an incoming traceparent so errors carry its trace id.
	// A sampled caller's traceparent is always followed; an unsampled
	// caller's trace is sampled at this rate.
	TracesSampleRate float64
}

// Breadcrumb is something that happened before an error.
type Breadcrumb struct {
	Timestamp int64          `json:"timestamp"` // ms since the epoch
	Category  string         `json:"category"`
	Message   string         `json:"message"`
	Level     string         `json:"level,omitempty"`
	Data      map[string]any `json:"data,omitempty"`
}

// Event is one error as it will be sent; BeforeSend may change it.
type Event struct {
	Name        string
	Message     string
	Stack       string
	Level       Level
	Handled     bool
	Timestamp   time.Time
	Release     string
	Environment string
	UserID      string
	Fingerprint string
	Route       string
	Method      string
	StatusCode  int
	Tags        map[string]string
	Flags       map[string]string
	Extra       map[string]any
	Breadcrumbs []Breadcrumb
	// TraceID and SpanID tie the event to a trace (set from a traced request
	// by WithRequest or WithContext): the issue links to it.
	TraceID string
	SpanID  string
}

// CaptureOption adds to one event.
type CaptureOption func(*Event)

// WithTags adds tags (searchable labels) to the event.
func WithTags(tags map[string]string) CaptureOption {
	return func(e *Event) {
		for k, v := range tags {
			e.Tags[k] = v
		}
	}
}

// WithExtra adds context shown with the event.
func WithExtra(extra map[string]any) CaptureOption {
	return func(e *Event) {
		for k, v := range extra {
			e.Extra[k] = v
		}
	}
}

// WithFlags records the flag variants the code was running under (flag key to variant).
func WithFlags(flags map[string]string) CaptureOption {
	return func(e *Event) {
		for k, v := range flags {
			e.Flags[k] = v
		}
	}
}

// WithUser names the user the error happened to (an id, not an email address).
func WithUser(id string) CaptureOption { return func(e *Event) { e.UserID = id } }

// WithFingerprint groups the event with others under your own key instead of by its stack.
func WithFingerprint(key string) CaptureOption { return func(e *Event) { e.Fingerprint = key } }

// WithLevel sets the event's level (default error).
func WithLevel(level Level) CaptureOption { return func(e *Event) { e.Level = level } }

// WithRequest records the request's method and route (its path, without the
// query string), and its trace when Middleware traced it.
func WithRequest(r *http.Request) CaptureOption {
	return func(e *Event) {
		if r == nil {
			return
		}
		e.Method = r.Method
		if r.URL != nil {
			e.Route = r.URL.Path
		}
		WithContext(r.Context())(e)
	}
}

// WithContext ties the event to the trace in ctx (Middleware's request, or a
// StartSpan span), so the issue links to the trace.
func WithContext(ctx context.Context) CaptureOption {
	return func(e *Event) {
		if tc, ok := traceFrom(ctx); ok {
			e.TraceID, e.SpanID = tc.traceID, tc.spanID
		}
	}
}

// WithStatusCode records the response status the error led to.
func WithStatusCode(code int) CaptureOption { return func(e *Event) { e.StatusCode = code } }

// Reporter queues events and sends them in the background. Its methods are
// safe for concurrent use. The zero value is not usable; call New.
type Reporter struct {
	opts   Options
	client *http.Client
	url    string

	mu      sync.Mutex
	queue   []*Event
	sentAt  []time.Time
	tags    map[string]string
	flags   map[string]string
	userID  string
	crumbs  []Breadcrumb
	stopped bool
	// Spans waiting to be sent (already in the wire format), and when the last minute's were recorded.
	spans      []wireEvent
	spanSentAt []time.Time

	wake    chan struct{}
	flushes chan chan struct{}
	quit    chan struct{}
	done    chan struct{}
	closed  sync.Once
}

// New starts a Reporter. Close it when the program ends.
func New(opts Options) *Reporter {
	if opts.Token == "" {
		opts.Token = os.Getenv("FLAGGR_ERRORS_TOKEN")
	}
	if opts.AppURL == "" {
		opts.AppURL = os.Getenv("FLAGGR_APP_URL")
	}
	if opts.AppURL == "" {
		opts.AppURL = DefaultAppURL
	}
	if opts.FlushInterval <= 0 {
		opts.FlushInterval = 2 * time.Second
	}
	if opts.SampleRate <= 0 || opts.SampleRate > 1 {
		opts.SampleRate = 1
	}
	client := opts.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	r := &Reporter{
		opts:    opts,
		client:  client,
		url:     strings.TrimRight(strings.TrimSpace(opts.AppURL), "/") + ingestPath,
		tags:    map[string]string{},
		flags:   map[string]string{},
		wake:    make(chan struct{}, 1),
		flushes: make(chan chan struct{}),
		quit:    make(chan struct{}),
		done:    make(chan struct{}),
	}
	if opts.IngestURL != "" {
		r.url = strings.TrimSpace(opts.IngestURL)
	}
	if opts.Disabled || (opts.Token == "" && opts.IngestURL == "") {
		r.stopped = true
		close(r.done)
		return r
	}
	go r.loop()
	return r
}

// SetTag sets a tag on every later event.
func (r *Reporter) SetTag(key, value string) {
	r.mu.Lock()
	r.tags[key] = value
	r.mu.Unlock()
}

// SetUser sets the user of every later event ("" clears it).
func (r *Reporter) SetUser(id string) {
	r.mu.Lock()
	r.userID = id
	r.mu.Unlock()
}

// SetFlag records the variant a flag evaluated to; later events carry it.
func (r *Reporter) SetFlag(key, variant string) {
	r.mu.Lock()
	r.flags[key] = variant
	r.mu.Unlock()
}

// AddBreadcrumb records something that happened; the next events carry the last 50.
func (r *Reporter) AddBreadcrumb(category, message string, data map[string]any) {
	r.mu.Lock()
	r.addCrumbLocked(Breadcrumb{Timestamp: time.Now().UnixMilli(), Category: category, Message: message, Data: data})
	r.mu.Unlock()
}

func (r *Reporter) addCrumbLocked(b Breadcrumb) {
	r.crumbs = append(r.crumbs, b)
	if len(r.crumbs) > maxCrumbs {
		r.crumbs = r.crumbs[len(r.crumbs)-maxCrumbs:]
	}
}

// CaptureError reports an error your code handled. A nil error is ignored.
func (r *Reporter) CaptureError(err error, opts ...CaptureOption) {
	if err == nil {
		return
	}
	r.capture(errorName(err), err.Error(), stackOf(), true, LevelError, opts)
}

// CaptureMessage reports a message as a warning event.
func (r *Reporter) CaptureMessage(message string, opts ...CaptureOption) {
	r.capture("Message", message, stackOf(), true, LevelWarning, opts)
}

// Recover reports a panic and re-panics with the same value, so the program
// fails as it would have. Use it deferred, first thing in main and in each
// goroutine you start: defer reporter.Recover(). It waits up to 2 s for the
// report to be sent before re-panicking.
func (r *Reporter) Recover() {
	value := recover()
	if value == nil {
		return
	}
	r.CapturePanic(value)
	r.Flush(2 * time.Second)
	panic(value)
}

// CapturePanic reports a recovered panic value as a fatal, unhandled event.
// Call it in your own recover() handler; Recover does this and re-panics.
func (r *Reporter) CapturePanic(value any, opts ...CaptureOption) {
	name, message := "panic", fmt.Sprint(value)
	if err, ok := value.(error); ok {
		message = err.Error()
	}
	r.capture(name, message, panicStack(stackOf()), false, LevelFatal, opts)
}

// panicStack drops the frames above the panic: in a recover handler's stack
// those are the handlers (yours, a framework's, this package's callers), not
// where the program failed. Go prints the panic as a "panic(...)" frame
// directly above the function that panicked. A stack without one is kept.
func panicStack(stack string) string {
	lines := strings.Split(stack, "\n")
	if len(lines) < 3 {
		return stack
	}
	for i := 1; i+1 < len(lines); i += 2 {
		if strings.HasPrefix(lines[i], "panic(") {
			return strings.Join(append([]string{lines[0]}, lines[i:]...), "\n")
		}
	}
	return stack
}

func (r *Reporter) capture(name, message, stack string, handled bool, level Level, opts []CaptureOption) {
	r.mu.Lock()
	if r.stopped {
		r.mu.Unlock()
		return
	}
	now := time.Now()
	if r.opts.SampleRate < 1 && rand.Float64() >= r.opts.SampleRate {
		r.mu.Unlock()
		return
	}
	cutoff := now.Add(-time.Minute)
	for len(r.sentAt) > 0 && r.sentAt[0].Before(cutoff) {
		r.sentAt = r.sentAt[1:]
	}
	if len(r.sentAt) >= maxPerMinute {
		r.mu.Unlock()
		return
	}
	r.sentAt = append(r.sentAt, now)
	event := &Event{
		Name:        name,
		Message:     message,
		Stack:       stack,
		Level:       level,
		Handled:     handled,
		Timestamp:   now,
		Release:     r.opts.Release,
		Environment: r.opts.Environment,
		UserID:      r.userID,
		Tags:        copyStrings(r.tags),
		Flags:       copyStrings(r.flags),
		Extra:       map[string]any{},
		Breadcrumbs: append([]Breadcrumb(nil), r.crumbs...),
	}
	r.mu.Unlock()

	for _, opt := range opts {
		opt(event)
	}
	if r.opts.BeforeSend != nil {
		if event = r.opts.BeforeSend(event); event == nil {
			return
		}
	}

	r.mu.Lock()
	r.queue = append(r.queue, event)
	if len(r.queue) > maxQueue {
		r.queue = r.queue[len(r.queue)-maxQueue:]
	}
	r.addCrumbLocked(Breadcrumb{Timestamp: now.UnixMilli(), Category: "error", Level: "error", Message: cut(event.Name+": "+event.Message, 200)})
	r.mu.Unlock()
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// Flush sends what is queued and waits for it, up to timeout. It reports
// whether everything queued was sent (or dropped by a refusal) in time.
func (r *Reporter) Flush(timeout time.Duration) bool {
	done := make(chan struct{})
	select {
	case r.flushes <- done:
	case <-r.done:
		return true
	case <-time.After(timeout):
		return false
	}
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}

// Close sends what is queued (waiting up to 2 s) and stops the Reporter.
// Later captures are dropped; Flush after Close returns at once.
func (r *Reporter) Close() {
	r.closed.Do(func() {
		r.Flush(2 * time.Second)
		close(r.quit)
		<-r.done
	})
}

func (r *Reporter) loop() {
	ticker := time.NewTicker(r.opts.FlushInterval)
	defer ticker.Stop()
	defer close(r.done)
	for {
		select {
		case <-r.quit:
			r.mu.Lock()
			r.stopped = true
			r.mu.Unlock()
			return
		case <-ticker.C:
			r.sendQueued()
		case <-r.wake:
			// Batch what arrives together; a burst becomes one request.
			time.Sleep(50 * time.Millisecond)
			r.sendQueued()
		case reply := <-r.flushes:
			r.sendQueued()
			close(reply)
		}
	}
}

func (r *Reporter) sendQueued() {
	r.mu.Lock()
	events := r.queue
	spans := r.spans
	r.queue, r.spans = nil, nil
	stopped := r.stopped
	r.mu.Unlock()
	if stopped || len(events)+len(spans) == 0 {
		return
	}
	all := append(wireEvents(events, r.opts.ServiceID), spans...)
	// The ingest takes at most maxBatch events a request.
	for start := 0; start < len(all); start += maxBatch {
		end := start + maxBatch
		if end > len(all) {
			end = len(all)
		}
		if !r.post(all[start:end]) {
			return
		}
	}
}

// post sends one batch; false when sending should stop (refused, or the ingest isn't there).
func (r *Reporter) post(batch []wireEvent) bool {
	body, err := json.Marshal(map[string]any{"events": batch})
	if err != nil {
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.url, bytes.NewReader(body))
	if err != nil {
		return true
	}
	req.Header.Set("Content-Type", "application/json")
	if r.opts.Token != "" {
		req.Header.Set("Authorization", "Bearer "+r.opts.Token)
	}
	req.Header.Set("User-Agent", "flaggr-go-errorreport")
	resp, err := r.client.Do(req)
	if err != nil {
		return true // offline: these events are lost, as a crash reporter's are
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		r.stop(fmt.Sprintf("flaggr errorreport: %s refused the token (%d); it needs a project API token, not an SDK key. No more errors are sent.", r.url, resp.StatusCode))
		return false
	case http.StatusNotFound, http.StatusMethodNotAllowed:
		r.stop(fmt.Sprintf("flaggr errorreport: %s doesn't take errors (%d); set Options.AppURL to the Flaggr app's URL. No more errors are sent.", r.url, resp.StatusCode))
		return false
	}
	return true
}

func (r *Reporter) stop(reason string) {
	r.mu.Lock()
	r.stopped = true
	r.queue, r.spans = nil, nil
	r.mu.Unlock()
	if r.opts.Logf != nil {
		r.opts.Logf("%s", reason)
	}
}

type wireEvent struct {
	Kind        string         `json:"kind"`
	ServiceID   string         `json:"serviceId,omitempty"`
	Environment string         `json:"environment,omitempty"`
	Fingerprint string         `json:"fingerprint,omitempty"`
	Name        string         `json:"name,omitempty"`
	Message     string         `json:"message,omitempty"`
	Stack       string         `json:"stack,omitempty"`
	Route       string         `json:"route,omitempty"`
	Method      string         `json:"method,omitempty"`
	StatusCode  int            `json:"statusCode,omitempty"`
	DurationMs  *int           `json:"durationMs,omitempty"`
	Release     string         `json:"release,omitempty"`
	Timestamp   string         `json:"timestamp"`
	Data        map[string]any `json:"data"`
}

func wireEvents(events []*Event, serviceID string) []wireEvent {
	out := make([]wireEvent, 0, len(events))
	for _, e := range events {
		data := map[string]any{}
		// Extra context first: the fields Flaggr reads win over a key of the same name.
		for k, v := range e.Extra {
			data[k] = v
		}
		data["handled"] = e.Handled
		data["level"] = string(e.Level)
		data["sdk"] = "flaggr-go"
		if e.UserID != "" {
			data["userId"] = cut(e.UserID, maxShort)
		}
		if len(e.Flags) > 0 {
			data["flags"] = e.Flags
		}
		if len(e.Tags) > 0 {
			data["tags"] = e.Tags
		}
		if len(e.Breadcrumbs) > 0 {
			data["breadcrumbs"] = e.Breadcrumbs
		}
		if e.TraceID != "" {
			data["traceId"] = e.TraceID
			if e.SpanID != "" {
				data["spanId"] = e.SpanID
			}
		}
		fingerprint := ""
		if e.Fingerprint != "" {
			fingerprint = cut("custom_"+e.Fingerprint, maxShort)
		}
		out = append(out, wireEvent{
			Kind:        "error",
			ServiceID:   serviceID,
			Environment: cut(e.Environment, maxShort),
			Fingerprint: fingerprint,
			Name:        cut(e.Name, maxShort),
			Message:     cut(e.Message, maxText),
			Stack:       cut(e.Stack, maxText),
			Route:       cut(e.Route, maxShort),
			Method:      cut(e.Method, maxShort),
			StatusCode:  e.StatusCode,
			Release:     cut(e.Release, maxShort),
			Timestamp:   e.Timestamp.UTC().Format(time.RFC3339Nano),
			Data:        data,
		})
	}
	return out
}

// errorName is the error's concrete type, unwrapped past fmt.Errorf's wrappers
// ("*fs.PathError", "*url.Error"), so one kind of failure groups together.
func errorName(err error) string {
	for {
		t := reflect.TypeOf(err).String()
		if t != "*fmt.wrapError" && t != "*fmt.wrapErrors" && t != "*errors.joinError" {
			return t
		}
		next := errors.Unwrap(err)
		if next == nil {
			return t
		}
		err = next
	}
}

// stackOf is the calling goroutine's stack without this package's frames (and
// runtime/debug.Stack's), so the first frame is the code that reported.
func stackOf() string {
	lines := strings.Split(strings.TrimRight(string(debug.Stack()), "\n"), "\n")
	if len(lines) == 0 {
		return ""
	}
	out := []string{lines[0]} // "goroutine N [running]:"
	for i := 1; i+1 < len(lines); i += 2 {
		fn := lines[i]
		if strings.HasPrefix(fn, "runtime/debug.Stack(") ||
			strings.Contains(fn, "flaggr-go/errorreport.(*Reporter)") ||
			strings.Contains(fn, "flaggr-go/errorreport.stackOf(") {
			continue
		}
		out = append(out, fn, lines[i+1])
	}
	return strings.Join(out, "\n")
}

func copyStrings(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// cut keeps at most max bytes, never splitting a UTF-8 character.
func cut(s string, max int) string {
	if len(s) <= max {
		return s
	}
	s = s[:max]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}
