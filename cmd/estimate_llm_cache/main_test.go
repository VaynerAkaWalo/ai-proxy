package main

import (
	"bytes"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const catalog = `{
  "claude-priced": {"litellm_provider": "anthropic", "mode": "chat", "input_cost_per_token": 0.000004, "output_cost_per_token": 0.00002, "cache_read_input_token_cost": 0.0000002, "cache_creation_input_token_cost": 0.000005},
  "claude-flat": {"litellm_provider": "anthropic", "mode": "chat", "input_cost_per_token": 0.000004, "output_cost_per_token": 0.00002, "cache_read_input_token_cost": 0.000004, "cache_creation_input_token_cost": 0.000005}
}`

var testRates = rates{input: 0.000004, cacheRead: 0.0000002, cacheWrite: 0.000005, output: 0.00002, reasoning: 0.00002}

func costOf(input, output, read, write, reasoning float64, price rates) float64 {
	uncached := 1 - read - write

	return input*(read*price.cacheRead+write*price.cacheWrite+uncached*price.input) + output*(reasoning*price.reasoning+(1-reasoning)*price.output)
}

func TestTheReadShareIsTheOneThatReproducesTheStoredCost(t *testing.T) {
	known := classified{input: 1000, output: 100, cacheRead: 900, cacheWrite: 30, reasoning: 25}
	old := legacy{attempts: 10, input: 2e6, output: 1e5, cost: costOf(2e6, 1e5, 0.8, 0.03, 0.25, testRates)}

	e := plan(modelKey{"claude", "claude-priced"}, old, known, testRates)

	if e.skipped != "" || math.Abs(e.readShare-0.8) > 1e-9 || math.Abs(e.writeShare-0.03) > 1e-9 || math.Abs(e.reasoning-0.25) > 1e-9 {
		t.Fatalf("incorrect estimate: %+v", e)
	}
}

func TestAnOlderMixWithFewerCacheWritesKeepsTheClassifiedUncachedShare(t *testing.T) {
	known := classified{input: 1000, output: 100, cacheRead: 969, cacheWrite: 30, reasoning: 25}
	old := legacy{attempts: 10, input: 2e6, output: 1e5, cost: costOf(2e6, 1e5, 0.989, 0.01, 0.25, testRates)}

	e := plan(modelKey{"claude", "claude-priced"}, old, known, testRates)

	if e.skipped != "" || math.Abs(e.writeShare-0.01) > 1e-9 || math.Abs(e.readShare-0.989) > 1e-9 {
		t.Fatalf("incorrect estimate: %+v", e)
	}
}

func TestAModelThatNeverWritesToTheCacheIsNotGivenWrites(t *testing.T) {
	known := classified{input: 1000, output: 100, cacheRead: 950, reasoning: 25}
	dear := legacy{attempts: 10, input: 2e6, output: 1e5, cost: 1.1 * costOf(2e6, 1e5, 0, 0, 0.25, testRates)}

	e := plan(modelKey{"codex", "claude-priced"}, dear, known, testRates)

	if e.skipped == "" || e.writeShare != 0 {
		t.Fatalf("writes were invented for a model without any: %+v", e)
	}
}

func TestACostAboveWhatFewerWritesExplainIsNotBlamedOnMoreWrites(t *testing.T) {
	known := classified{input: 1000, output: 100, cacheRead: 950, cacheWrite: 1, reasoning: 25}
	dear := legacy{attempts: 10, input: 2e6, output: 1e5, cost: 1.1 * costOf(2e6, 1e5, 0, 0, 0.25, testRates)}

	e := plan(modelKey{"claude", "claude-priced"}, dear, known, testRates)

	if e.skipped == "" {
		t.Fatalf("a cost above the classified mix was explained by more writes: %+v", e)
	}
}

func TestFlatCacheWritePricingCannotBeSolved(t *testing.T) {
	flat := testRates
	flat.cacheWrite = flat.cacheRead
	known := classified{input: 1000, output: 100, cacheRead: 969, cacheWrite: 30, reasoning: 25}
	cheap := legacy{attempts: 10, input: 2e6, output: 1e5, cost: 0.9 * costOf(2e6, 1e5, 1, 0, 0.25, flat)}

	e := plan(modelKey{"claude", "claude-flat"}, cheap, known, flat)

	if e.skipped == "" || math.IsNaN(e.writeShare) || math.IsInf(e.writeShare, 0) {
		t.Fatalf("an unsolvable write share was planned: %+v", e)
	}
}

func TestACostNoMixCanProduceIsSkipped(t *testing.T) {
	known := classified{input: 1000, output: 100, cacheRead: 900, cacheWrite: 30, reasoning: 25}
	cheap := legacy{attempts: 10, input: 2e6, output: 1e5, cost: 1}

	e := plan(modelKey{"claude", "claude-priced"}, cheap, known, testRates)

	if e.skipped == "" {
		t.Fatalf("an impossible share was planned: %+v", e)
	}
}

func TestAModelWithoutClassifiedUsageIsSkipped(t *testing.T) {
	e := plan(modelKey{"claude", "claude-priced"}, legacy{attempts: 1, input: 10, output: 1, cost: 1}, classified{}, testRates)

	if e.skipped == "" {
		t.Fatalf("a model with nothing to interpolate from was planned: %+v", e)
	}
}

func TestFlatCacheReadPricingCannotBeSolved(t *testing.T) {
	flat := testRates
	flat.cacheRead = flat.input
	known := classified{input: 1000, output: 100, cacheRead: 900, reasoning: 25}

	e := plan(modelKey{"claude", "claude-flat"}, legacy{attempts: 1, input: 10, output: 1, cost: 1}, known, flat)

	if e.skipped == "" {
		t.Fatalf("an unsolvable split was planned: %+v", e)
	}
}

type patch struct {
	Where map[string]any `json:"where"`
	Set   map[string]any `json:"set"`
}

func fakeAnalytics(t *testing.T, patches *[]patch, patched int) *httptest.Server {
	t.Helper()

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch {
			var p patch
			if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
				t.Error(err)
			}
			*patches = append(*patches, p)
			_ = json.NewEncoder(w).Encode(map[string]int{"rows": patched})
			return
		}

		var body struct {
			Query struct {
				Measures []string `json:"measures"`
			} `json:"query"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}

		rows := [][]any{{"claude", "claude-priced", 1000, 100, 900, 30, 25}}
		if len(body.Query.Measures) == 4 {
			rows = [][]any{
				{"claude", "claude-priced", 10, 2e6, 1e5, costOf(2e6, 1e5, 0.8, 0.03, 0.25, testRates)},
				{"xai", "grok", 5, 10, 1, 1},
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"rows": rows})
	}))
}

func runAgainst(t *testing.T, server *httptest.Server, args ...string) (string, error) {
	t.Helper()

	path := filepath.Join(t.TempDir(), "catalog.json")
	if err := os.WriteFile(path, []byte(catalog), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ANALYTICS_TOKEN", "token")

	var out bytes.Buffer
	err := run(t.Context(), append([]string{"-catalog", path, "-url", server.URL}, args...), &out)

	return out.String(), err
}

func TestApplyPatchesOnlyOldAttemptsWithoutACacheSplit(t *testing.T) {
	var patches []patch
	server := fakeAnalytics(t, &patches, 10)
	defer server.Close()

	if _, err := runAgainst(t, server, "-apply"); err != nil {
		t.Fatal(err)
	}

	if len(patches) != 1 {
		t.Fatalf("incorrect patches: %+v", patches)
	}
	where, set := patches[0].Where, patches[0].Set
	if where["executed_model"] != "claude-priced" || where["usage_quality"] != "unknown" || where["source"] != "ai_proxy" || where["cost_status"] != "priced" {
		t.Fatalf("incorrect selection: %+v", where)
	}
	if value, present := where["cache_read_tokens"]; !present || value != nil {
		t.Fatalf("attempts that already have a split are selected: %+v", where)
	}
	if set["usage_quality"] != "estimated" || set["unclassified_tokens"] != float64(0) {
		t.Fatalf("estimate is not marked as one: %+v", set)
	}
	read := set["cache_read_tokens"].(map[string]any)["multiply"].([]any)
	if read[0] != "input_tokens" || math.Abs(read[1].(float64)-0.8) > 1e-6 {
		t.Fatalf("incorrect cache read expression: %+v", read)
	}
}

func TestAPatchThatMatchesOtherRowsThanPlannedFails(t *testing.T) {
	var patches []patch
	server := fakeAnalytics(t, &patches, 11)
	defer server.Close()

	_, err := runAgainst(t, server, "-apply")

	if err == nil || !strings.Contains(err.Error(), "planned") {
		t.Fatalf("a row count mismatch was not reported: %v", err)
	}
}

func TestADryRunPatchesNothing(t *testing.T) {
	var patches []patch
	server := fakeAnalytics(t, &patches, 10)
	defer server.Close()

	out, err := runAgainst(t, server)

	if err != nil || len(patches) != 0 || !strings.Contains(out, "claude/claude-priced") || strings.Contains(out, "xai") {
		t.Fatalf("dry run changed data or planned the wrong models: %v %d %s", err, len(patches), out)
	}
}
