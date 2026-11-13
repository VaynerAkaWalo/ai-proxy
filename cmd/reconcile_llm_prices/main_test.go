package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

	if len(updates) != 1 || updates[0].set["price_min_context_tokens"] != int64(200001) || updates[0].where["input_tokens"] != int64(300000) || updates[0].where["price_min_context_tokens"] != int64(0) || updates[0].where["price_provider"] != "anthropic" || updates[0].where["price_model"] != "claude-banded" {
		t.Fatalf("incorrect updates: %+v", updates)
	}
}

func fakeAnalytics(t *testing.T, patches *[]map[string]map[string]any) *httptest.Server {
	t.Helper()

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}

		if r.Method == http.MethodPatch {
			patch := map[string]map[string]any{"where": body["where"].(map[string]any), "set": body["set"].(map[string]any)}
			*patches = append(*patches, patch)
			rows := 5
			if _, banded := patch["where"]["input_tokens"]; banded {
				rows = 1
			}
			_ = json.NewEncoder(w).Encode(map[string]int{"rows": rows})
			return
		}

		query := body["query"].(map[string]any)
		rows := [][]any{
			{"claude", "claude-banded", "claude-banded", "standard", nil, nil, nil, nil, 5},
			{"codex", "plain-model", "plain-model", "standard", "openai", "plain-model", "", 0, 3},
		}
		if len(query["dimensions"].([]any)) == len(groupDimensions)+1 {
			rows = [][]any{
				{"claude", "claude-banded", "claude-banded", "standard", nil, nil, nil, nil, 300000, 1},
				{"claude", "claude-banded", "claude-banded", "standard", nil, nil, nil, nil, 1000, 4},
				{"claude", "claude-banded", "claude-banded", "standard", "anthropic", "claude-banded", "", 0, 400000, 2},
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"rows": rows})
	}))
}

func TestReconcileKeysThenFixesBandsAndLeavesKeyedAttemptsAlone(t *testing.T) {
	var patches []map[string]map[string]any
	server := fakeAnalytics(t, &patches)
	defer server.Close()
	path := filepath.Join(t.TempDir(), "catalog.json")
	if err := os.WriteFile(path, []byte(catalog), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ANALYTICS_TOKEN", "token")

	var out bytes.Buffer
	if err := run(t.Context(), []string{"-catalog", path, "-url", server.URL, "-apply"}, &out); err != nil {
		t.Fatal(err)
	}

	if len(patches) != 2 {
		t.Fatalf("incorrect patches: %+v", patches)
	}
	keyed, banded := patches[0], patches[1]
	if keyed["set"]["price_provider"] != "anthropic" || keyed["set"]["price_min_context_tokens"] != float64(0) || keyed["where"]["price_provider"] != nil || keyed["where"]["executed_model"] != "claude-banded" {
		t.Fatalf("incorrect key patch: %+v", keyed)
	}
	if banded["set"]["price_min_context_tokens"] != float64(200001) || banded["where"]["input_tokens"] != float64(300000) || banded["where"]["price_model"] != "claude-banded" {
		t.Fatalf("incorrect band patch: %+v", banded)
	}
}

func TestADryRunPatchesNothing(t *testing.T) {
	var patches []map[string]map[string]any
	server := fakeAnalytics(t, &patches)
	defer server.Close()
	path := filepath.Join(t.TempDir(), "catalog.json")
	if err := os.WriteFile(path, []byte(catalog), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ANALYTICS_TOKEN", "token")

	var out bytes.Buffer
	if err := run(t.Context(), []string{"-catalog", path, "-url", server.URL}, &out); err != nil || len(patches) != 0 || !bytes.Contains(out.Bytes(), []byte("price keys: 1 updates")) {
		t.Fatalf("dry run changed data or planned nothing: %v %d %s", err, len(patches), out.String())
	}
}
