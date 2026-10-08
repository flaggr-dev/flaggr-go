package flaggr_test

import (
	"context"
	"fmt"
	"log"
	"os"

	flaggr "github.com/flaggr-dev/flaggr-go"
)

func ExampleNewClient() {
	client, err := flaggr.NewClient(
		flaggr.WithAPIKey(os.Getenv("FLAGGR_API_KEY")),       // project API token (fgr_...)
		flaggr.WithServiceID(os.Getenv("FLAGGR_SERVICE_ID")), // the service's ID, not its slug
		flaggr.WithEnvironment("production"),
	)
	if err != nil {
		log.Fatal(err)
	}
	defer client.Close()

	enabled, err := client.Boolean(context.Background(), "checkout-v2", false, flaggr.EvalContext{
		TargetingKey: "user-123",
		Attributes:   map[string]interface{}{"plan": "enterprise", "country": "AU"},
	})
	if err != nil {
		log.Printf("checkout-v2: using the default: %v", err)
	}
	fmt.Println("checkout-v2:", enabled)
}

func ExampleClient_Resolve() {
	client, err := flaggr.NewClient(
		flaggr.WithAPIKey(os.Getenv("FLAGGR_API_KEY")),
		flaggr.WithServiceID(os.Getenv("FLAGGR_SERVICE_ID")),
	)
	if err != nil {
		log.Fatal(err)
	}
	defer client.Close()

	detail, err := client.Resolve(context.Background(), "checkout-flow-version", "v1", flaggr.EvalContext{
		TargetingKey: "user-42",
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("value=%v reason=%s variant=%s\n", detail.Value, detail.Reason, detail.Variant)
}

func ExampleWithLocalEvaluation() {
	client, err := flaggr.NewClient(
		flaggr.WithAPIKey("fgr_example"),
		flaggr.WithServiceID("example-service-id"),
		flaggr.WithLocalEvaluation(true),
	)
	if err != nil {
		log.Fatal(err)
	}
	defer client.Close()

	client.LoadFlags([]flaggr.CachedFlag{{
		Key:          "dark-mode",
		Enabled:      true,
		DefaultValue: false,
		Overrides: []flaggr.CachedOverride{
			{Identifiers: []string{"user-123"}, Value: true},
		},
	}})

	ctx := context.Background()
	forUser, _ := client.Boolean(ctx, "dark-mode", false, flaggr.EvalContext{TargetingKey: "user-123"})
	forOthers, _ := client.Boolean(ctx, "dark-mode", false, flaggr.EvalContext{TargetingKey: "user-456"})
	missing, _ := client.Boolean(ctx, "not-cached", true)
	fmt.Println(forUser, forOthers, missing)
	// Output: true false true
}
