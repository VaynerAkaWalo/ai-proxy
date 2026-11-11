package handlers

import (
	"context"
	"strings"
	"sync/atomic"

	gjson "github.com/MoeForge/moego/ojson"
	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

const (
	projectHeader     = "X-Project"
	projectContextKey = "cliproxy.project"
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
			c.Set(projectContextKey, project)
		}
		c.Request.Header.Del(projectHeader)
		c.Next()
	}
}

func enrichContextWithProject(ctx context.Context, payload []byte) context.Context {
	meta := logging.GetClientRequestMetadata(ctx)
	if meta.Project != "" {
		return ctx
	}

	meta.Project = requestProject(ctx, payload)
	if meta.Project == "" {
		return ctx
	}
	return logging.WithClientRequestMetadata(ctx, meta)
}

func requestProject(ctx context.Context, payload []byte) string {
	ginCtx, _ := ctx.Value("gin").(*gin.Context)
	if ginCtx != nil {
		if project := ginCtx.GetString(projectContextKey); project != "" {
			return project
		}
	}

	project := strings.Clone(projectFromCwd(cwdFromPayload(payload)))
	if ginCtx != nil && project != "" {
		ginCtx.Set(projectContextKey, project)
	}
	return project
}

var cwdMarkers = []struct{ start, end string }{
	{"<cwd>", "</cwd>"},
	{"Working directory: ", "\n"},
	{"working directory: ", "\n"},
	{"**Workspace Directories:**\n  - ", "\n"},
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

	for _, content := range instructions {
		if cwd := cwdFromContent(content); cwd != "" {
			return cwd
		}
	}
	for _, leading := range messages {
		if cwd := cwdFromLeadingMessages(leading); cwd != "" {
			return cwd
		}
	}
	return ""
}

func cwdFromLeadingMessages(messages gjson.Result) string {
	if !messages.IsArray() {
		return cwdFromContent(messages)
	}

	cwd := ""
	messages.ForEach(func(_, message gjson.Result) bool {
		switch message.Get("role").String() {
		case "system", "developer", "user":
		default:
			return false
		}
		content := message.Get("content")
		if !content.Exists() {
			content = message.Get("parts")
		}
		cwd = cwdFromContent(content)
		return cwd == ""
	})
	return cwd
}

func cwdFromContent(content gjson.Result) string {
	if !content.IsArray() {
		return cwdFromText(content.String())
	}

	cwd := ""
	content.ForEach(func(_, part gjson.Result) bool {
		if part.Type == gjson.String {
			cwd = cwdFromText(part.Str)
		} else {
			cwd = cwdFromText(part.Get("text").String())
		}
		return cwd == ""
	})
	return cwd
}

func cwdFromText(text string) string {
	for _, marker := range cwdMarkers {
		_, rest, found := strings.Cut(text, marker.start)
		if !found {
			continue
		}
		cwd, _, _ := strings.Cut(rest, marker.end)
		return strings.TrimSpace(cwd)
	}
	return ""
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
