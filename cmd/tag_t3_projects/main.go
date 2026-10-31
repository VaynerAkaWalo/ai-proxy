// Command tag_t3_projects backfills the project of IDV attempts recorded before ai-proxy tagged
// projects, attributing each attempt to the T3 chat that made it.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
)

type t3Thread struct {
	NativeID string `json:"native_id"`
	Provider string `json:"provider"`
	Cwd      string `json:"cwd"`
	Runs     string `json:"runs"`
}

type window struct {
	provider   string
	start, end time.Time
	project    string
}

type history struct {
	sessions map[string]string
	windows  []window
}

type row map[string]any

type outcome struct {
	Attempts int    `json:"attempts"`
	CostUSD  string `json:"cost_usd"`
	cost     *big.Rat
}

type report map[string]*outcome

// Drivers that reach models through ai-proxy, unlike Cursor or Antigravity with their own backends.
var proxiedDrivers = map[string]bool{"claudeAgent": true, "codex": true, "grok": true}

// T3 drivers can run another vendor's models, so the upstream ai-proxy records follows the model.
var modelProviders = []struct{ prefix, provider string }{
	{"claude-", "claude"}, {"gpt-", "codex"}, {"grok", "xai"}, {"custom-grok", "xai"},
}

func proxyProvider(model string) string {
	for _, m := range modelProviders {
		if strings.HasPrefix(model, m.prefix) {
			return m.provider
		}
	}
	return ""
}

var sessionPrefix = regexp.MustCompile(`^[a-z]+:`)
var integerColumns = []string{
	"status_code", "input_tokens", "output_tokens", "input_uncached_tokens", "cache_read_tokens",
	"cache_write_tokens", "output_non_reasoning_tokens", "reasoning_tokens", "total_tokens", "unclassified_tokens",
}

var datasetID = regexp.MustCompile(`^[a-z0-9_-]+/[a-z0-9_]+$`)

func loadHistory(paths []string) (history, error) {
	h := history{sessions: map[string]string{}}
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			return h, err
		}
		var threads []t3Thread
		if err := json.Unmarshal(raw, &threads); err != nil {
			return h, fmt.Errorf("%s: %w", path, err)
		}

		for _, thread := range threads {
			project := handlers.ProjectFromCwd(thread.Cwd)
			h.sessions[thread.NativeID] = project

			if !proxiedDrivers[thread.Provider] {
				continue
			}
			var runs [][3]string
			if err := json.Unmarshal([]byte(thread.Runs), &runs); err != nil {
				return h, fmt.Errorf("%s: runs of %s: %w", path, thread.NativeID, err)
			}
			for _, run := range runs {
				provider := proxyProvider(run[2])
				if provider == "" {
					continue
				}
				start, errStart := time.Parse(time.RFC3339Nano, run[0])
				end, errEnd := time.Parse(time.RFC3339Nano, run[1])
				if err := errors.Join(errStart, errEnd); err != nil {
					return h, fmt.Errorf("%s: run of %s: %w", path, thread.NativeID, err)
				}
				h.windows = append(h.windows, window{provider: provider, start: start, end: end, project: project})
			}
		}
	}
	return h, nil
}

// nativeSession strips the proxy's agent prefix and subagent suffix, leaving the id T3 stores.
func nativeSession(session string) string {
	session = sessionPrefix.ReplaceAllString(session, "")
	session, _, _ = strings.Cut(session, ":agent:")
	return session
}

// attribute returns the project of the T3 chat behind an attempt and how it was found.
// Attempts without a session can only be placed by time, so a chat in another project
// running concurrently on the same provider makes them ambiguous.
func (h history) attribute(attempt row, started, completed time.Time) (string, string) {
	if session := stringValue(attempt["session_id"]); session != "" {
		project, ok := h.sessions[nativeSession(session)]
		switch {
		case !ok:
			return "", "unmatched_session"
		case project == "":
			return "", "session_chat_without_project"
		}
		return project, "session"
	}

	projects := map[string]bool{}
	for _, w := range h.windows {
		if w.provider == stringValue(attempt["provider"]) && !started.Before(w.start) && !completed.After(w.end) {
			projects[w.project] = true
		}
	}
	switch len(projects) {
	case 0:
		return "", "unmatched_time"
	case 1:
		if projects[""] {
			return "", "time_chat_without_project"
		}
		for project := range projects {
			return project, "time"
		}
	}
	return "", "ambiguous_time"
}

func stringValue(value any) string {
	s, _ := value.(string)
	return s
}

// ClickHouse exports DateTime64(3, 'UTC') without a zone, IDV requires one.
func snapshotTime(value any) (time.Time, error) {
	return time.Parse("2006-01-02 15:04:05.999", stringValue(value))
}

// checkResendable rejects snapshot encodings IDV would refuse when the attempt is sent back,
// so a wrong export fails the dry run instead of an upload halfway through.
func checkResendable(attempt row) error {
	if _, ok := attempt["cost_usd"].(json.Number); ok {
		return errors.New("cost_usd is a number, export with output_format_json_quote_decimals=1")
	}
	for _, column := range integerColumns {
		if _, ok := attempt[column].(string); ok {
			return fmt.Errorf("%s is a string, export with output_format_json_quote_64bit_integers=0", column)
		}
	}
	return nil
}

