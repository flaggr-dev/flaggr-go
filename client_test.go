package flaggr

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func newTestServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	return httptest.NewServer(handler)
}

func respondJSON(w http.ResponseWriter, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(data)
}

func TestBoolean(t *testing.T) {
	ts := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		respondJSON(w, map[string]interface{}{
			"value":  true,
			"reason": "TARGETING_MATCH",
		})
	})
	defer ts.Close()

	client, err := NewClient(
		WithAPIURL(ts.URL),
		WithAPIKey("fgr_test"),
		WithServiceID("test-svc"),
	)
	if err != nil {
		t.Fatal(err)
	}

	val, err := client.Boolean(context.Background(), "my-flag", false)
	if err != nil {
		t.Fatal(err)
	}
	if !val {
		t.Error("expected true, got false")
	}
}

func TestBooleanWithContext(t *testing.T) {
	ts := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		var req evalRequest
		json.NewDecoder(r.Body).Decode(&req)

		if req.Context["targetingKey"] != "user-123" {
			t.Errorf("expected targetingKey=user-123, got %v", req.Context["targetingKey"])
		}

		respondJSON(w, map[string]interface{}{
			"value":  true,
			"reason": "TARGETING_MATCH",
		})
	})
	defer ts.Close()

	client, _ := NewClient(
		WithAPIURL(ts.URL),
		WithAPIKey("fgr_test"),
		WithServiceID("test-svc"),
	)

	val, err := client.Boolean(context.Background(), "my-flag", false, EvalContext{
		TargetingKey: "user-123",
		Attributes:   map[string]interface{}{"plan": "enterprise"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !val {
		t.Error("expected true")
	}
}

func TestString(t *testing.T) {
	ts := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		respondJSON(w, map[string]interface{}{
			"value":  "dark",
			"reason": "TARGETING_MATCH",
		})
	})
	defer ts.Close()

	client, _ := NewClient(
		WithAPIURL(ts.URL),
		WithAPIKey("fgr_test"),
		WithServiceID("test-svc"),
	)

	val, err := client.String(context.Background(), "theme", "light")
	if err != nil {
		t.Fatal(err)
	}
	if val != "dark" {
		t.Errorf("expected dark, got %s", val)
	}
}

func TestNumber(t *testing.T) {
	ts := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		respondJSON(w, map[string]interface{}{
			"value":  42.0,
			"reason": "TARGETING_MATCH",
		})
	})
	defer ts.Close()

	client, _ := NewClient(
		WithAPIURL(ts.URL),
		WithAPIKey("fgr_test"),
		WithServiceID("test-svc"),
	)

	val, err := client.Number(context.Background(), "limit", 10)
	if err != nil {
		t.Fatal(err)
	}
	if val != 42.0 {
		t.Errorf("expected 42, got %f", val)
	}
}

func TestErrorResponse(t *testing.T) {
	ts := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte("Unauthorized"))
	})
	defer ts.Close()

	client, _ := NewClient(
		WithAPIURL(ts.URL),
		WithAPIKey("fgr_test"),
		WithServiceID("test-svc"),
	)

	val, err := client.Boolean(context.Background(), "my-flag", false)
	if err == nil {
		t.Fatal("expected error")
	}
	if val != false {
		t.Error("expected default value on error")
	}
}

func TestNewClientValidation(t *testing.T) {
	_, err := NewClient(WithAPIKey("fgr_test"))
	if err == nil {
		t.Error("expected error for missing serviceID")
	}

	_, err = NewClient(WithServiceID("svc"))
	if err == nil {
		t.Error("expected error for missing apiKey")
	}
}

func TestResolve(t *testing.T) {
	ts := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		respondJSON(w, map[string]interface{}{
			"value":   true,
			"reason":  "TARGETING_MATCH",
			"variant": "on",
		})
	})
	defer ts.Close()

	client, _ := NewClient(
		WithAPIURL(ts.URL),
		WithAPIKey("fgr_test"),
		WithServiceID("test-svc"),
	)

	detail, err := client.Resolve(context.Background(), "my-flag", false)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Reason != "TARGETING_MATCH" {
		t.Errorf("expected TARGETING_MATCH, got %s", detail.Reason)
	}
	if detail.Variant != "on" {
		t.Errorf("expected on, got %s", detail.Variant)
	}
}
