package main

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Activity separated by more than this is a different window, so an idle session does not
// claim the hours between its messages.
const idleGap = 10 * time.Minute

// cliSession is what the collector keeps of a Claude Code or Codex session: no transcripts.
type cliSession struct {
	Provider string      `json:"provider"`
	ID       string      `json:"id"`
	Cwd      string      `json:"cwd"`
	Windows  [][2]string `json:"windows"`
}

type cliLine struct {
	Timestamp string `json:"timestamp"`
	SessionID string `json:"sessionId"`
	Cwd       string `json:"cwd"`
	Type      string `json:"type"`
	Payload   struct {
		ID  string `json:"id"`
		Cwd string `json:"cwd"`
	} `json:"payload"`
}

type activity struct {
	cwd    string
	stamps []time.Time
}

// collectCLISessions scans Claude Code and Codex session directories. Subagent transcripts
// carry the parent sessionId, so they fold into the parent's windows.
func collectCLISessions(claudeDir, codexDir string) ([]cliSession, error) {
	var sessions []cliSession
	for _, source := range []struct{ provider, dir string }{{"claude", claudeDir}, {"codex", codexDir}} {
		found := map[string]*activity{}
		err := filepath.WalkDir(source.dir, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				if os.IsNotExist(err) {
					return nil
				}
				return err
			}
			if entry.IsDir() || !strings.HasSuffix(path, ".jsonl") {
				return nil
			}
			return readLines(path, func(raw []byte) error {
				var line cliLine
				if json.Unmarshal(raw, &line) != nil {
					return nil
				}
				id, cwd := line.SessionID, line.Cwd
				if line.Type == "session_meta" {
					id, cwd = line.Payload.ID, line.Payload.Cwd
				}
				if id == "" && source.provider == "codex" {
					id = codexIDFromPath(path)
				}
				stamp, err := time.Parse(time.RFC3339Nano, line.Timestamp)
				if id == "" || err != nil {
					return nil
				}
				a := found[id]
				if a == nil {
					a = &activity{}
					found[id] = a
				}
				if a.cwd == "" && !strings.Contains(path, string(filepath.Separator)+"subagents"+string(filepath.Separator)) {
					a.cwd = cwd
				}
				a.stamps = append(a.stamps, stamp)
				return nil
			})
		})
		if err != nil {
			return nil, err
		}
		for id, a := range found {
			sessions = append(sessions, cliSession{Provider: source.provider, ID: id, Cwd: a.cwd, Windows: a.windows()})
		}
	}
	sort.Slice(sessions, func(i, j int) bool { return sessions[i].ID < sessions[j].ID })
	return sessions, nil
}

// codexIDFromPath reads the uuid out of rollout-<timestamp>-<uuid>.jsonl for lines without a session_meta.
func codexIDFromPath(path string) string {
	name := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	if len(name) < 36 {
		return ""
	}
	return name[len(name)-36:]
}

func (a *activity) windows() [][2]string {
	sort.Slice(a.stamps, func(i, j int) bool { return a.stamps[i].Before(a.stamps[j]) })
	var windows [][2]string
	start, last := a.stamps[0], a.stamps[0]
	flush := func() {
		windows = append(windows, [2]string{start.UTC().Format(time.RFC3339Nano), last.UTC().Format(time.RFC3339Nano)})
	}
	for _, stamp := range a.stamps[1:] {
		if stamp.Sub(last) > idleGap {
			flush()
			start = stamp
		}
		last = stamp
	}
	flush()
	return windows
}
