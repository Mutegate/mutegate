package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/mutegate/mutegate/internal/inspector"
	"github.com/mutegate/mutegate/internal/pricing"
	"github.com/mutegate/mutegate/internal/provider/anthropic"
	"github.com/mutegate/mutegate/internal/storage"
)

// extractClientToken returns the caller's Mutegate key. Anthropic clients
// (including Claude Code) authenticate with x-api-key rather than a Bearer
// header, so both are accepted.
func extractClientToken(r *http.Request) string {
	if t := extractBearerToken(r); t != "" {
		return t
	}
	return strings.TrimSpace(r.Header.Get("x-api-key"))
}

// writeAnthropicError emits an Anthropic-shaped error so SDKs surface it the
// way they surface upstream errors.
func writeAnthropicError(w http.ResponseWriter, status int, errType, message string, extra map[string]any) {
	payload := map[string]any{
		"type":  "error",
		"error": map[string]any{"type": errType, "message": message},
	}
	for k, v := range extra {
		payload[k] = v
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(payload)
}

// anthropicRequest is a native Messages API call that has been
// authenticated and scanned: what is left is to send it.
type anthropicRequest struct {
	key    *storage.Key
	meta   reqMeta
	policy inspector.Policy
	model  string
	stream bool
	body   []byte
}

// acceptAnthropic does what /v1/messages and /v1/messages/count_tokens share:
// the caller's key, its limits when asked to, and DLP on everything the body
// would carry to Anthropic. It answers the client itself and returns false
// when the request goes no further.
func (h *Handlers) acceptAnthropic(w http.ResponseWriter, r *http.Request, limits bool) (*anthropicRequest, bool) {
	token := extractClientToken(r)
	if token == "" {
		writeAnthropicError(w, http.StatusUnauthorized, "authentication_error",
			"missing API key: send it as 'x-api-key' or 'Authorization: Bearer'", nil)
		return nil, false
	}
	key, err := h.KeyStore.GetByMutegateKey(r.Context(), token)
	if err != nil {
		if err == storage.ErrKeyNotFound {
			writeAnthropicError(w, http.StatusUnauthorized, "authentication_error", "invalid API key", nil)
			return nil, false
		}
		h.Logger.Error("key lookup failed", "err", err)
		writeAnthropicError(w, http.StatusInternalServerError, "api_error", "internal error", nil)
		return nil, false
	}

	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "failed to read body", nil)
		return nil, false
	}

	var peek struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	_ = json.Unmarshal(bodyBytes, &peek)
	model := peek.Model
	if model == "" {
		model = "claude"
	}

	meta := h.metaFor(r, key.OrgID, key.ID, model)
	// The native Messages API only ever speaks to Anthropic.
	meta.provider = string(storage.KindAnthropic)
	if limits && !h.enforceLimits(w, r, meta) {
		return nil, false
	}

	// DLP: scan outbound content before anything leaves the network.
	policy := h.policyFor(r.Context(), key.OrgID, key.ID)
	if h.Inspector != nil {
		res := h.inspect(r.Context()).ScanMessagesRequest(bodyBytes, policy)
		h.noteNER(res.NERError)
		h.recordDLPEvents(r.Context(), meta, res.Findings)
		h.recordSuppressed(r.Context(), meta, res.Suppressed)
		if res.Verdict == inspector.ActionBlock {
			rules := blockedRules(res.Findings)
			writeAnthropicError(w, http.StatusForbidden, "permission_error",
				"request blocked by DLP policy: sensitive data detected ("+strings.Join(rules, ", ")+")",
				map[string]any{"mutegate": map[string]any{"blocked_by": "dlp", "rules": rules}})
			return nil, false
		}
		bodyBytes = res.Body
	}
	return &anthropicRequest{key: key, meta: meta, policy: policy, model: model, stream: peek.Stream, body: bodyBytes}, true
}

// forwardAnthropic sends an accepted request to the key's Anthropic upstream.
// When it returns no response it has answered the client already; proxyErr
// is then set if Anthropic could not be reached, as opposed to the key having
// no Anthropic provider at all.
func (h *Handlers) forwardAnthropic(w http.ResponseWriter, r *http.Request, req *anthropicRequest, path string) (resp *http.Response, proxyErr error) {
	up, err := h.upstreamFor(r.Context(), req.key, req.model, storage.KindAnthropic)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error",
			upstreamErrorText("anthropic", err), nil)
		return nil, nil
	}
	resp, err = anthropic.New(up.BaseURL, up.APIKey).WithHTTPClient(up.Client).
		Forward(r.Context(), path, r.URL.RawQuery, req.body, r.Header)
	if err != nil {
		writeAnthropicError(w, http.StatusBadGateway, "api_error", "failed to proxy request", nil)
		return nil, err
	}
	return resp, nil
}

// copyAgentHeaders passes on the response headers an agent acts on: when to
// retry and whether to, its rate limits, and the request id support asks for.
// Claude Code reads all of them; the rest of the upstream's headers stay here.
func copyAgentHeaders(dst, src http.Header) {
	for name, values := range src {
		n := strings.ToLower(name)
		if n == "retry-after" || n == "x-should-retry" || n == "request-id" ||
			strings.HasPrefix(n, "anthropic-ratelimit-") {
			dst[name] = append([]string(nil), values...)
		}
	}
}

