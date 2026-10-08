// Package flaggr provides a Go client for the Flaggr feature flag platform
// (https://flaggr.dev).
//
// It wraps the Flaggr REST API for flag evaluation with typed methods for
// boolean, string, number, and object flags.
//
// Install it with:
//
//	go get github.com/flaggr-dev/flaggr-go@latest
//
// Usage:
//
//	client, err := flaggr.NewClient(
//	    flaggr.WithAPIKey(os.Getenv("FLAGGR_API_KEY")),       // project API token (fgr_...)
//	    flaggr.WithServiceID(os.Getenv("FLAGGR_SERVICE_ID")), // the service's ID, not its slug
//	)
//	if err != nil {
//	    log.Fatal(err)
//	}
//	defer client.Close()
//
//	enabled, err := client.Boolean(ctx, "checkout-v2", false, flaggr.EvalContext{
//	    TargetingKey: "user-123",
//	})
package flaggr

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	// maxResponseBytes caps how much of a response body the client reads.
	// Evaluation responses are a few kilobytes.
	maxResponseBytes = 4 << 20

	// maxErrorBodyBytes caps how much of an error response body is quoted in
	// the returned error.
	maxErrorBodyBytes = 512
)

// CachedFlag stores flag configuration for zero-network local in-process evaluation.
type CachedFlag struct {
	Key          string
	Enabled      bool
	DefaultValue interface{}
	Overrides    []CachedOverride
}

// CachedOverride stores user/identity overrides.
type CachedOverride struct {
	Identifiers []string
	Value       interface{}
}

// Client is a Flaggr feature flag evaluation client.
type Client struct {
	apiURL      string
	apiKey      string
	serviceID   string
	environment string
	httpClient  *http.Client
	localEval   bool
	cacheMu     sync.RWMutex
	cache       map[string]CachedFlag
}

// EvalContext provides targeting attributes for flag evaluation.
type EvalContext struct {
	TargetingKey string
	Attributes   map[string]interface{}
}

// EvaluationDetail contains the full result of a flag evaluation.
type EvaluationDetail struct {
	Value        interface{} `json:"value"`
	Reason       string      `json:"reason"`
	Variant      string      `json:"variant"`
	ErrorCode    string      `json:"errorCode,omitempty"`
	ErrorMessage string      `json:"errorMessage,omitempty"`
}

// Option configures a Client.
type Option func(*Client)

// WithAPIURL sets the Flaggr API URL. Surrounding whitespace and trailing
// slashes are removed.
func WithAPIURL(url string) Option {
	return func(c *Client) { c.apiURL = strings.TrimRight(strings.TrimSpace(url), "/") }
}

// WithAPIKey sets the API authentication token. Surrounding whitespace, such
// as a trailing newline, is removed.
func WithAPIKey(key string) Option {
	return func(c *Client) { c.apiKey = strings.TrimSpace(key) }
}

// WithServiceID sets the service identifier for evaluations. Surrounding
// whitespace is removed.
func WithServiceID(id string) Option {
	return func(c *Client) { c.serviceID = strings.TrimSpace(id) }
}

// WithEnvironment sets the target environment (default: "production").
// Surrounding whitespace is removed.
func WithEnvironment(env string) Option {
	return func(c *Client) { c.environment = strings.TrimSpace(env) }
}

// WithHTTPClient sets a custom HTTP client. A nil client keeps the default.
func WithHTTPClient(hc *http.Client) Option {
	return func(c *Client) {
		if hc != nil {
			c.httpClient = hc
		}
	}
}

// WithLocalEvaluation enables zero-network in-process memory evaluation.
func WithLocalEvaluation(enabled bool) Option {
	return func(c *Client) { c.localEval = enabled }
}

// NewClient creates a new Flaggr client with the given options.
func NewClient(opts ...Option) (*Client, error) {
	c := &Client{
		apiURL:      "https://flaggr.dev",
		environment: "production",
		httpClient: &http.Client{
			Timeout: 5 * time.Second,
		},
		cache: make(map[string]CachedFlag),
	}

	for _, opt := range opts {
		opt(c)
	}

	if c.serviceID == "" {
		return nil, fmt.Errorf("flaggr: serviceID is required")
	}
	if c.apiKey == "" {
		return nil, fmt.Errorf("flaggr: apiKey is required")
	}

	return c, nil
}

