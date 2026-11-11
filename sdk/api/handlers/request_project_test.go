package handlers

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
)

func TestRequestProjectClaudeEnvironmentSurvivesEditToolGuidance(t *testing.T) {
	payload := []byte(`{
		"system": [{"type": "text", "text": "That no longer holds for files inside the working directory: Edit works there without a prior Read.\n"}],
		"messages": [
			{"role": "user", "content": [{"type": "text", "text": "refresh the login page"}]},
			{"role": "system", "content": [{"type": "text", "text": "# Environment\nYou have been invoked in the following environment: \n - Primary working directory: /home/dev/.t3/worktrees/barricade/t3-7943b478-8f8a4b15e531ee6a\n - This is a git worktree\n"}]}
		]
	}`)
	if got := requestProject(context.Background(), payload); got != "barricade" {
		t.Fatalf("project = %q, want barricade", got)
	}
}

func TestRequestProjectClaudeEnvironmentUpdateReplacesDirectory(t *testing.T) {
	payload := []byte(`{
		"messages": [
			{"role": "system", "content": "# Environment\n - Primary working directory: /home/dev/.t3/worktrees/barricade/t3-7943b478-8f8a4b15e531ee6a\n"},
			{"role": "user", "content": "look at styles"},
			{"role": "assistant", "content": "The repo is ai-proxy and the working directory: /home/dev/code/other\n"},
			{"role": "system", "content": "# Environment update\n - Primary working directory: /home/dev/.t3/worktrees/barricade/t3-7943b478-8f8a4b15e531ee6a/src/web/styles (was /home/dev/.t3/worktrees/barricade/t3-7943b478-8f8a4b15e531ee6a)\n"}
		]
	}`)
	if got := requestProject(context.Background(), payload); got != "barricade" {
		t.Fatalf("project = %q, want barricade", got)
	}
	replaced := []byte(`{"system":" - Primary working directory: /home/dev/code/barricade (was /home/dev/code/analytics)\n"}`)
	if got := requestProject(context.Background(), replaced); got != "barricade" {
		t.Fatalf("replaced project = %q, want barricade", got)
	}
}

func TestRequestProjectGrokWorkingDirectory(t *testing.T) {
	payload := []byte(`{
		"messages": [{
			"role": "system",
			"content": "**Session ID:** 01a1ca4e-a7f8-78b3-a816-5bc36e74b846\n\n**Working directory:** /home/dev/.t3/worktrees/ai-proxy/t3-3a2386cc-d8f52f4863c89df6\n\n**Turn:** 4\n"
		}]
	}`)
	if got := requestProject(context.Background(), payload); got != "ai-proxy" {
		t.Fatalf("project = %q, want ai-proxy", got)
	}
}

func TestRequestProjectSystemDirectoryOutranksUserPrompt(t *testing.T) {
	payload := []byte(`{
		"messages": [
			{"role": "system", "content": "**Working directory:** /home/dev/.t3/worktrees/ai-proxy/t3-3a2386cc-d8f52f4863c89df6\n"},
			{"role": "user", "content": "Working directory: /home/dev/code/barricade\n"}
		]
	}`)
	if got := requestProject(context.Background(), payload); got != "ai-proxy" {
		t.Fatalf("project = %q, want ai-proxy", got)
	}
}

func TestRequestProjectScratchAndConversationStayUntagged(t *testing.T) {
	scratch := []byte(`{"messages":[{"role":"system","content":" - Primary working directory: /home/dev/.t3/scratch/2026-11-11-chat\n"}]}`)
	if got := requestProject(context.Background(), scratch); got != "" {
		t.Fatalf("scratch project = %q, want empty", got)
	}
	chat := []byte(`{"messages":[{"role":"user","content":"most usage in barricade is untagged"},{"role":"assistant","content":"working directory: /home/dev/code/barricade\n"}]}`)
	if got := requestProject(context.Background(), chat); got != "" {
		t.Fatalf("conversation project = %q, want empty", got)
	}
}