// handleMessages proxies the native Anthropic Messages API (POST /v1/messages)
// with DLP scanning, so agents that speak Anthropic natively — Claude Code
// among them — can be protected by pointing ANTHROPIC_BASE_URL at Mutegate.
func (h *Handlers) handleMessages(w http.ResponseWriter, r *http.Request) {
	req, ok := h.acceptAnthropic(w, r, true)
	if !ok {
		return
	}
	meta, policy := req.meta, req.policy

	start := time.Now()
	resp, proxyErr := h.forwardAnthropic(w, r, req, "/v1/messages")
	if resp == nil {
		if proxyErr != nil {
			h.recordUsage(meta, pricing.Usage{}, http.StatusBadGateway, time.Since(start), proxyErr.Error())
		}
		return
	}
	defer resp.Body.Close()
	upstream, status := resp.Body, resp.StatusCode
	respCT := resp.Header.Get("Content-Type")
	if respCT == "" {
		respCT = "application/json"
	}
	copyAgentHeaders(w.Header(), resp.Header)

	rs := h.newRespScanner(meta, policy)

	if flusher, ok := w.(http.Flusher); ok && isStreaming(respCT) {
		w.Header().Set("Content-Type", respCT)
		w.WriteHeader(status)
		usage := h.streamMessages(w, flusher, upstream, rs)
		if rs != nil {
			rs.record(context.WithoutCancel(r.Context()))
		}
		h.recordUsage(meta, usage, status, time.Since(start), "")
		return
	}

	data, readErr := io.ReadAll(upstream)
	usage := anthropicUsageFromJSON(data)
	errStr := ""
	if readErr != nil {
		errStr = readErr.Error()
	}

	// An upstream error goes back word for word: Claude Code decides whether
	// and how to retry by reading it.
	if rs != nil && status < http.StatusBadRequest {
		res := h.inspect(r.Context()).ScanMessagesResponse(data, policy)
		h.noteNER(res.NERError)
		h.recordFindings(r.Context(), meta, res.Findings, "", storage.DirectionResponse)
		h.recordFindings(r.Context(), meta, res.Suppressed, "suppressed", storage.DirectionResponse)
		if res.Verdict == inspector.ActionBlock {
			rules := blockedRules(res.Findings)
			writeAnthropicError(w, http.StatusForbidden, "permission_error",
				"response blocked by DLP policy: sensitive data detected ("+strings.Join(rules, ", ")+")",
				map[string]any{"mutegate": map[string]any{
					"blocked_by": "dlp", "direction": "response", "rules": rules}})
			h.recordUsage(meta, usage, http.StatusForbidden, time.Since(start), errStr)
			return
		}
		data = res.Body
	}

	w.Header().Set("Content-Type", respCT)
	w.WriteHeader(status)
	w.Write(data)
	h.recordUsage(meta, usage, status, time.Since(start), errStr)
}

// handleCountTokens serves POST /v1/messages/count_tokens, which Claude Code
// calls for exact context sizes. Counting sends the whole conversation to
// Anthropic, so it is scanned like a message: a secret is blocked here too.
// It costs nothing and is not a model call, so no budget or rate limit is
// spent on it and no usage is recorded.
func (h *Handlers) handleCountTokens(w http.ResponseWriter, r *http.Request) {
	req, ok := h.acceptAnthropic(w, r, false)
	if !ok {
		return
	}
	resp, _ := h.forwardAnthropic(w, r, req, "/v1/messages/count_tokens")
	if resp == nil {
		return
	}
	defer resp.Body.Close()
	copyAgentHeaders(w.Header(), resp.Header)
	ct := resp.Header.Get("Content-Type")
	if ct == "" {
		ct = "application/json"
	}
	w.Header().Set("Content-Type", ct)
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

// blockedRules lists the distinct rules whose verdict was block.
func blockedRules(findings []inspector.Finding) []string {
	rules := make([]string, 0, len(findings))
	seen := map[string]bool{}
	for _, f := range findings {
		if f.Action == inspector.ActionBlock && !seen[f.Rule] {
			seen[f.Rule] = true
			rules = append(rules, f.Rule)
		}
	}
	return rules
}

// streamMessages relays the SSE stream and pulls token usage out of the
// Anthropic event flow (message_start carries the input and cache counts,
// message_delta the final output count).
func (h *Handlers) streamMessages(w io.Writer, flusher http.Flusher, upstream io.Reader,
	rs *respScanner,
) pricing.Usage {
	var usage anthropic.Usage
	var filter func(string) (string, bool)
	if rs != nil {
		filter = rs.messagesFilter()
	}
	streamSSEFiltered(w, flusher, upstream, func(data []byte) {
		var evt struct {
			Type    string `json:"type"`
			Message *struct {
				Usage *anthropic.Usage `json:"usage"`
			} `json:"message"`
			Usage *anthropic.Usage `json:"usage"`
		}
		if json.Unmarshal(data, &evt) != nil {
			return
		}
		switch evt.Type {
		case "message_start":
			if evt.Message != nil && evt.Message.Usage != nil {
				usage = *evt.Message.Usage
			}
		case "message_delta":
			if evt.Usage != nil {
				usage.Merge(*evt.Usage)
			}
		}
	}, filter)
	return usage.Tokens()
}

// anthropicUsageFromJSON reads usage off a non-streaming Messages response.
func anthropicUsageFromJSON(body []byte) pricing.Usage {
	var resp struct {
		Usage anthropic.Usage `json:"usage"`
	}
	_ = json.Unmarshal(body, &resp)
	return resp.Usage.Tokens()
}