func (r report) add(name string, attempt row) {
	o := r[name]
	if o == nil {
		o = &outcome{cost: new(big.Rat)}
		r[name] = o
	}
	o.Attempts++
	if value, ok := new(big.Rat).SetString(stringValue(attempt["cost_usd"])); ok {
		o.cost.Add(o.cost, value)
	}
	o.CostUSD = o.cost.FloatString(2)
}

func readLines(path string, consume func([]byte) error) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	line := 0
	for scanner.Scan() {
		line++
		if len(bytes.TrimSpace(scanner.Bytes())) == 0 {
			continue
		}
		if err := consume(scanner.Bytes()); err != nil {
			return fmt.Errorf("%s line %d: %w", path, line, err)
		}
	}
	return scanner.Err()
}

// tag returns the untagged attempts T3 history attributes to a project, ready to be re-sent.
func tag(h history, snapshotPath string) ([]row, report, error) {
	tagged, stats := []row{}, report{}
	err := readLines(snapshotPath, func(raw []byte) error {
		attempt := row{}
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		if err := decoder.Decode(&attempt); err != nil {
			return err
		}
		if stringValue(attempt["source"]) == "" || stringValue(attempt["execution_id"]) == "" {
			return errors.New("invalid IDV key")
		}
		if err := checkResendable(attempt); err != nil {
			return err
		}
		if attempt["project"] != nil {
			stats.add("already_tagged", attempt)
			return nil
		}

		started, err := snapshotTime(attempt["started_at"])
		if err != nil {
			return err
		}
		completed, err := snapshotTime(attempt["completed_at"])
		if err != nil {
			return err
		}
		project, how := h.attribute(attempt, started, completed)
		stats.add(how, attempt)
		if project == "" {
			return nil
		}

		attempt["project"], attempt["started_at"], attempt["completed_at"] = project, started.UTC(), completed.UTC()
		tagged = append(tagged, attempt)
		stats.add("project:"+project, attempt)
		return nil
	})
	return tagged, stats, err
}

func upload(ctx context.Context, client *http.Client, endpoint, token string, rows []row) error {
	for start := 0; start < len(rows); start += 256 {
		batch := rows[start:min(start+256, len(rows))]
		var body bytes.Buffer
		encoder := json.NewEncoder(&body)
		for _, attempt := range batch {
			if err := encoder.Encode(attempt); err != nil {
				return err
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, &body)
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/x-ndjson")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		response, err := client.Do(req)
		if err != nil {
			return fmt.Errorf("batch starting at %d: %w", start, err)
		}
		raw, errRead := io.ReadAll(io.LimitReader(response.Body, 1024*1024))
		_ = response.Body.Close()
		var acknowledged struct {
			Rows int `json:"rows"`
		}
		if response.StatusCode != http.StatusOK || errRead != nil || json.Unmarshal(raw, &acknowledged) != nil || acknowledged.Rows != len(batch) {
			return fmt.Errorf("batch starting at %d not acknowledged, HTTP %d: %s", start, response.StatusCode, raw)
		}
		fmt.Fprintf(os.Stderr, "acknowledged %d/%d rows\n", start+len(batch), len(rows))
	}
	return nil
}

func writeNDJSON(path string, rows []row) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(file)
	for _, attempt := range rows {
		if err := encoder.Encode(attempt); err != nil {
			_ = file.Close()
			return err
		}
	}
	return file.Close()
}

func run(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("tag_t3_projects", flag.ContinueOnError)
	var t3Paths []string
	flags.Func("t3", "Output of export.sql for one T3 install, repeatable", func(path string) error {
		t3Paths = append(t3Paths, path)
		return nil
	})
	existingPath := flags.String("existing", "", "JSONEachRow snapshot of the existing IDV attempts")
	output := flags.String("output", "", "New NDJSON file for the tagged attempts")
	apply := flags.Bool("apply", false, "Re-send tagged attempts, default is dry run")
	url := flags.String("url", "https://analytics.blamedevs.com", "IDV base URL")
	dataset := flags.String("dataset", "pawel/llm_usage", "Destination dataset owner/name")
	tokenEnv := flags.String("token-env", "", "Optional environment variable containing a bearer token")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if len(t3Paths) == 0 || *existingPath == "" || *output == "" || !datasetID.MatchString(*dataset) {
		return errors.New("t3, existing, output and an owner/name dataset are required")
	}
	if *tokenEnv != "" && os.Getenv(*tokenEnv) == "" {
		return errors.New("bearer token environment variable is empty")
	}

	h, err := loadHistory(t3Paths)
	if err != nil {
		return err
	}
	rows, stats, err := tag(h, *existingPath)
	if err != nil {
		return err
	}
	if err := writeNDJSON(*output, rows); err != nil {
		return err
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(stats); err != nil {
		return err
	}
	if !*apply {
		return nil
	}

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	endpoint := strings.TrimRight(*url, "/") + "/api/datasets/" + *dataset + "/rows"
	return upload(ctx, client, endpoint, os.Getenv(*tokenEnv), rows)
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	if err := run(ctx, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
