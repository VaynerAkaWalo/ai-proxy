// Package quotaprobe reads subscription plan, usage windows and manual reset
// counts from the same upstream endpoints the management panel's quota page
// uses, so exports can report what the panel shows.
package quotaprobe

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"

	claudeauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/claude"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

const (
	probeTimeout = 30 * time.Second
	maxBodyBytes = 1 << 20

	codexUsageURL = "https://chatgpt.com/backend-api/wham/usage"
	codexUA       = "codex-tui/0.149.1 (Mac OS 26.5.2; arm64) iTerm.app/3.6.11 (codex-tui; 0.149.1)"

	xaiBillingWeeklyURL = "https://cli-chat-proxy.grok.com/v1/billing?format=credits"
	xaiUserURL          = "https://cli-chat-proxy.grok.com/v1/user?include=subscription"
	xaiSettingsURL      = "https://cli-chat-proxy.grok.com/v1/settings"
	xaiClientVersion    = "1.0.46"
)

// Result is what one probe learned about a credential. Unknown values are
// zero or nil.
type Result struct {
	ObservedAt       time.Time
	Plan             string
	ManualResetsLeft *int
	Windows          []coreauth.QuotaWindowUsage
}

// Supported reports whether the provider has a probe.
func Supported(provider string) bool {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "claude", "codex", "xai":
		return true
	}
	return false
}

// Probe queries the upstream for one credential. It sends only the credential's
// own token to its own provider.
func Probe(ctx context.Context, cfg *config.Config, auth *coreauth.Auth) (*Result, error) {
	if auth == nil {
		return nil, fmt.Errorf("quota probe: auth is nil")
	}
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	token := accessToken(auth)
	if token == "" {
		return nil, fmt.Errorf("quota probe: no access token")
	}
	switch strings.ToLower(strings.TrimSpace(auth.Provider)) {
	case "claude":
		return probeClaude(ctx, cfg, auth, token)
	case "codex":
		return probeCodex(ctx, cfg, auth, token)
	case "xai":
		return probeXAI(ctx, cfg, auth, token)
	}
	return nil, fmt.Errorf("quota probe: unsupported provider")
}

// ownHosts lists the hosts a credential's base_url may point to for its token
// to belong to the probed service.
var ownHosts = map[string][]string{
	"claude": {"api.anthropic.com"},
	"codex":  {"chatgpt.com", "chat.openai.com"},
	"xai":    {"api.x.ai", "cli-chat-proxy.grok.com"},
}