func TestRequestProjectExistingDirectoryMarkers(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		want    string
	}{
		{
			name:    "codex cwd",
			payload: `{"input":[{"role":"user","content":[{"type":"input_text","text":"<environment_context>\n<cwd>/home/dev/.t3/worktrees/analytics/t3-abc</cwd>\n</environment_context>"}]}]}`,
			want:    "analytics",
		},
		{
			name:    "opencode workspace",
			payload: `{"messages":[{"role":"user","content":"**Workspace Directories:**\n  - /home/dev/code/changefeed\n"}]}`,
			want:    "changefeed",
		},
		{
			name:    "gemini directory",
			payload: `{"systemInstruction":{"parts":[{"text":"You are working in the directory: /home/dev/code/secrets\n"}]}}`,
			want:    "secrets",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := requestProject(context.Background(), []byte(tc.payload)); got != tc.want {
				t.Fatalf("project = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSessionProjectRememberedUntilOverridden(t *testing.T) {
	resetRememberedProjects()
	t.Cleanup(resetRememberedProjects)

	headers := http.Header{"X-Claude-Code-Session-Id": []string{"sess-barricade"}}
	detected := []byte(`{"messages":[{"role":"system","content":" - Primary working directory: /home/dev/.t3/worktrees/barricade/t3-7943b478-8f8a4b15e531ee6a\n"}]}`)
	first := EnrichContextWithSessionHierarchy(projectTestContext("client-a"), headers, detected, nil)
	if got := logging.GetClientRequestMetadata(first).Project; got != "barricade" {
		t.Fatalf("detected project = %q, want barricade", got)
	}

	omitted := []byte(`{"messages":[{"role":"user","content":"continue"}]}`)
	second := EnrichContextWithSessionHierarchy(projectTestContext("client-a"), headers, omitted, nil)
	if got := logging.GetClientRequestMetadata(second).Project; got != "barricade" {
		t.Fatalf("remembered project = %q, want barricade", got)
	}

	otherClient := EnrichContextWithSessionHierarchy(projectTestContext("client-b"), headers, omitted, nil)
	if got := logging.GetClientRequestMetadata(otherClient).Project; got != "" {
		t.Fatalf("other client project = %q, want empty", got)
	}

	moved := []byte(`{"messages":[{"role":"system","content":"**Working directory:** /home/dev/.t3/worktrees/ai-proxy/t3-3a2386cc-d8f52f4863c89df6\n"}]}`)
	third := EnrichContextWithSessionHierarchy(projectTestContext("client-a"), headers, moved, nil)
	if got := logging.GetClientRequestMetadata(third).Project; got != "ai-proxy" {
		t.Fatalf("updated project = %q, want ai-proxy", got)
	}
	fourth := EnrichContextWithSessionHierarchy(projectTestContext("client-a"), headers, omitted, nil)
	if got := logging.GetClientRequestMetadata(fourth).Project; got != "ai-proxy" {
		t.Fatalf("updated memory = %q, want ai-proxy", got)
	}

	explicit := projectTestContext("client-a")
	ginCtx, _ := explicit.Value("gin").(*gin.Context)
	ginCtx.Request.Header.Set("X-Project", "manual")
	ProjectHeaderMiddleware()(ginCtx)
	overridden := EnrichContextWithSessionHierarchy(explicit, headers, omitted, nil)
	if got := logging.GetClientRequestMetadata(overridden).Project; got != "manual" {
		t.Fatalf("header project = %q, want manual", got)
	}
}

func TestSessionProjectInheritsParentUntilChildHasWorkspace(t *testing.T) {
	resetRememberedProjects()
	t.Cleanup(resetRememberedProjects)

	parentHeaders := http.Header{"X-Claude-Code-Session-Id": []string{"parent-sess"}}
	parentPayload := []byte(`{"messages":[{"role":"system","content":" - Primary working directory: /home/dev/.t3/worktrees/barricade/t3-parent\n"}]}`)
	parent := EnrichContextWithSessionHierarchy(projectTestContext("client-a"), parentHeaders, parentPayload, nil)
	if got := logging.GetClientRequestMetadata(parent).Project; got != "barricade" {
		t.Fatalf("parent project = %q, want barricade", got)
	}

	childHeaders := http.Header{"X-Claude-Code-Session-Id": []string{"child-sess"}}
	childPayload := []byte(`{"parent_session_id":"parent-sess","messages":[{"role":"user","content":"delegate this"}]}`)
	child := EnrichContextWithSessionHierarchy(projectTestContext("client-a"), childHeaders, childPayload, nil)
	if got := logging.GetClientRequestMetadata(child).Project; got != "barricade" {
		t.Fatalf("child project = %q, want barricade", got)
	}

	childWorkspace := []byte(`{"parent_session_id":"parent-sess","messages":[{"role":"system","content":"**Working directory:** /home/dev/code/analytics\n"}]}`)
	moved := EnrichContextWithSessionHierarchy(projectTestContext("client-a"), childHeaders, childWorkspace, nil)
	if got := logging.GetClientRequestMetadata(moved).Project; got != "analytics" {
		t.Fatalf("child workspace project = %q, want analytics", got)
	}
}

func TestSessionProjectExplicitEmptyWorkspaceClearsMemory(t *testing.T) {
	cases := []struct {
		name string
		cwd  string
	}{
		{name: "t3 scratch", cwd: "/home/dev/.t3/scratch/2026-11-11-chat"},
		{name: "temp dir", cwd: "/tmp/claude-scratch"},
		{name: "home dir", cwd: "/home/dev"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resetRememberedProjects()
			t.Cleanup(resetRememberedProjects)

			headers := http.Header{"X-Claude-Code-Session-Id": []string{"sess-" + strings.ReplaceAll(tc.name, " ", "-")}}
			detected := []byte(`{"messages":[{"role":"system","content":" - Primary working directory: /home/dev/.t3/worktrees/barricade/t3-7943b478-8f8a4b15e531ee6a\n"}]}`)
			first := EnrichContextWithSessionHierarchy(projectTestContext("client-a"), headers, detected, nil)
			if got := logging.GetClientRequestMetadata(first).Project; got != "barricade" {
				t.Fatalf("detected project = %q, want barricade", got)
			}

			scratch := []byte(fmt.Sprintf(`{"messages":[{"role":"system","content":" - Primary working directory: %s\n"}]}`, tc.cwd))
			second := EnrichContextWithSessionHierarchy(projectTestContext("client-a"), headers, scratch, nil)
			if got := logging.GetClientRequestMetadata(second).Project; got != "" {
				t.Fatalf("scratch project = %q, want empty", got)
			}

			omitted := []byte(`{"messages":[{"role":"user","content":"continue"}]}`)
			third := EnrichContextWithSessionHierarchy(projectTestContext("client-a"), headers, omitted, nil)
			if got := logging.GetClientRequestMetadata(third).Project; got != "" {
				t.Fatalf("cleared memory = %q, want empty", got)
			}

			moved := []byte(`{"messages":[{"role":"system","content":"**Working directory:** /home/dev/.t3/worktrees/ai-proxy/t3-3a2386cc-d8f52f4863c89df6\n"}]}`)
			fourth := EnrichContextWithSessionHierarchy(projectTestContext("client-a"), headers, moved, nil)
			if got := logging.GetClientRequestMetadata(fourth).Project; got != "ai-proxy" {
				t.Fatalf("retagged project = %q, want ai-proxy", got)
			}
		})
	}
}

func TestSessionProjectScratchDoesNotInheritParent(t *testing.T) {
	resetRememberedProjects()
	t.Cleanup(resetRememberedProjects)

	parentHeaders := http.Header{"X-Claude-Code-Session-Id": []string{"parent-sess"}}
	parentPayload := []byte(`{"messages":[{"role":"system","content":" - Primary working directory: /home/dev/.t3/worktrees/barricade/t3-parent\n"}]}`)
	parent := EnrichContextWithSessionHierarchy(projectTestContext("client-a"), parentHeaders, parentPayload, nil)
	if got := logging.GetClientRequestMetadata(parent).Project; got != "barricade" {
		t.Fatalf("parent project = %q, want barricade", got)
	}

	childHeaders := http.Header{"X-Claude-Code-Session-Id": []string{"child-sess"}}
	scratch := []byte(`{"parent_session_id":"parent-sess","messages":[{"role":"system","content":" - Primary working directory: /home/dev/.t3/scratch/delegate\n"}]}`)
	child := EnrichContextWithSessionHierarchy(projectTestContext("client-a"), childHeaders, scratch, nil)
	if got := logging.GetClientRequestMetadata(child).Project; got != "" {
		t.Fatalf("scratch child project = %q, want empty", got)
	}

	followUp := []byte(`{"parent_session_id":"parent-sess","messages":[{"role":"user","content":"continue"}]}`)
	again := EnrichContextWithSessionHierarchy(projectTestContext("client-a"), childHeaders, followUp, nil)
	if got := logging.GetClientRequestMetadata(again).Project; got != "" {
		t.Fatalf("scratch child memory = %q, want empty", got)
	}

	parentAgain := EnrichContextWithSessionHierarchy(projectTestContext("client-a"), parentHeaders, []byte(`{"messages":[{"role":"user","content":"continue"}]}`), nil)
	if got := logging.GetClientRequestMetadata(parentAgain).Project; got != "barricade" {
		t.Fatalf("parent memory = %q, want barricade", got)
	}
}

func TestSessionProjectScratchClearsWithoutGinContext(t *testing.T) {
	resetRememberedProjects()
	t.Cleanup(resetRememberedProjects)

	headers := http.Header{"X-Claude-Code-Session-Id": []string{"sess-no-gin"}}
	detected := []byte(`{"messages":[{"role":"system","content":" - Primary working directory: /home/dev/code/barricade\n"}]}`)
	first := EnrichContextWithSessionHierarchy(context.Background(), headers, detected, nil)
	if got := logging.GetClientRequestMetadata(first).Project; got != "barricade" {
		t.Fatalf("detected project = %q, want barricade", got)
	}

	scratch := []byte(`{"messages":[{"role":"system","content":" - Primary working directory: /tmp/claude-scratch\n"}]}`)
	second := EnrichContextWithSessionHierarchy(context.Background(), headers, scratch, nil)
	if got := logging.GetClientRequestMetadata(second).Project; got != "" {
		t.Fatalf("scratch project = %q, want empty", got)
	}

	omitted := []byte(`{"messages":[{"role":"user","content":"continue"}]}`)
	third := EnrichContextWithSessionHierarchy(context.Background(), headers, omitted, nil)
	if got := logging.GetClientRequestMetadata(third).Project; got != "" {
		t.Fatalf("cleared memory = %q, want empty", got)
	}

	parentHeaders := http.Header{"X-Claude-Code-Session-Id": []string{"parent-no-gin"}}
	parentPayload := []byte(`{"messages":[{"role":"system","content":" - Primary working directory: /home/dev/code/barricade\n"}]}`)
	parent := EnrichContextWithSessionHierarchy(context.Background(), parentHeaders, parentPayload, nil)
	if got := logging.GetClientRequestMetadata(parent).Project; got != "barricade" {
		t.Fatalf("parent project = %q, want barricade", got)
	}
	childHeaders := http.Header{"X-Claude-Code-Session-Id": []string{"child-no-gin"}}
	childScratch := []byte(`{"parent_session_id":"parent-no-gin","messages":[{"role":"system","content":" - Primary working directory: /tmp/claude-scratch\n"}]}`)
	child := EnrichContextWithSessionHierarchy(context.Background(), childHeaders, childScratch, nil)
	if got := logging.GetClientRequestMetadata(child).Project; got != "" {
		t.Fatalf("scratch delegate project = %q, want empty", got)
	}
	followUp := []byte(`{"parent_session_id":"parent-no-gin","messages":[{"role":"user","content":"continue"}]}`)
	again := EnrichContextWithSessionHierarchy(context.Background(), childHeaders, followUp, nil)
	if got := logging.GetClientRequestMetadata(again).Project; got != "" {
		t.Fatalf("scratch delegate memory = %q, want empty", got)
	}
}

func TestRequestProjectScratchClearsConnectionProject(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	ginCtx.Set(projectContextKey, "stale")
	ctx := context.WithValue(context.Background(), "gin", ginCtx)
	payload := []byte(`{"messages":[{"role":"system","content":"**Working directory:** /tmp/claude-scratch\n"}]}`)
	if got := requestProject(ctx, payload); got != "" {
		t.Fatalf("project = %q, want empty", got)
	}
	if got := ginCtx.GetString(projectContextKey); got != "" {
		t.Fatalf("connection project = %q, want empty", got)
	}
}

func TestRequestProjectConnectionMemoryYieldsToNewDirectory(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	ginCtx.Set(projectContextKey, "stale")
	ctx := context.WithValue(context.Background(), "gin", ginCtx)
	payload := []byte(`{"messages":[{"role":"system","content":"**Working directory:** /home/dev/.t3/worktrees/ai-proxy/t3-new\n"}]}`)
	if got := requestProject(ctx, payload); got != "ai-proxy" {
		t.Fatalf("project = %q, want ai-proxy", got)
	}
}

func projectTestContext(apiKey string) context.Context {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	ginCtx.Set("userApiKey", apiKey)
	return context.WithValue(context.Background(), "gin", ginCtx)
}