// SetFlag inserts or updates a flag in the local in-process cache.
func (c *Client) SetFlag(flag CachedFlag) {
	c.cacheMu.Lock()
	defer c.cacheMu.Unlock()
	c.cache[flag.Key] = flag
}

// InvalidateFlag removes a flag from the local in-process cache.
func (c *Client) InvalidateFlag(flagKey string) {
	c.cacheMu.Lock()
	defer c.cacheMu.Unlock()
	delete(c.cache, flagKey)
}

// LoadFlags replaces all flags in the local in-process cache.
func (c *Client) LoadFlags(flags []CachedFlag) {
	c.cacheMu.Lock()
	defer c.cacheMu.Unlock()
	c.cache = make(map[string]CachedFlag, len(flags))
	for _, f := range flags {
		c.cache[f.Key] = f
	}
}

// Close releases resources held by the client.
func (c *Client) Close() {
	c.httpClient.CloseIdleConnections()
}

// Boolean evaluates a boolean flag.
func (c *Client) Boolean(ctx context.Context, flagKey string, defaultValue bool, evalCtx ...EvalContext) (bool, error) {
	if c.localEval {
		return c.evaluateLocalBoolean(flagKey, defaultValue, mergeCtx(evalCtx))
	}
	result, err := c.evaluate(ctx, flagKey, defaultValue, mergeCtx(evalCtx))
	if err != nil {
		return defaultValue, err
	}

	switch v := result.(type) {
	case bool:
		return v, nil
	case string:
		return v == "true", nil
	default:
		return defaultValue, nil
	}
}

func (c *Client) evaluateLocalBoolean(flagKey string, defaultValue bool, evalCtx *EvalContext) (bool, error) {
	c.cacheMu.RLock()
	flag, exists := c.cache[flagKey]
	c.cacheMu.RUnlock()

	if !exists {
		return defaultValue, nil
	}
	if !flag.Enabled {
		if v, ok := flag.DefaultValue.(bool); ok {
			return v, nil
		}
		return defaultValue, nil
	}

	if evalCtx != nil && evalCtx.TargetingKey != "" {
		for _, o := range flag.Overrides {
			for _, id := range o.Identifiers {
				if id == evalCtx.TargetingKey {
					if v, ok := o.Value.(bool); ok {
						return v, nil
					}
				}
			}
		}
	}

	if v, ok := flag.DefaultValue.(bool); ok {
		return v, nil
	}
	return defaultValue, nil
}

// String evaluates a string flag.
func (c *Client) String(ctx context.Context, flagKey string, defaultValue string, evalCtx ...EvalContext) (string, error) {
	if c.localEval {
		return c.evaluateLocalString(flagKey, defaultValue, mergeCtx(evalCtx))
	}
	result, err := c.evaluate(ctx, flagKey, defaultValue, mergeCtx(evalCtx))
	if err != nil {
		return defaultValue, err
	}

	if v, ok := result.(string); ok {
		return v, nil
	}
	return defaultValue, nil
}

func (c *Client) evaluateLocalString(flagKey string, defaultValue string, evalCtx *EvalContext) (string, error) {
	c.cacheMu.RLock()
	flag, exists := c.cache[flagKey]
	c.cacheMu.RUnlock()

	if !exists {
		return defaultValue, nil
	}
	if !flag.Enabled {
		if v, ok := flag.DefaultValue.(string); ok {
			return v, nil
		}
		return defaultValue, nil
	}

	if evalCtx != nil && evalCtx.TargetingKey != "" {
		for _, o := range flag.Overrides {
			for _, id := range o.Identifiers {
				if id == evalCtx.TargetingKey {
					if v, ok := o.Value.(string); ok {
						return v, nil
					}
				}
			}
		}
	}

	if v, ok := flag.DefaultValue.(string); ok {
		return v, nil
	}
	return defaultValue, nil
}

