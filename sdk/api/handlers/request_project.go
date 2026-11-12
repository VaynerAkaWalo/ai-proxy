package handlers

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	gjson "github.com/MoeForge/moego/ojson"
	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

const (
	projectHeader            = "X-Project"
	projectContextKey        = "cliproxy.project"
	projectHeaderContextKey  = "cliproxy.project.header"
	rememberedProjectTTL     = 24 * time.Hour
	rememberedProjectMaxSize = 8192
)

var projectPaths atomic.Pointer[map[string]string]

func setProjectPaths(cfg *config.SDKConfig) {
	if cfg != nil {
		projectPaths.Store(&cfg.ProjectPaths)
	}
}

func ProjectHeaderMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if project := strings.TrimSpace(c.GetHeader(projectHeader)); project != "" {
			c.Set(projectHeaderContextKey, project)
			c.Set(projectContextKey, project)
		}
		c.Request.Header.Del(projectHeader)
		c.Next()
	}
}

type projectHit int

const (
	projectMiss projectHit = iota
	projectTag
	projectSuppress
)

type projectDetection struct {
	hit projectHit
}

type projectDetectionContextKey struct{}

func withProjectDetection(ctx context.Context, hit projectHit) context.Context {
	return context.WithValue(ctx, projectDetectionContextKey{}, projectDetection{hit: hit})
}

func projectDetectionFrom(ctx context.Context) projectDetection {
	detection, _ := ctx.Value(projectDetectionContextKey{}).(projectDetection)
	return detection
}

func enrichContextWithProject(ctx context.Context, payload []byte) context.Context {
	detected, hit := resolveRequestProject(ctx, payload)
	ctx = withProjectDetection(ctx, hit)
	if hit == projectMiss {
		return ctx
	}
	meta := logging.GetClientRequestMetadata(ctx)
	if meta.Project == detected {
		return ctx
	}
	meta.Project = detected
	return logging.WithClientRequestMetadata(ctx, meta)
}

func requestProject(ctx context.Context, payload []byte) string {
	project, _ := resolveRequestProject(ctx, payload)
	return project
}

func resolveRequestProject(ctx context.Context, payload []byte) (string, projectHit) {
	ginCtx, _ := ctx.Value("gin").(*gin.Context)
	if ginCtx != nil {
		if project := strings.TrimSpace(ginCtx.GetString(projectHeaderContextKey)); project != "" {
			return strings.Clone(project), projectTag
		}
	}

	if cwd := cwdFromPayload(payload); cwd != "" {
		project := strings.Clone(projectFromCwd(cwd))
		if project == "" {
			if ginCtx != nil {
				ginCtx.Set(projectContextKey, "")
			}
			return "", projectSuppress
		}
		if ginCtx != nil {
			ginCtx.Set(projectContextKey, project)
		}
		return project, projectTag
	}

	if ginCtx != nil {
		if project := strings.TrimSpace(ginCtx.GetString(projectContextKey)); project != "" {
			return strings.Clone(project), projectTag
		}
	}
	return "", projectMiss
}

func resolveSessionProject(ctx context.Context, detected, sessionID, parentSessionID string) string {
	if sessionID == "" {
		return detected
	}
	scope := projectCallerScope(ctx)
	if projectDetectionFrom(ctx).hit == projectSuppress {
		storeRememberedProject(scope, sessionID, "", true)
		clearProjectOnConnection(ctx)
		return ""
	}
	if detected != "" {
		storeRememberedProject(scope, sessionID, detected, false)
		return detected
	}
	if project, ok := loadRememberedProject(scope, sessionID); ok {
		if project != "" {
			rememberProjectOnConnection(ctx, project)
		}
		return project
	}
	if parentSessionID != "" && parentSessionID != sessionID {
		if project, ok := loadRememberedProject(scope, parentSessionID); ok && project != "" {
			rememberProjectOnConnection(ctx, project)
			return project
		}
	}
	return ""
}

func projectCallerScope(ctx context.Context) string {
	ginCtx, _ := ctx.Value("gin").(*gin.Context)
	return requestCallerScope(ginCtx)
}

