# Mutegate — roadmap: the pivot to a self-hosted DLP gateway for AI agents

> Recorded: July 2026. The road to 1.0 (October 2026) is at the end:
> [Toward 1.0](#toward-10).
> Positioning: **a proxy between a team's AI agents and LLM providers that
> inspects traffic for leaked secrets and PII before it reaches the cloud,
> keeps an incident log and tracks spend. One Go binary, self-hosted.**
>
> Buyer: the CTO, tech lead or DevSecOps of a team running AI agents.
> Integration: change `base_url`. Niche: DLP for **API and agent** traffic —
> not browser traffic, where the enterprise players already are (Netskope,
> Palo Alto, Lasso).

---

## Epic 0 — Security hardening (blocks everything else)

A security product cannot be the leaky one. Review findings that had to be
fixed:

- [x] **Auth in no-DB mode**: `runtimeKeyStore.GetByMutegateKey` accepted any
      bearer token (`internal/config/runtime.go`). Introduce a real mutegate
      key (generated at startup or from `MUTEGATE_KEY`) and compare against it.
- [x] **Admin closed by default**: an empty `ADMIN_API_KEY` meant no auth at
      all (`internal/server/handlers.go: requireAdmin`). Generate a key at
      startup and log it, or refuse to start without one in production.
- [x] **CORS**: drop `Access-Control-Allow-Origin: *` for admin routes
      (allowlist / same-origin).
- [x] Provider keys in PostgreSQL: encrypted at rest (AES-256-GCM,
      `MUTEGATE_ENCRYPTION_KEY`); Mutegate keys as sha256 hash plus hint, with
      the old schema migrating in place.
- [x] README ↔ environment drift: `OPENAI_API_KEY`/`ANTHROPIC_API_KEY`/
      `GROQ_API_KEY` were documented but never read in `config.Load()` — wire
      them up.
- [x] Timeouts on the upstream `http.Client` in every provider.

**Done when:** no endpoint can be reached without a valid key, no key sits in
plaintext, and the README promises nothing that does not exist. ~2–3 days.

## Epic 1 — Open-source hygiene

- [x] LICENSE (Apache 2.0 — compatible with enterprise use).
- [x] CI (GitHub Actions): build, `go vet`, tests, front-end lint.
- [x] First tests: pricing, provider routing, Anthropic translation, auth.
- [x] `/ready` checks that PostgreSQL is reachable.
- [x] `POST /admin/keys` (documented in the README, missing from the code).

**Done when:** CI is green on pull requests and the critical paths are covered.
~2–3 days, partly in parallel with Epic 0.

## Epic 2 — The DLP engine (`internal/inspector`), the core of the product

- [x] Package `inspector`: `Scan(text) []Finding`, `Apply(policy, findings) Verdict`.
- [x] Regex detectors (no ML in the MVP):
  - Secrets: AWS keys, GitHub/GitLab tokens, private keys (`-----BEGIN`), JWTs,
    generic `api_key=` — porting the gitleaks rules.
  - PII: email addresses, phone numbers, card numbers (Luhn), IBANs.
  - Custom: user-supplied regexes and stop-words.
- [x] Actions: `block` (403 with an explanation, upstream never called),
      `redact` (replaced with `[REDACTED:rule:n]`), `alert` (pass through,
      record it).
- [x] Wired into the pipeline: `handleChatCompletions` after reading the body,
      before `resolveProviderForKey`. Scans `messages[].content`.
- [x] The MVP scans requests only (not responses) and chat completions only.

**Done when:** a request carrying an AWS key is blocked or redacted according
to the policy and the event is recorded; inspection latency under 5 ms on a
typical request (benchmarked). ~1 week.

## Epic 3 — The DLP event log

- [x] Table `dlp_events` in PostgreSQL: ts, key_id, model, provider, rule,
      action, masked_sample. **The sensitive content itself is never stored.**
- [x] `storage.DLPStore` (interface plus an in-memory ring buffer; PostgreSQL
      above).
- [x] API: `GET /admin/dlp/events` (filters: action, rule, key_id, period,
      limit), `GET /admin/dlp/summary` (counters for the KPIs).

**Done when:** events are visible through the API with filtering. ~2–3 days.

## Epic 4 — Policies

- [x] The `Policy` model: detector groups (secrets/pii/custom) → action, plus a
      list of custom rules. Bound to a Mutegate key, with a default policy.
- [x] Storage: a `dlp_policies` table (JSONB) in PostgreSQL plus an in-memory
      variant.
- [x] API: `GET /admin/policies`, `PUT /admin/policies/default|keys/{id}`,
      `DELETE /admin/policies/keys/{id}`, `POST /admin/policies/test` — a dry
      run answering "what would happen to this text", including with an unsaved
      policy.

**Done when:** different keys work under different policies without a restart.
~3–4 days.

## Epic 5 — The admin console (`web/`)

Design prompt: [`DESIGN_PROMPT.md`](DESIGN_PROMPT.md).

- [x] Tabs: Overview (dashboard plus DLP KPIs), DLP Events (the incident feed
      with filters and a detail drawer), Policies (group toggles, actions,
      custom rules, a live dry-run preview), Settings/Keys, and a playground
      chat.
- [x] States: skeleton, empty, error; dark and light themes.

**Done when:** the whole loop works through the UI — set a policy, have an
agent send a secret, see the incident in the feed. ~1 week.

## Epic 6 — Alerts

- [x] A webhook on `block` and `alert` events (generic JSON plus Slack and
      Telegram templates).
- [x] Configured through the environment and the admin API
      (`GET/PUT /admin/alerts`, `POST /admin/alerts/test`), with per key+rule
      debouncing against storms and asynchronous delivery off the request path.
      The UI stayed in the backlog.

**Done when:** a block event reaches Slack in under five seconds. ✓ (async worker)

## Epic 7 — Positioning and launch

- [x] Rewrite the README around the DLP gateway: hero, console screenshots, a
      two-minute quickstart ("docker run → curl a secret → 403"), verified
      against the real Docker image.
- [x] `examples/`: curl, openai-python, openai-node, seed-demo, and how to
      point coding agents at the gateway (`OPENAI_BASE_URL`).
- [x] A demo seed: `examples/seed-demo.sh`, used for the screenshots in
      `docs/screenshots/`.
- [x] Launch post drafts: [`LAUNCH.md`](LAUNCH.md) (Show HN, r/selfhosted,
      r/devops) plus a pre-publication checklist.
- [x] Publication: the repository is public, releases v0.1.0 → v0.2.0 ship
      binaries and a multi-arch image on ghcr.
- [ ] Sending the posts ([`LAUNCH.md`](LAUNCH.md)) — the owner's manual step.

**Done when:** a developer who has never seen the project gets from the README
to their first caught incident in ten minutes. ~3–4 days.

---

## Shipped after the MVP

- **Response scanning**, streaming included — a sliding window across SSE
  chunks (`scan_responses`).
- NER detectors for names and addresses with a local model, EN and RU — a
  separate service beside the gateway (`ner`, see [`ner/README.md`](../ner/README.md)).
- `/v1/responses` (the OpenAI Responses API) and custom providers (DeepSeek,
  Qwen, Ollama/vLLM — routed by model prefix).
- Per-key rate limits and budgets; agent and session attribution.
- Prometheus `/metrics`.
- An alerts UI and the "what would have been blocked" report.
- The OpenAI chat API in front of Claude: tools, images, streaming, usage
  and cost; Claude Code attributed with no setup.
- Accounts, organizations and roles; OAuth sign-in; connection guides.

## Backlog (on demand)

- De-redaction: restoring placeholders in the answer for redact mode.
- More endpoints: `/v1/embeddings`; more providers (Gemini, Bedrock).
- Versioned database migrations (today: `ALTER TABLE ... IF NOT EXISTS`).
- NER languages beyond EN and RU; NER over streamed responses.
- SSO/OIDC for the console (enterprise demand).

## Order and milestones

```
Epic 0 ──► Epic 2 ──► Epic 3 ──► Epic 4 ──► Epic 5 ──► Epic 6 ──► Epic 7
Epic 1 ──┘ (in parallel with 0)

M1 (end of week 1): a secure gateway + CI + the first caught secret (curl)
M2 (end of week 2): policies + the events API — the product works headless
M3 (end of week 3): UI + alerts — the complete MVP
M4 (~day 25):       README, landing, examples — public launch
```

---

## Toward 1.0

Recorded October 2026, after a review of what changed in the APIs agents use;
updated November 2026. Three shifts set the order:

1. **Agents moved to the Responses API.** OpenAI's newest models call tools
   only through it, and Codex speaks nothing else (plus a WebSocket mode).
   A Responses request carries diffs, shell output, files inside tool results
   and MCP calls — far more than Mutegate scans today.
2. **A gateway must not rewrite what it forwards.** Claude Code's
   [gateway guide](https://code.claude.com/docs/en/llm-gateway-protocol)
   says "inspect without modifying", and the newest Claude models reject a
   request whose `system`, `tools` or earlier messages differ from what
   produced their thinking
   ([preserved thinking](https://platform.claude.com/docs/en/build-with-claude/preserved-thinking)) —
   since November for every account on Fable and Mythos 5.2. OpenAI's
   GPT-6.2 drops its reasoning after an edited prefix. Redaction has to be
   deterministic, and replayed history left alone.
3. **More data leaves without passing the gateway.** Providers call MCP
   servers, fetch pages and run code themselves; files go up through
   `/v1/files`. What the gateway does see is the request that asks for it —
   the `tools` list — so that is where control goes.

### v0.5 — works with every agent, and breaks none

- [ ] Fixes: repeated `anthropic-beta` headers are dropped; a held-back
      `tool_use` tail is flushed as text; upstream response headers
      (`retry-after`, `x-should-retry`, rate limits, request id) are lost;
      no body size limit on `/v1/*`; SSE lines over 64 KiB end the stream;
      redacted Responses text may still arrive in `output_item.done`.
- [ ] Pass-through fidelity: every `anthropic-*` header and unknown body field
      verbatim, upstream error bodies unchanged, `/v1/messages/count_tokens`.
- [ ] DLP that does not break the conversation: an inspect-only mode (block,
      never rewrite) as the default for agent traffic; redaction made
      deterministic and stable across turns; replayed assistant turns and
      thinking never touched; responses blocked rather than redacted.
- [ ] The whole Responses API: every input and output item type, walked
      recursively with a policy for unknown types; every streaming event;
      `GET /v1/responses/{id}`; a clear refusal of WebSocket mode until it is
      proxied (Codex: `supports_websockets = false`).
- [ ] Recursive scanning of every string a request carries — nested tool
      results, `tool_use` input in history, Anthropic `tools`, documents.
- [ ] Claude Code attribution beyond the session: subagent, parent and
      request class.
- [ ] Codex as it ships: its built-in provider opens a WebSocket by default,
      so refuse the upgrade at once (or proxy it) and document a custom
      provider with `supports_websockets = false`; serve `GET /models` in
      Codex's catalog shape; answer a block with `inference_hook_denied`,
      which Codex treats as final rather than retrying.
- [ ] Anthropic API `2026-10-21` and message threads: the key as
      `Authorization: Bearer`, bodies up to 128 MB, no sampling fields added
      when translating; a thread keeps its history at Anthropic, so scan and
      redact the request that creates it, and never switch upstream keys
      within one.
- [ ] Claude Code auto mode behind a gateway: the server-side safety check
      travels in the final `message_delta` — response scanning must leave it
      and tool_use ids intact. An end-to-end test that drives the real agent.
- [ ] Cost: cache writes by TTL (billed on OpenAI since GPT-5.6), reasoning
      tokens, the model in the response rather than the one requested.

### v0.6 — DLP that knows what an agent is

- [ ] Server-side tools and MCP governed through `tools`: allowlists for
      `server_url` and `mcp_servers`, approvals enforced, tunnels denied;
      credentials inside those definitions treated as expected, not leaks.
- [ ] Tool definitions pinned: a changed description or schema ("rug pull")
      is an incident; descriptions scanned for injected instructions.
- [ ] Secrets v2: the Betterleaks rule set (MIT, Go), connection strings and
      cloud credentials; canary tokens that make any sighting an incident.
- [ ] Coding-agent policies: file paths in Read/Bash results (`.env`,
      `*.pem`, `secrets/`), a ceiling on how much code leaves per key.
- [ ] Prompt-injection detection on tool results (a small local model beside
      NER), alert-only at first.
- [ ] Skills pinned and scanned like tool definitions: MCP skills (SEP-2640)
      and Agent Skills carry instructions the model follows.
- [ ] OIDC sign-in moved up from v0.7: Codex (`gateway_oauth`) and Claude
      Code now expect to sign in to a gateway themselves.

### v0.7 — MCP, and what a company asks for

- [ ] An MCP DLP proxy (#55): Streamable HTTP, specs 2025-11-25 and
      2026-07-28 (the next one is due 2027-03-30); arguments and results
      scanned; tool allowlists; token audience checked, no passthrough, no
      headers across redirects; the draft files extension governed, since its
      uploads leave outside the JSON-RPC stream.
- [ ] IdP groups mapped to policies.
- [ ] Mutegate as an Anthropic inference hook: allow/deny for claude.ai,
      Cowork and Claude Code in Enterprise organizations, without a proxy.
- [ ] OpenTelemetry (GenAI semantic conventions) and an audit stream for a
      SIEM, with retention of six months or more.
- [ ] Signed images, an SBOM, and a page mapping Mutegate to the OWASP
      LLM 2026, Agentic and MCP Top 10s.
- [ ] Gemini through its OpenAI-compatible layer, then the native API for
      Gemini CLI; Anthropic-compatible upstreams (DeepSeek, Kimi, MiniMax,
      OpenRouter, vLLM, Ollama) for `/v1/messages`.

### Not on the road

- Routing across a hundred providers, semantic caching, model marketplaces.
- Hosting or cataloguing MCP servers.
- Toxicity and hallucination filters, or an LLM judge.
- Browser and endpoint DLP.
- Storing raw prompts; verifying found secrets against their provider by
  default; proxying realtime audio (blocked, not scanned).
