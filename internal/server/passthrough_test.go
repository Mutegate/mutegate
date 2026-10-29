package server

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mutegate/mutegate/internal/config"
	"github.com/mutegate/mutegate/internal/inspector"
	"github.com/mutegate/mutegate/internal/storage"
)

// What Claude Code needs of a gateway, per its gateway guide: every
// anthropic-* header and the query reach Anthropic as sent, the headers it
// retries by come back, and an upstream error arrives word for word.

type seenRequest struct {
	path, query string
	header      http.Header
	body        string
}

// passthroughRouter is a gateway in front of a fake Anthropic that records
// what reached it and answers with reply.
func passthroughRouter(t *testing.T, policy inspector.Policy, reply func(w http.ResponseWriter)) (http.Handler, *[]seenRequest, *fakeLogStore) {
	t.Helper()
	var seen []seenRequest
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		seen = append(seen, seenRequest{r.URL.Path, r.URL.RawQuery, r.Header.Clone(), string(b)})
		reply(w)
	}))
	t.Cleanup(upstream.Close)

	ks := config.NewRuntimeStore("ap-test").KeyStore()
	if err := ks.SetProviderKeys(context.Background(), storage.DefaultOrgID, map[string]string{"anthropic": "sk-ant-upstream"}); err != nil {
		t.Fatal(err)
	}
	logs := &fakeLogStore{}
	h := Routes(Options{
		KeyStore:         ks,
		LogStore:         logs,
		DLPStore:         storage.NewMemDLPStore(100),
		PolicyStore:      storage.NewMemPolicyStore(policy),
		Inspector:        inspector.New(),
		DLPPolicy:        policy,
		AnthropicBaseURL: upstream.URL,
		AdminAPIKey:      "admin-test",
		Logger:           slog.Default(),
	})
	return h, &seen, logs
}

func claudeCodeRequest(path, body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer ap-test")
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Add("anthropic-beta", "claude-code-20250219")
	req.Header.Add("anthropic-beta", "interleaved-thinking-2025-05-14,context-management-2025-06-27")
	req.Header.Set("x-claude-code-session-id", "s-1")
	return req
}

const okMessage = `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":3,"output_tokens":1}}`

func TestMessagesForwardsEveryAnthropicHeaderAndTheQuery(t *testing.T) {
	h, seen, _ := passthroughRouter(t, inspector.DefaultPolicy(), func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(okMessage))
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, claudeCodeRequest("/v1/messages?beta=true",
		`{"model":"claude-sonnet-4-5","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	got := (*seen)[0]
	if got.path != "/v1/messages" || got.query != "beta=true" {
		t.Errorf("upstream got %s?%s, want /v1/messages?beta=true", got.path, got.query)
	}
	betas := got.header.Values("anthropic-beta")
	if len(betas) != 2 || betas[0] != "claude-code-20250219" || !strings.Contains(betas[1], "context-management") {
		t.Errorf("anthropic-beta reached Anthropic as %q, want both headers", betas)
	}
	if got.header.Get("x-api-key") != "sk-ant-upstream" || got.header.Get("Authorization") != "" {
		t.Errorf("credentials upstream: x-api-key %q, Authorization %q; want only the provider key",
			got.header.Get("x-api-key"), got.header.Get("Authorization"))
	}
	if got.header.Get("x-claude-code-session-id") != "" {
		t.Error("the gateway's own attribution headers went upstream")
	}
}

func TestMessagesReturnsTheHeadersAgentsRetryBy(t *testing.T) {
	h, _, _ := passthroughRouter(t, inspector.DefaultPolicy(), func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("retry-after", "7")
		w.Header().Set("x-should-retry", "true")
		w.Header().Set("request-id", "req_1")
		w.Header().Set("anthropic-ratelimit-unified-status", "allowed_warning")
		w.Header().Set("set-cookie", "upstream=1")
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"Number of request tokens has exceeded your per-minute rate limit"}}`))
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, claudeCodeRequest("/v1/messages",
		`{"model":"claude-sonnet-4-5","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want the upstream's 429", rec.Code)
	}
	for name, want := range map[string]string{
		"retry-after": "7", "x-should-retry": "true", "request-id": "req_1",
		"anthropic-ratelimit-unified-status": "allowed_warning",
	} {
		if got := rec.Header().Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if rec.Header().Get("set-cookie") != "" {
		t.Error("an upstream cookie reached the client")
	}
	if !strings.Contains(rec.Body.String(), "Number of request tokens has exceeded") {
		t.Errorf("the upstream error was reworded: %s", rec.Body.String())
	}
}

// With response scanning on, an upstream error still goes back untouched:
// Claude Code reads its wording to decide how to recover.
func TestMessagesLeavesUpstreamErrorsAloneWhenScanningResponses(t *testing.T) {
	policy := inspector.DefaultPolicy()
	policy.ScanResponses = true
	upstreamErr := `{"type":"error","error":{"type":"invalid_request_error","message":"prompt is too long: 210000 tokens > 200000 maximum, contact ops@example.com"}}`
	h, _, _ := passthroughRouter(t, policy, func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(upstreamErr))
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, claudeCodeRequest("/v1/messages",
		`{"model":"claude-sonnet-4-5","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`))
	if rec.Code != http.StatusBadRequest || rec.Body.String() != upstreamErr {
		t.Errorf("got %d %s; want the upstream's 400 unchanged", rec.Code, rec.Body.String())
	}
}

func TestCountTokensIsForwardedAndScanned(t *testing.T) {
	h, seen, logs := passthroughRouter(t, inspector.DefaultPolicy(), func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"input_tokens":42}`))
	})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, claudeCodeRequest("/v1/messages/count_tokens?beta=true",
		`{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"how big is this?"}]}`))
	if rec.Code != http.StatusOK || rec.Body.String() != `{"input_tokens":42}` {
		t.Fatalf("got %d %s", rec.Code, rec.Body.String())
	}
	if got := (*seen)[0]; got.path != "/v1/messages/count_tokens" || len(got.header.Values("anthropic-beta")) != 2 {
		t.Errorf("upstream got %s with betas %q", got.path, got.header.Values("anthropic-beta"))
	}
	if len(logs.entries) != 0 {
		t.Errorf("counting tokens wrote %d usage rows; it is not a model call", len(logs.entries))
	}

	// Counting sends the conversation to Anthropic, so a secret stops here.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, claudeCodeRequest("/v1/messages/count_tokens",
		`{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"deploy with AKIAIOSFODNN7EXAMPLE"}]}`))
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403 for a secret", rec.Code)
	}
	if len(*seen) != 1 {
		t.Error("the request with a secret reached Anthropic")
	}
}
