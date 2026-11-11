package test

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

func chatBody(prompt string, stream bool) string {
	return fmt.Sprintf(`{"model":"grok-fixture-chat","stream":%t,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":%q}]}`, stream, prompt)
}

func TestAccountingReconciliationScenario(t *testing.T) {
	chat := contracts[0]
	upstream := newFakeUpstream(t, func(w http.ResponseWriter, _ *http.Request, call upstreamCall) bool {
		switch {
		case call.Account == "acct-A":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"message":"quota","type":"insufficient_quota","code":"insufficient_quota"}}`))
		case strings.Contains(call.Body, "interrupt me"):
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprint(w, strings.Join(strings.SplitAfter(chat.stream.body, "\n\n")[:2], ""))
			w.(http.Flusher).Flush()
			if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
				_ = conn.Close()
			}
		default:
			return false
		}
		return true
	})
	accounts := []upstreamAccount{
		{id: "rec-A", provider: chat.provider, apiKey: "acct-A", models: []string{chat.model}, priority: 2},
		{id: "rec-B", provider: chat.provider, apiKey: "acct-B", models: []string{chat.model}, priority: 1},
	}
	recorder := recordAccounting(t)

	first := newGateway(t, gatewayOptions{mode: accountingEnabled, upstreamURL: upstream.server.URL, accounts: accounts})

	failover := post(t, context.Background(), first.URL+chat.path, chatBody("failover", false), nil)
	if failover.Status != http.StatusOK {
		t.Fatalf("failover must hide the first account's quota error: %d %s", failover.Status, failover.Body)
	}
	post(t, context.Background(), first.URL+chat.path, chatBody("plain one", false), nil)
	interrupted := post(t, context.Background(), first.URL+chat.path, chatBody("interrupt me", true), nil)
	t.Logf("interrupted stream reached the client as status %d with %d lines", interrupted.Status, interrupted.Chunks)
	post(t, context.Background(), first.URL+chat.path, chatBody("plain two", true), nil)

	events := first.durableEvents(t, recorder.waitEvents(t, 5))
	byTrace := map[string][]usage.AccountingEvent{}
	var successes, unexportable []usage.AccountingEvent
	for _, event := range events {
		byTrace[event.TraceID] = append(byTrace[event.TraceID], event)
		if event.Status == "succeeded" {
			successes = append(successes, event)
		} else {
			unexportable = append(unexportable, event)
		}
	}
	if len(events) != 5 || len(successes) != 3 || len(unexportable) != 2 {
		t.Fatalf("want 3 successes and 2 non-success attempts from 4 requests, got %d events: %+v", len(events), events)
	}
	var failoverTrace []usage.AccountingEvent
	for _, group := range byTrace {
		if len(group) == 2 {
			failoverTrace = group
		}
	}
	if len(failoverTrace) != 2 || failoverTrace[0].ExecutionID == failoverTrace[1].ExecutionID {
		t.Fatalf("failover must yield two attempts under one trace with distinct execution IDs: %+v", byTrace)
	}
}