// Number evaluates a number flag.
func (c *Client) Number(ctx context.Context, flagKey string, defaultValue float64, evalCtx ...EvalContext) (float64, error) {
	result, err := c.evaluate(ctx, flagKey, defaultValue, mergeCtx(evalCtx))
	if err != nil {
		return defaultValue, err
	}

	switch v := result.(type) {
	case float64:
		return v, nil
	case json.Number:
		return v.Float64()
	default:
		return defaultValue, nil
	}
}

// Object evaluates an object flag.
func (c *Client) Object(ctx context.Context, flagKey string, defaultValue map[string]interface{}, evalCtx ...EvalContext) (map[string]interface{}, error) {
	result, err := c.evaluate(ctx, flagKey, defaultValue, mergeCtx(evalCtx))
	if err != nil {
		return defaultValue, err
	}

	if v, ok := result.(map[string]interface{}); ok {
		return v, nil
	}
	return defaultValue, nil
}

// Resolve evaluates a flag and returns the full evaluation detail.
func (c *Client) Resolve(ctx context.Context, flagKey string, defaultValue interface{}, evalCtx ...EvalContext) (*EvaluationDetail, error) {
	return c.evaluateDetail(ctx, flagKey, defaultValue, mergeCtx(evalCtx))
}

func mergeCtx(ctxs []EvalContext) *EvalContext {
	if len(ctxs) == 0 {
		return nil
	}
	return &ctxs[0]
}

type evalRequest struct {
	FlagKey      string                 `json:"flagKey"`
	ServiceID    string                 `json:"serviceId"`
	Environment  string                 `json:"environment"`
	DefaultValue interface{}            `json:"defaultValue"`
	Context      map[string]interface{} `json:"context,omitempty"`
}

func (c *Client) buildPayload(flagKey string, defaultValue interface{}, evalCtx *EvalContext) evalRequest {
	req := evalRequest{
		FlagKey:      flagKey,
		ServiceID:    c.serviceID,
		Environment:  c.environment,
		DefaultValue: defaultValue,
	}

	if evalCtx != nil {
		ctx := make(map[string]interface{})
		if evalCtx.TargetingKey != "" {
			ctx["targetingKey"] = evalCtx.TargetingKey
		}
		for k, v := range evalCtx.Attributes {
			ctx[k] = v
		}
		if len(ctx) > 0 {
			req.Context = ctx
		}
	}

	return req
}

func (c *Client) evaluate(ctx context.Context, flagKey string, defaultValue interface{}, evalCtx *EvalContext) (interface{}, error) {
	detail, err := c.evaluateDetail(ctx, flagKey, defaultValue, evalCtx)
	if err != nil {
		return defaultValue, err
	}
	return detail.Value, nil
}

func (c *Client) evaluateDetail(ctx context.Context, flagKey string, defaultValue interface{}, evalCtx *EvalContext) (*EvaluationDetail, error) {
	payload := c.buildPayload(flagKey, defaultValue, evalCtx)

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("flaggr: marshal error: %w", err)
	}

	url := c.apiURL + "/api/flags/evaluate"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("flaggr: request error: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("flaggr: http error: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("flaggr: read error: %w", err)
	}
	if len(respBody) > maxResponseBytes {
		return nil, fmt.Errorf("flaggr: evaluation failed: %d: response body larger than %d bytes", resp.StatusCode, maxResponseBytes)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("flaggr: evaluation failed: %d %s", resp.StatusCode, truncateBody(respBody, maxErrorBodyBytes))
	}

	var detail EvaluationDetail
	if err := json.Unmarshal(respBody, &detail); err != nil {
		return nil, fmt.Errorf("flaggr: unmarshal error: %w", err)
	}

	return &detail, nil
}

// truncateBody returns body as a string, cut to at most limit bytes on a rune
// boundary.
func truncateBody(body []byte, limit int) string {
	if len(body) <= limit {
		return string(body)
	}
	cut := limit
	for cut > limit-utf8.UTFMax+1 && cut > 0 && !utf8.RuneStart(body[cut]) {
		cut--
	}
	return string(body[:cut]) + "... (truncated)"
}