func rememberProjectOnConnection(ctx context.Context, project string) {
	ginCtx, _ := ctx.Value("gin").(*gin.Context)
	if ginCtx == nil || project == "" || strings.TrimSpace(ginCtx.GetString(projectHeaderContextKey)) != "" {
		return
	}
	ginCtx.Set(projectContextKey, project)
}

func clearProjectOnConnection(ctx context.Context) {
	ginCtx, _ := ctx.Value("gin").(*gin.Context)
	if ginCtx == nil || strings.TrimSpace(ginCtx.GetString(projectHeaderContextKey)) != "" {
		return
	}
	ginCtx.Set(projectContextKey, "")
}

type rememberedProjectEntry struct {
	project string
	expires time.Time
}

var (
	rememberedProjectsMu sync.Mutex
	rememberedProjects   = map[string]rememberedProjectEntry{}
)

func resetRememberedProjects() {
	rememberedProjectsMu.Lock()
	rememberedProjects = map[string]rememberedProjectEntry{}
	rememberedProjectsMu.Unlock()
}

func rememberedProjectKey(scope, sessionID string) string {
	return scope + "\x00" + sessionID
}

func storeRememberedProject(scope, sessionID, project string, suppressed bool) {
	if sessionID == "" || (project == "" && !suppressed) {
		return
	}
	rememberedProjectsMu.Lock()
	defer rememberedProjectsMu.Unlock()
	now := time.Now()
	key := rememberedProjectKey(scope, sessionID)
	if _, exists := rememberedProjects[key]; !exists && len(rememberedProjects) >= rememberedProjectMaxSize {
		for existingKey, entry := range rememberedProjects {
			if !entry.expires.After(now) {
				delete(rememberedProjects, existingKey)
			}
		}
		for len(rememberedProjects) >= rememberedProjectMaxSize {
			for existingKey := range rememberedProjects {
				delete(rememberedProjects, existingKey)
				break
			}
		}
	}
	rememberedProjects[key] = rememberedProjectEntry{
		project: project,
		expires: now.Add(rememberedProjectTTL),
	}
}

func loadRememberedProject(scope, sessionID string) (string, bool) {
	if sessionID == "" {
		return "", false
	}
	rememberedProjectsMu.Lock()
	defer rememberedProjectsMu.Unlock()
	key := rememberedProjectKey(scope, sessionID)
	entry, ok := rememberedProjects[key]
	if !ok {
		return "", false
	}
	now := time.Now()
	if !entry.expires.After(now) {
		delete(rememberedProjects, key)
		return "", false
	}
	entry.expires = now.Add(rememberedProjectTTL)
	rememberedProjects[key] = entry
	return entry.project, true
}

var cwdMarkers = []struct{ start, end string }{
	{"<cwd>", "</cwd>"},
	{"**Working directory:** ", "\n"},
	{"Working directory: ", "\n"},
	{"working directory: ", "\n"},
	{"**Workspace Directories:**\n  - ", "\n"},
	{"Workspace Path: ", "\n"},
	{"working in the directory: ", "\n"},
}

func cwdFromPayload(payload []byte) string {
	var instructions, messages []gjson.Result
	util.ParseGJSONBytesNoCopy(payload).ForEach(func(key, value gjson.Result) bool {
		switch key.Str {
		case "system", "instructions":
			instructions = append(instructions, value)
		case "systemInstruction", "system_instruction":
			instructions = append(instructions, value.Get("parts"))
		case "messages", "input", "contents":
			messages = append(messages, value)
		}
		return true
	})

	systemCwd := ""
	userCwd := ""
	for _, content := range instructions {
		if found := cwdFromContent(content); found != "" {
			systemCwd = found
		}
	}
	for _, group := range messages {
		systemFound, userFound := cwdFromMessageGroup(group)
		if systemFound != "" {
			systemCwd = systemFound
		}
		if userFound != "" {
			userCwd = userFound
		}
	}
	if systemCwd != "" {
		return systemCwd
	}
	return userCwd
}

