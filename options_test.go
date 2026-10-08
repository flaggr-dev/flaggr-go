package flaggr

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"unicode/utf8"
)

// TestWithHTTPClientNilKeepsDefault checks that a nil *http.Client leaves the
// default client in place instead of panicking on the first request.
func TestWithHTTPClientNilKeepsDefault(t *testing.T) {
	url, seen, closeServer := captureServer(t, map[string]interface{}{"value": true, "reason": "DEFAULT"})
	defer closeServer()

	client, err := NewClient(WithAPIURL(url), WithAPIKey("fgr_test"), WithServiceID("svc-123"), WithHTTPClient(nil))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	on, err := client.Boolean(context.Background(), "my-flag", false)
	if err != nil || !on {
		t.Fatalf("Boolean = %v, %v; want true, nil", on, err)
	}
	<-seen
}

// TestOptionsTrimWhitespace checks that values read from environment
// variables or files with surrounding whitespace, such as a trailing newline,
// still work.
func TestOptionsTrimWhitespace(t *testing.T) {
	url, seen, closeServer := captureServer(t, map[string]interface{}{"value": true, "reason": "DEFAULT"})
	defer closeServer()

	client, err := NewClient(
		WithAPIURL(" "+url+"/\n"),
		WithAPIKey("fgr_test\n"),
		WithServiceID(" svc-123\r\n"),
		WithEnvironment("staging\n"),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	if _, err := client.Boolean(context.Background(), "my-flag", false); err != nil {
		t.Fatal(err)
	}

	got := <-seen
	if got.path != "/api/flags/evaluate" {
		t.Errorf("path = %q, want /api/flags/evaluate", got.path)
	}
	if got.auth != "Bearer fgr_test" {
		t.Errorf("Authorization = %q, want %q", got.auth, "Bearer fgr_test")
	}
	if got.body["serviceId"] != "svc-123" {
		t.Errorf("serviceId = %q, want svc-123", got.body["serviceId"])
	}
	if got.body["environment"] != "staging" {
		t.Errorf("environment = %q, want staging", got.body["environment"])
	}
}

// TestNewClientRejectsBlankCredentials checks that a key or service ID made
// only of whitespace counts as missing.
func TestNewClientRejectsBlankCredentials(t *testing.T) {
	if _, err := NewClient(WithAPIKey(" \n"), WithServiceID("svc-123")); err == nil {
		t.Error("NewClient accepted a blank API key")
	}
	if _, err := NewClient(WithAPIKey("fgr_test"), WithServiceID("\t\n")); err == nil {
		t.Error("NewClient accepted a blank service ID")
	}
}

// TestErrorBodyTruncated checks that an error quotes at most the start of a
// large error page, such as a proxy's HTML 502 page.
func TestErrorBodyTruncated(t *testing.T) {
	page := "<html>" + strings.Repeat("x", 5000) + "</html>"
	ts := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(page))
	})
	defer ts.Close()

	client, err := NewClient(WithAPIURL(ts.URL), WithAPIKey("fgr_test"), WithServiceID("svc-123"))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	on, err := client.Boolean(context.Background(), "my-flag", true)
	if err == nil {
		t.Fatal("expected an error for a 502")
	}
	if !on {
		t.Error("expected the default value true")
	}
	msg := err.Error()
	if len(msg) > 600 {
		t.Errorf("error is %d bytes long, want at most 600", len(msg))
	}
	if !strings.Contains(msg, "502") || !strings.Contains(msg, "<html>xxx") {
		t.Errorf("error = %.120q..., want the status and the start of the body", msg)
	}
}

// TestShortErrorBodyUnchanged checks that a short error body is quoted whole.
func TestShortErrorBodyUnchanged(t *testing.T) {
	ts := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":"Too Many Requests","message":"IP rate limit exceeded. Please slow down.","retryAfter":45}`))
	})
	defer ts.Close()

	client, err := NewClient(WithAPIURL(ts.URL), WithAPIKey("fgr_test"), WithServiceID("svc-123"))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	_, err = client.Resolve(context.Background(), "my-flag", false)
	want := `flaggr: evaluation failed: 429 {"error":"Too Many Requests","message":"IP rate limit exceeded. Please slow down.","retryAfter":45}`
	if err == nil || err.Error() != want {
		t.Errorf("error = %v\nwant    %s", err, want)
	}
}

// TestOversizedResponseRejected checks that the client stops reading a
// response body past its size limit and reports an error.
func TestOversizedResponseRejected(t *testing.T) {
	ts := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"value":"`))
		chunk := []byte(strings.Repeat("a", 64<<10))
		for written := 0; written <= maxResponseBytes; written += len(chunk) {
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
		_, _ = w.Write([]byte(`","reason":"DEFAULT"}`))
	})
	defer ts.Close()

	client, err := NewClient(WithAPIURL(ts.URL), WithAPIKey("fgr_test"), WithServiceID("svc-123"))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	val, err := client.String(context.Background(), "my-flag", "fallback")
	if err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Errorf("error = %v, want a response size error", err)
	}
	if val != "fallback" {
		t.Errorf("value = %.20q, want the default", val)
	}
}

// TestTruncateBodyKeepsValidUTF8 checks that truncation never splits a
// multi-byte character.
func TestTruncateBodyKeepsValidUTF8(t *testing.T) {
	for _, r := range []string{"a", "\u00e9", "\u20ac", "\U0001F600"} {
		body := []byte(strings.Repeat(r, 600))
		got := truncateBody(body, maxErrorBodyBytes)
		if !utf8.ValidString(got) {
			t.Errorf("%q: truncated body is not valid UTF-8", r)
		}
		if n := len(strings.TrimSuffix(got, "... (truncated)")); n > maxErrorBodyBytes || n < maxErrorBodyBytes-3 {
			t.Errorf("%q: kept %d bytes, want %d-%d", r, n, maxErrorBodyBytes-3, maxErrorBodyBytes)
		}
	}
	// Invalid UTF-8 still keeps nearly the whole limit.
	junk := []byte(strings.Repeat("\x80", 600))
	if n := len(strings.TrimSuffix(truncateBody(junk, maxErrorBodyBytes), "... (truncated)")); n < maxErrorBodyBytes-3 {
		t.Errorf("invalid UTF-8: kept %d bytes, want at least %d", n, maxErrorBodyBytes-3)
	}
	if got := truncateBody([]byte("short"), maxErrorBodyBytes); got != "short" {
		t.Errorf("short body = %q, want it unchanged", got)
	}
}