// accessToken returns the credential's OAuth access token, or "" when the
// credential is not a subscription OAuth credential of the probed service.
// Configured API keys and credentials with a third-party base_url are skipped so
// their secrets are never sent to the provider's hard-coded endpoints.
func accessToken(auth *coreauth.Auth) string {
	provider := strings.ToLower(strings.TrimSpace(auth.Provider))
	if auth.Attributes != nil && strings.TrimSpace(auth.Attributes["api_key"]) != "" {
		return ""
	}
	baseURLs := []string{}
	if auth.Attributes != nil {
		baseURLs = append(baseURLs, auth.Attributes["base_url"])
	}
	if v, ok := auth.Metadata["base_url"].(string); ok {
		baseURLs = append(baseURLs, v)
	}
	for _, raw := range baseURLs {
		if raw = strings.TrimSpace(raw); raw != "" && !ownHost(provider, raw) {
			return ""
		}
	}
	for _, key := range []string{"access_token", "accessToken"} {
		if v, ok := auth.Metadata[key].(string); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func ownHost(provider, rawURL string) bool {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Scheme != "https" {
		return false
	}
	for _, host := range ownHosts[provider] {
		if strings.EqualFold(parsed.Hostname(), host) {
			return true
		}
	}
	return false
}

func probeClaude(ctx context.Context, cfg *config.Config, auth *coreauth.Auth, token string) (*Result, error) {
	service := claudeauth.NewClaudeAuthWithProxyURL(cfg, auth.ProxyURL)
	result := &Result{ObservedAt: time.Now()}
	profile, errProfile := service.FetchOAuthProfile(ctx, token)
	if errProfile == nil {
		result.Plan = profile.PlanLabel()
	}
	body, errUsage := service.FetchOAuthUsage(ctx, token, true)
	if errUsage != nil {
		if errProfile != nil {
			return nil, errUsage
		}
		return result, nil
	}
	root := gjson.ParseBytes(body)
	for _, w := range []struct{ key, window string }{{"five_hour", coreauth.QuotaWindow5h}, {"seven_day", coreauth.QuotaWindowWeekly}} {
		node := root.Get(w.key)
		if !node.Get("utilization").Exists() || node.Get("utilization").Type != gjson.Number {
			continue
		}
		result.Windows = append(result.Windows, coreauth.QuotaWindowUsage{
			Window: w.window, Utilization: clamp(node.Get("utilization").Float() / 100), ResetsAt: parseTime(node.Get("resets_at").String()),
		})
	}
	if grants := root.Get("cedar_ember.grants"); grants.IsArray() {
		left, valid := 0, true
		grants.ForEach(func(_, grant gjson.Result) bool {
			value := grant.Get("resets_left")
			if value.Type != gjson.Number || value.Int() < 0 {
				valid = false
				return false
			}
			left += int(value.Int())
			return true
		})
		if valid {
			result.ManualResetsLeft = &left
		}
	}
	return result, nil
}

func probeCodex(ctx context.Context, cfg *config.Config, auth *coreauth.Auth, token string) (*Result, error) {
	headers := map[string]string{"Authorization": "Bearer " + token, "User-Agent": codexUA, "Accept": "application/json"}
	if accountID, ok := auth.Metadata["account_id"].(string); ok && strings.TrimSpace(accountID) != "" {
		headers["Chatgpt-Account-Id"] = strings.TrimSpace(accountID)
	}
	body, err := getJSON(ctx, helps.NewProxyAwareHTTPClient(ctx, cfg, auth, 0), codexUsageURL, headers)
	if err != nil {
		return nil, err
	}
	root := gjson.ParseBytes(body)
	result := &Result{ObservedAt: time.Now(), Plan: strings.TrimSpace(root.Get("plan_type").String())}
	credits := root.Get("rate_limit_reset_credits")
	for _, key := range []string{"applicable_available_count", "available_count"} {
		if value := credits.Get(key); value.Type == gjson.Number && value.Int() >= 0 {
			left := int(value.Int())
			result.ManualResetsLeft = &left
			break
		}
	}
	return result, nil
}

func probeXAI(ctx context.Context, cfg *config.Config, auth *coreauth.Auth, token string) (*Result, error) {
	headers := map[string]string{
		"Authorization": "Bearer " + token, "x-xai-token-auth": "xai-grok-cli", "x-grok-client-version": xaiClientVersion,
		"accept": "*/*", "user-agent": fmt.Sprintf("grok-pager/%s grok-shell/%s (macos; aarch64)", xaiClientVersion, xaiClientVersion),
	}
	if sub, ok := auth.Metadata["sub"].(string); ok && strings.TrimSpace(sub) != "" {
		headers["x-userid"] = strings.TrimSpace(sub)
	}
	client := helps.NewProxyAwareHTTPClient(ctx, cfg, auth, 0)
	result := &Result{ObservedAt: time.Now()}

	var tier, display string
	if body, err := getJSON(ctx, client, xaiUserURL, headers); err == nil {
		root := gjson.ParseBytes(body)
		tier = firstString(root, "subscriptionTier", "subscription_tier")
	}
	if body, err := getJSON(ctx, client, xaiSettingsURL, headers); err == nil {
		display = firstString(gjson.ParseBytes(body), "subscription_tier_display", "subscriptionTierDisplay")
	}
	result.Plan = display
	if result.Plan == "" {
		result.Plan = tier
	}

	body, err := getJSON(ctx, client, xaiBillingWeeklyURL, headers)
	if err != nil {
		if result.Plan != "" {
			return result, nil
		}
		return nil, err
	}
	billing := gjson.ParseBytes(body).Get("config")
	period := billing.Get("currentPeriod")
	if !period.Exists() {
		period = billing.Get("current_period")
	}
	percent := billing.Get("creditUsagePercent")
	if !percent.Exists() {
		percent = billing.Get("credit_usage_percent")
	}
	// Only the weekly credit period is a usage window; monthly spend is not.
	periodWeekly := !period.Exists() || strings.Contains(strings.ToLower(period.Get("type").String()), "weekly")
	if value, ok := number(percent); ok && periodWeekly {
		result.Windows = append(result.Windows, coreauth.QuotaWindowUsage{
			Window: coreauth.QuotaWindowWeekly, Utilization: clamp(value / 100), ResetsAt: parseTime(period.Get("end").String()),
		})
	}
	return result, nil
}

func getJSON(ctx context.Context, client *http.Client, url string, headers map[string]string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("quota probe request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("quota probe status %d", resp.StatusCode)
	}
	if !json.Valid(body) {
		return nil, fmt.Errorf("quota probe returned invalid JSON")
	}
	return body, nil
}

// number reads a JSON number or numeric string.
func number(value gjson.Result) (float64, bool) {
	switch value.Type {
	case gjson.Number:
		return value.Float(), true
	case gjson.String:
		var parsed float64
		if _, err := fmt.Sscanf(strings.TrimSpace(value.String()), "%g", &parsed); err == nil && !math.IsNaN(parsed) {
			return parsed, true
		}
	}
	return 0, false
}

func firstString(root gjson.Result, keys ...string) string {
	for _, key := range keys {
		if v := strings.TrimSpace(root.Get(key).String()); v != "" {
			return v
		}
	}
	return ""
}

func parseTime(value string) time.Time {
	parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(value))
	if err != nil {
		return time.Time{}
	}
	return parsed
}

func clamp(value float64) float64 { return math.Min(1, math.Max(0, value)) }
