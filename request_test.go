package flaggr

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"sync"
	"testing"
)

// capturedRequest is what the test server saw of one evaluation request.
type capturedRequest struct {
	method      string
	path        string
	auth        string
	contentType string
	body        map[string]interface{}
}

// captureServer answers every request with response and sends what it
// received on the returned channel.
func captureServer(t *testing.T, response map[string]interface{}) (string, <-chan capturedRequest, func()) {
	t.Helper()
	seen := make(chan capturedRequest, 1)
	ts := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		c := capturedRequest{
			method:      r.Method,
			path:        r.URL.Path,
			auth:        r.Header.Get("Authorization"),
			contentType: r.Header.Get("Content-Type"),
		}
		if err := json.NewDecoder(r.Body).Decode(&c.body); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		seen <- c
		respondJSON(w, response)
	})
	return ts.URL, seen, ts.Close
}

// TestEvaluateRequestContract pins the request to the shape Flaggr serves:
// POST {apiURL}/api/flags/evaluate with a Bearer token and a JSON body naming
// the flag, service and environment ("production" unless configured).
func TestEvaluateRequestContract(t *testing.T) {
	url, seen, closeServer := captureServer(t, map[string]interface{}{
		"flagKey": "checkout-flow",
		"value":   "v2",
		"reason":  "TARGETING_MATCH",
		"variant": "variant-a",
	})
	defer closeServer()

	client, err := NewClient(WithAPIURL(url), WithAPIKey("fgr_test"), WithServiceID("svc-123"))
	if err != nil {
		t.Fatal(err)
	}

	val, err := client.String(context.Background(), "checkout-flow", "v1", EvalContext{
		TargetingKey: "user-1",
		Attributes:   map[string]interface{}{"country": "AU", "seats": 5, "beta": true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if val != "v2" {
		t.Errorf("value = %q, want v2", val)
	}

	got := <-seen
	if got.method != http.MethodPost {
		t.Errorf("method = %s, want POST", got.method)
	}
	if got.path != "/api/flags/evaluate" {
		t.Errorf("path = %s, want /api/flags/evaluate", got.path)
	}
	if got.auth != "Bearer fgr_test" {
		t.Errorf("Authorization = %q, want %q", got.auth, "Bearer fgr_test")
	}
	if got.contentType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got.contentType)
	}

	want := map[string]interface{}{
		"flagKey":      "checkout-flow",
		"serviceId":    "svc-123",
		"environment":  "production",
		"defaultValue": "v1",
		"context": map[string]interface{}{
			"targetingKey": "user-1",
			"country":      "AU",
			"seats":        float64(5),
			"beta":         true,
		},
	}
	if !reflect.DeepEqual(got.body, want) {
		t.Errorf("body = %#v\nwant   %#v", got.body, want)
	}
}

func TestWithEnvironmentIsSent(t *testing.T) {
	url, seen, closeServer := captureServer(t, map[string]interface{}{"value": true, "reason": "DEFAULT"})
	defer closeServer()

	client, err := NewClient(WithAPIURL(url), WithAPIKey("fgr_test"), WithServiceID("svc-123"), WithEnvironment("staging"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Boolean(context.Background(), "my-flag", false); err != nil {
		t.Fatal(err)
	}

	got := <-seen
	if got.body["environment"] != "staging" {
		t.Errorf("environment = %v, want staging", got.body["environment"])
	}
	if _, ok := got.body["context"]; ok {
		t.Errorf("context sent without an EvalContext: %v", got.body["context"])
	}
}

func TestObjectAndResolveDecodeResponse(t *testing.T) {
	url, seen, closeServer := captureServer(t, map[string]interface{}{
		"flagKey": "banner",
		"value":   map[string]interface{}{"text": "hello", "dismissible": true},
		"reason":  "DEFAULT",
		"variant": "on",
	})
	defer closeServer()

	client, err := NewClient(WithAPIURL(url), WithAPIKey("fgr_test"), WithServiceID("svc-123"))
	if err != nil {
		t.Fatal(err)
	}

	obj, err := client.Object(context.Background(), "banner", map[string]interface{}{"text": ""})
	if err != nil {
		t.Fatal(err)
	}
	<-seen
	if obj["text"] != "hello" || obj["dismissible"] != true {
		t.Errorf("object = %#v", obj)
	}

	detail, err := client.Resolve(context.Background(), "banner", nil)
	if err != nil {
		t.Fatal(err)
	}
	<-seen
	if detail.Reason != "DEFAULT" || detail.Variant != "on" {
		t.Errorf("detail = %+v", detail)
	}
}

// TestNotFoundReturnsDefault covers both answers Flaggr gives for a flag that
// doesn't exist: the default value echoed back, or no value and an error code.
func TestNotFoundReturnsDefault(t *testing.T) {
	for name, response := range map[string]map[string]interface{}{
		"default echoed": {"value": true, "reason": "NOT_FOUND"},
		"no value":       {"value": nil, "reason": "FLAG_NOT_FOUND", "errorCode": "FLAG_NOT_FOUND"},
	} {
		t.Run(name, func(t *testing.T) {
			url, seen, closeServer := captureServer(t, response)
			defer closeServer()

			client, err := NewClient(WithAPIURL(url), WithAPIKey("fgr_test"), WithServiceID("svc-123"))
			if err != nil {
				t.Fatal(err)
			}
			val, err := client.Boolean(context.Background(), "missing", true)
			<-seen
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !val {
				t.Error("expected the default value true")
			}
		})
	}
}

// TestLocalEvaluationConcurrent exercises the cache from many goroutines; run
// it with -race.
func TestLocalEvaluationConcurrent(t *testing.T) {
	client, err := NewClient(WithAPIKey("fgr_test"), WithServiceID("svc-123"), WithLocalEvaluation(true))
	if err != nil {
		t.Fatal(err)
	}
	client.SetFlag(CachedFlag{
		Key:          "dark-mode",
		Enabled:      true,
		DefaultValue: false,
		Overrides:    []CachedOverride{{Identifiers: []string{"user-1"}, Value: true}},
	})

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				on, err := client.Boolean(context.Background(), "dark-mode", false, EvalContext{TargetingKey: "user-1"})
				if err != nil || !on {
					t.Errorf("Boolean = %v, %v; want true, nil", on, err)
					return
				}
			}
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				client.SetFlag(CachedFlag{
					Key:          "dark-mode",
					Enabled:      true,
					DefaultValue: false,
					Overrides:    []CachedOverride{{Identifiers: []string{"user-1"}, Value: true}},
				})
				client.SetFlag(CachedFlag{Key: "other", Enabled: true, DefaultValue: "x"})
				client.InvalidateFlag("other")
			}
		}()
	}
	wg.Wait()
}
