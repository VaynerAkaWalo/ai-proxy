package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

const threads = `[
  {"native_id":"aaaa","provider":"claudeAgent","cwd":"/home/agent/.t3/worktrees/ai-proxy/t3-1","runs":"[[\"2026-10-10T10:00:00.000Z\",\"2026-10-10T11:00:00.000Z\"]]"},
  {"native_id":"bbbb","provider":"codex","cwd":"/Users/me/code/analytics","runs":"[[\"2026-10-10T10:30:00.000Z\",\"2026-10-10T12:00:00.000Z\"]]"},
  {"native_id":"cccc","provider":"claudeAgent","cwd":"/home/agent/.t3/scratch/chat-1","runs":"[[\"2026-10-10T12:00:00.000Z\",\"2026-10-10T13:00:00.000Z\"]]"},
  {"native_id":"dddd","provider":"claudeAgent","cwd":"/home/agent/code/rules","runs":"[[\"2026-10-10T10:50:00.000Z\",\"2026-10-10T10:55:00.000Z\"]]"},
  {"native_id":"eeee","provider":"codex","cwd":"/home/agent/code/vantage","runs":"[]"}
]`

func attempt(id, session, provider, started, completed, project string) string {
	fields := map[string]any{
		"source": "ai_proxy", "execution_id": id, "provider": provider,
		"started_at": started, "completed_at": completed, "cost_usd": "1.500000000000", "input_tokens": 10,
	}
	if session != "" {
		fields["session_id"] = session
	}
	if project != "" {
		fields["project"] = project
	}
	raw, _ := json.Marshal(fields)
	return string(raw)
}

func TestTagAttributesAttemptsToT3Chats(t *testing.T) {
	h, err := loadHistory([]string{writeFile(t, "t3.json", threads)})
	if err != nil {
		t.Fatal(err)
	}
	snapshot := strings.Join([]string{
		attempt("subagent", "claude:aaaa:agent:x", "claude", "2026-10-01 00:00:00.000", "2026-10-01 00:00:01.000", ""),
		attempt("litellm-session", "bbbb", "codex", "2026-09-01 00:00:00.000", "2026-09-01 00:00:01.000", ""),
		attempt("codex-subagent", "codex:eeee", "codex", "2026-10-10 10:00:00.000", "2026-10-10 10:00:01.000", ""),
		attempt("other-client", "claude:ffff", "claude", "2026-10-10 10:10:00.000", "2026-10-10 10:10:01.000", ""),
		attempt("by-time", "", "claude", "2026-10-10 10:10:00.000", "2026-10-10 10:10:01.500", ""),
		attempt("other-provider-overlaps", "", "codex", "2026-10-10 10:40:00.000", "2026-10-10 10:41:00.000", ""),
		attempt("concurrent", "", "claude", "2026-10-10 10:51:00.000", "2026-10-10 10:52:00.000", ""),
		attempt("past-run-end", "", "claude", "2026-10-10 10:59:00.000", "2026-10-10 11:00:01.000", ""),
		attempt("scratch", "", "claude", "2026-10-10 12:10:00.000", "2026-10-10 12:11:00.000", ""),
		attempt("scratch-session", "claude:cccc", "claude", "2026-10-10 12:10:00.000", "2026-10-10 12:11:00.000", ""),
		attempt("live", "", "claude", "2026-10-10 10:10:00.000", "2026-10-10 10:10:01.000", "thinky"),
	}, "\n")

	rows, stats, err := tag(h, writeFile(t, "snapshot.ndjson", snapshot))
	if err != nil {
		t.Fatal(err)
	}

	got := map[string]any{}
	for _, row := range rows {
		got[row["execution_id"].(string)] = row["project"]
	}
	want := map[string]any{
		"subagent": "ai-proxy", "litellm-session": "analytics", "codex-subagent": "vantage",
		"by-time": "ai-proxy", "other-provider-overlaps": "analytics",
	}
	if len(got) != len(want) {
		t.Fatalf("tagged %v, want %v", got, want)
	}
	for id, project := range want {
		if got[id] != project {
			t.Errorf("%s tagged %v, want %v", id, got[id], project)
		}
	}

	counts := map[string]int{
		"session": 3, "time": 2, "unmatched_session": 1, "ambiguous_time": 1, "unmatched_time": 1,
		"time_chat_without_project": 1, "session_chat_without_project": 1, "already_tagged": 1,
	}
	for name, count := range counts {
		if stats[name] == nil || stats[name].Attempts != count {
			t.Errorf("%s: got %+v, want %d attempts", name, stats[name], count)
		}
	}
	if stats["project:ai-proxy"].CostUSD != "3.00" {
		t.Errorf("ai-proxy cost %s", stats["project:ai-proxy"].CostUSD)
	}
}

func TestTagResendsZonedTimestamps(t *testing.T) {
	h, err := loadHistory([]string{writeFile(t, "t3.json", threads)})
	if err != nil {
		t.Fatal(err)
	}
	rows, _, err := tag(h, writeFile(t, "snapshot.ndjson", attempt("x", "claude:aaaa", "claude", "2026-10-10 10:10:00.250", "2026-10-10 10:10:01.000", "")))
	if err != nil {
		t.Fatal(err)
	}

	raw, err := json.Marshal(rows[0])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"started_at":"2026-10-10T10:10:00.25Z"`) || !strings.Contains(string(raw), `"cost_usd":"1.500000000000"`) {
		t.Fatalf("unexpected row %s", raw)
	}
}

func TestTagRejectsEncodingsIDVRefuses(t *testing.T) {
	h, err := loadHistory([]string{writeFile(t, "t3.json", threads)})
	if err != nil {
		t.Fatal(err)
	}
	for name, line := range map[string]string{
		"numeric decimal": `{"source":"ai_proxy","execution_id":"x","cost_usd":1.5}`,
		"quoted integer":  `{"source":"ai_proxy","execution_id":"x","input_tokens":"10"}`,
	} {
		if _, _, err := tag(h, writeFile(t, "snapshot.ndjson", line)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}