func cwdFromMessageGroup(messages gjson.Result) (string, string) {
	if !messages.IsArray() {
		return "", cwdFromContent(messages)
	}

	systemCwd := ""
	userCwd := ""
	leading := true
	messages.ForEach(func(_, message gjson.Result) bool {
		content := message.Get("content")
		if !content.Exists() {
			content = message.Get("parts")
		}
		switch message.Get("role").String() {
		case "system", "developer":
			if found := cwdFromContent(content); found != "" {
				systemCwd = found
			}
		case "user":
			if !leading {
				return true
			}
			if found := cwdFromContent(content); found != "" {
				userCwd = found
			}
		default:
			leading = false
		}
		return true
	})
	return systemCwd, userCwd
}

func cwdFromContent(content gjson.Result) string {
	if !content.IsArray() {
		return cwdFromText(content.String())
	}

	cwd := ""
	content.ForEach(func(_, part gjson.Result) bool {
		text := part.Get("text").String()
		if part.Type == gjson.String {
			text = part.Str
		}
		if found := cwdFromText(text); found != "" {
			cwd = found
		}
		return true
	})
	return cwd
}

func cwdFromText(text string) string {
	found := ""
	for text != "" {
		start, marker, ok := earliestCwdMarker(text)
		if !ok {
			break
		}
		after := text[start+len(marker.start):]
		raw, rest, hasEnd := strings.Cut(after, marker.end)
		if cwd := cleanCwd(raw); looksLikePath(cwd) {
			found = cwd
		}
		if hasEnd {
			text = rest
			continue
		}
		text = after
	}
	return found
}

func earliestCwdMarker(text string) (int, struct{ start, end string }, bool) {
	bestAt := -1
	var best struct{ start, end string }
	for _, marker := range cwdMarkers {
		at := strings.Index(text, marker.start)
		if at < 0 {
			continue
		}
		if bestAt < 0 || at < bestAt || (at == bestAt && len(marker.start) > len(best.start)) {
			bestAt = at
			best = marker
		}
	}
	return bestAt, best, bestAt >= 0
}

func cleanCwd(raw string) string {
	cwd := strings.TrimSpace(raw)
	if before, _, ok := strings.Cut(cwd, " (was "); ok {
		cwd = strings.TrimSpace(before)
	}
	return cwd
}

func looksLikePath(cwd string) bool {
	if cwd == "" || strings.ContainsAny(cwd, " \t\r") {
		return false
	}
	return strings.Contains(cwd, "/") || strings.Contains(cwd, `\`)
}

func projectFromCwd(cwd string) string {
	cwd = strings.TrimRight(strings.ReplaceAll(cwd, `\`, "/"), "/")
	if cwd == "" {
		return ""
	}
	if project, ok := configuredProject(cwd); ok {
		return project
	}

	segments := strings.Split(cwd, "/")
	for i, segment := range segments {
		switch {
		case segment == ".worktrees":
			return segmentAt(segments, i-1)
		case segment == "worktrees" && segmentAt(segments, i-1) == ".claude":
			return segmentAt(segments, i-2)
		case segment == "worktrees" && segmentAt(segments, i-1) == ".codex":
			return segmentAt(segments, i+2)
		case segment == "worktrees":
			return segmentAt(segments, i+1)
		case segment == "scratch" && segmentAt(segments, i-1) == ".t3":
			return ""
		}
	}

	if isScratchDir(segments) {
		return ""
	}
	return segments[len(segments)-1]
}

func configuredProject(cwd string) (string, bool) {
	paths := projectPaths.Load()
	if paths == nil {
		return "", false
	}

	longest, project := -1, ""
	for prefix, name := range *paths {
		prefix = strings.TrimRight(prefix, "/")
		if len(prefix) > longest && (cwd == prefix || strings.HasPrefix(cwd, prefix+"/")) {
			longest, project = len(prefix), name
		}
	}
	return project, longest >= 0
}

func segmentAt(segments []string, i int) string {
	if i < 0 || i >= len(segments) {
		return ""
	}
	return segments[i]
}

func isScratchDir(segments []string) bool {
	if len(segments) < 2 {
		return true
	}
	switch segments[1] {
	case "tmp", "temp":
		return true
	case "var", "private":
		return len(segments) > 2 && (segments[2] == "tmp" || segments[2] == "folders" || segments[2] == "var")
	case "home", "Users":
		return len(segments) <= 3
	case "root":
		return len(segments) == 2
	}
	return false
}
