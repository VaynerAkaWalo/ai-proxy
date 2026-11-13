package main

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

const catalog = `{
  "plain-model": {"litellm_provider": "openai", "mode": "chat", "input_cost_per_token": 0.000001, "output_cost_per_token": 0.00001},
  "claude-banded": {"litellm_provider": "anthropic", "mode": "chat", "input_cost_per_token": 0.000003, "output_cost_per_token": 0.000015, "input_cost_per_token_above_200k_tokens": 0.000006}
}`

func testBook(t *testing.T) *usage.PriceBook {
	t.Helper()

	book, err := usage.NewLiteLLMPriceBook([]byte(catalog))
	if err != nil {
		t.Fatal(err)
	}

	return book
}

func ptr(value string) *string { return &value }

func unkeyed(provider, model string, count int64) attemptGroup {
	return attemptGroup{provider: ptr(provider), requested: ptr(model), executed: ptr(model), tier: ptr("standard"), count: count}
}

func TestOnlyAttemptsWithoutAKeyAreKeyed(t *testing.T) {
	keyed := unkeyed("codex", "plain-model", 5)
	keyed.current = &usage.PriceKey{Provider: "openai", Model: "plain-model"}
	noProvider := attemptGroup{executed: ptr("plain-model"), count: 3}

	updates := planGroups(testBook(t), []attemptGroup{keyed, noProvider, unkeyed("codex", "plain-model", 7)})

	if len(updates) != 1 || updates[0].attempts != 7 || updates[0].set["price_provider"] != "openai" || updates[0].set["price_model"] != "plain-model" || updates[0].set["price_min_context_tokens"] != int64(0) {
		t.Fatalf("incorrect updates: %+v", updates)
	}
	if updates[0].where["price_provider"] != nil || updates[0].where["executed_model"].(*string) == nil {
		t.Fatalf("incorrect selection: %+v", updates[0].where)
	}
}

func TestAModelWithoutAPriceStillGetsTheKeyItWillMatchLater(t *testing.T) {
	updates := planGroups(testBook(t), []attemptGroup{unkeyed("claude", "claude-new", 4)})

	if len(updates) != 1 || updates[0].found || updates[0].set["price_provider"] != "anthropic" || updates[0].set["price_model"] != "claude-new" {
		t.Fatalf("incorrect updates: %+v", updates)
	}
}

func TestContextBandsAreFixedForLargeContextsOnly(t *testing.T) {
	book := testBook(t)
	g := unkeyed("claude", "claude-banded", 2)
	if !isBanded(book, g) || isBanded(book, unkeyed("codex", "plain-model", 1)) {
		t.Fatal("banded models misdetected")
	}

	updates := planBands(book, []contextGroup{{group: g, tokens: 300000, attempts: 1}, {group: g, tokens: 1000, attempts: 1}})

	if len(updates) != 1 || updates[0].set["price_min_context_tokens"] != int64(200001) || updates[0].where["input_tokens"] != int64(300000) || updates[0].where["price_min_context_tokens"] != int64(0) {
		t.Fatalf("incorrect updates: %+v", updates)
	}
}
