# Flaggr Go SDK

[![Go Reference](https://pkg.go.dev/badge/github.com/flaggr-dev/flaggr-go.svg)](https://pkg.go.dev/github.com/flaggr-dev/flaggr-go)
[![CI](https://github.com/flaggr-dev/flaggr-go/actions/workflows/ci.yml/badge.svg)](https://github.com/flaggr-dev/flaggr-go/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

A Go client for [Flaggr](https://flaggr.dev), an OpenFeature-compatible feature flag platform.

The client evaluates flags through the Flaggr REST API. It has typed methods for boolean, string, number and object flags. It can also answer boolean and string flags in-process from a cache that you load.

## Installation

```bash
go get github.com/flaggr-dev/flaggr-go@latest
```

The module needs Go 1.22 or later and uses only the standard library. The package name is `flaggr`.

## Quick start

```go
package main

import (
	"context"
	"log"
	"os"

	flaggr "github.com/flaggr-dev/flaggr-go"
)

func main() {
	client, err := flaggr.NewClient(
		flaggr.WithAPIKey(os.Getenv("FLAGGR_API_KEY")),       // project API token (fgr_...)
		flaggr.WithServiceID(os.Getenv("FLAGGR_SERVICE_ID")), // service ID from the Flaggr console
	)
	if err != nil {
		log.Fatal(err)
	}
	defer client.Close()

	enabled, err := client.Boolean(context.Background(), "checkout-v2", false, flaggr.EvalContext{
		TargetingKey: "user-123",
	})
	if err != nil {
		log.Printf("checkout-v2: using the default: %v", err)
	}
	log.Printf("checkout-v2 = %v", enabled)
}
```

## Authentication

The client needs two values from your Flaggr project:

- **An API token.** Use a project API token (`fgr_...`). A token with read permission can evaluate flags. Create one in the project settings of the [Flaggr console](https://flaggr.dev) and keep it out of source control. Personal access tokens (`fgp_...`) can't evaluate flags. See [API Tokens](https://flaggr.dev/docs/api/tokens).
- **A service ID.** This is the ID of the service whose flags you evaluate. Copy it from the service's **Connection** tab in the console. The service's slug or display name won't work.

Every request sends the token in an `Authorization: Bearer <token>` header.

## Configuration

| Option | Default | Description |
|--------|---------|-------------|
| `WithAPIKey(token)` | required | Project API token |
| `WithServiceID(id)` | required | Service ID |
| `WithEnvironment(env)` | `"production"` | The environment to evaluate in, such as `development` or `staging` |
| `WithAPIURL(url)` | `"https://flaggr.dev"` | Base URL of the Flaggr API |
| `WithHTTPClient(hc)` | `&http.Client{Timeout: 5 * time.Second}` | Your own `*http.Client`, for timeouts, proxies or a custom transport |
| `WithLocalEvaluation(true)` | `false` | Answers `Boolean` and `String` from the local cache (see [Local evaluation](#local-evaluation)) |

`NewClient` returns an error when the API key or the service ID is empty. Each flag is configured separately in each environment, so a key can return different values in `development`, `staging` and `production`. See [Environments](https://flaggr.dev/docs/platform/environments).

## Evaluating flags

Each evaluation sends one `POST /api/flags/evaluate` request.

| Method | Returns |
|--------|---------|
| `Boolean(ctx, key, defaultValue, evalCtx...)` | `(bool, error)` |
| `String(ctx, key, defaultValue, evalCtx...)` | `(string, error)` |
| `Number(ctx, key, defaultValue, evalCtx...)` | `(float64, error)` |
| `Object(ctx, key, defaultValue, evalCtx...)` | `(map[string]interface{}, error)` |
| `Resolve(ctx, key, defaultValue, evalCtx...)` | `(*EvaluationDetail, error)` with the value, reason, variant and any error code |

The evaluation context is optional. If you pass more than one, only the first is used.

```go
ctx := context.Background()
user := flaggr.EvalContext{
	TargetingKey: "user-456",
	Attributes: map[string]interface{}{
		"plan":    "enterprise",
		"country": "AU",
		"seats":   25,
	},
}

theme, err := client.String(ctx, "theme", "light", user)
limit, err := client.Number(ctx, "rate-limit", 100, user)
banner, err := client.Object(ctx, "banner-config", map[string]interface{}{"text": ""}, user)

detail, err := client.Resolve(ctx, "checkout-v2", false, user)
if err == nil {
	log.Printf("value=%v reason=%s variant=%s", detail.Value, detail.Reason, detail.Variant)
}
```

`TargetingKey` is usually the user ID. Overrides and percentage rollouts are based on it. `Attributes` hold the values that your targeting rules match on. Attribute values must be strings, numbers or booleans: `https://flaggr.dev` returns `400 Bad Request` for nested objects, arrays and `nil`. See [Targeting Rules](https://flaggr.dev/docs/guides/targeting-rules).

## Error handling

The evaluation methods don't panic. A request can fail because of a network error, a timeout or a status other than 200, such as 401, 403 or 429. When it does, the typed methods return your default value and a non-nil error, and `Resolve` returns a nil detail and the error. Log the error and use the default:

```go
enabled, err := client.Boolean(ctx, "checkout-v2", false, user)
if err != nil {
	log.Printf("checkout-v2: using the default: %v", err)
}
```

In two cases the typed methods return your default value with a nil error:

- The flag doesn't exist in that service and environment. Call `Resolve` to see the reason: `NOT_FOUND` from `https://flaggr.dev`.
- The flag's value is of a different type from the method you called, such as `Number` on a string flag. One exception: `Boolean` reads the string `"true"` as `true` and any other string as `false`.

Flaggr gives the same 401 or 403 for a service ID that doesn't exist or that the token can't read. If you get one, check the service ID as well as the token.

## Local evaluation

With `WithLocalEvaluation(true)`, `Boolean` and `String` answer from an in-process cache without a network call. You fill the cache yourself because the client doesn't download flag definitions. `Number`, `Object` and `Resolve` still call the API.

```go
client, err := flaggr.NewClient(
	flaggr.WithAPIKey(os.Getenv("FLAGGR_API_KEY")),
	flaggr.WithServiceID(os.Getenv("FLAGGR_SERVICE_ID")),
	flaggr.WithLocalEvaluation(true),
)
if err != nil {
	log.Fatal(err)
}

client.LoadFlags([]flaggr.CachedFlag{{
	Key:          "dark-mode",
	Enabled:      true,
	DefaultValue: false,
	Overrides: []flaggr.CachedOverride{
		{Identifiers: []string{"user-123"}, Value: true},
	},
}})

on, _ := client.Boolean(ctx, "dark-mode", false, flaggr.EvalContext{TargetingKey: "user-123"}) // true
```

Local evaluation works like this:

- A key that isn't in the cache returns your default.
- A disabled flag returns its `DefaultValue`.
- An enabled flag returns the first override whose `Identifiers` include the `TargetingKey`. If no override matches, it returns the flag's `DefaultValue`.
- Cached values of the wrong type are skipped.

Targeting rules and percentage rollouts are evaluated only by the API. `SetFlag` adds or replaces one flag, `InvalidateFlag` removes one, and `LoadFlags` replaces the whole cache. A `Client` is safe for concurrent use.

## OpenFeature

Flaggr also serves the [OpenFeature Remote Evaluation Protocol (OFREP)](https://flaggr.dev/docs/api/ofrep). To use the OpenFeature Go SDK instead of this client, see the [Go SDK guide](https://flaggr.dev/docs/sdk/go-sdk).

## Documentation

- [Flaggr documentation](https://flaggr.dev/docs)
- [Quick start](https://flaggr.dev/docs/getting-started/quick-start)
- [REST API reference](https://flaggr.dev/docs/api/rest-endpoints)
- [API tokens](https://flaggr.dev/docs/api/tokens)
- [Package reference](https://pkg.go.dev/github.com/flaggr-dev/flaggr-go)

## License

[MIT](LICENSE)
