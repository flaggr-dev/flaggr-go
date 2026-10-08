package flaggr

import (
	"context"
	"testing"
)

func BenchmarkLocalClient_Boolean_Disabled(b *testing.B) {
	client, _ := NewClient(
		WithAPIKey("fgr_test"),
		WithServiceID("bench-svc"),
		WithLocalEvaluation(true),
	)
	client.SetFlag(CachedFlag{
		Key:          "enable-dark-mode",
		Enabled:      false,
		DefaultValue: false,
	})

	ctx := context.Background()
	evalCtx := EvalContext{TargetingKey: "user-123"}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _ = client.Boolean(ctx, "enable-dark-mode", false, evalCtx)
	}
}

func BenchmarkLocalClient_Boolean_Enabled(b *testing.B) {
	client, _ := NewClient(
		WithAPIKey("fgr_test"),
		WithServiceID("bench-svc"),
		WithLocalEvaluation(true),
	)
	client.SetFlag(CachedFlag{
		Key:          "enable-dark-mode",
		Enabled:      true,
		DefaultValue: true,
	})

	ctx := context.Background()
	evalCtx := EvalContext{TargetingKey: "user-123"}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _ = client.Boolean(ctx, "enable-dark-mode", false, evalCtx)
	}
}

func BenchmarkLocalClient_Boolean_Override(b *testing.B) {
	client, _ := NewClient(
		WithAPIKey("fgr_test"),
		WithServiceID("bench-svc"),
		WithLocalEvaluation(true),
	)
	client.SetFlag(CachedFlag{
		Key:          "enable-dark-mode",
		Enabled:      true,
		DefaultValue: false,
		Overrides: []CachedOverride{
			{Identifiers: []string{"user-123", "user-456"}, Value: true},
		},
	})

	ctx := context.Background()
	evalCtx := EvalContext{TargetingKey: "user-123"}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _ = client.Boolean(ctx, "enable-dark-mode", false, evalCtx)
	}
}

func BenchmarkLocalClient_String(b *testing.B) {
	client, _ := NewClient(
		WithAPIKey("fgr_test"),
		WithServiceID("bench-svc"),
		WithLocalEvaluation(true),
	)
	client.SetFlag(CachedFlag{
		Key:          "theme-variant",
		Enabled:      true,
		DefaultValue: "dark",
	})

	ctx := context.Background()
	evalCtx := EvalContext{TargetingKey: "user-123"}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _ = client.String(ctx, "theme-variant", "light", evalCtx)
	}
}
