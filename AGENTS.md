# Mutegate

A self-hosted DLP gateway for AI agents. It sits between agents (Claude Code,
Codex, Cursor, SDK apps) and LLM providers, scans what they send — prompts,
tool calls, tool results — for secrets and personal data, and blocks, redacts
or records it before it leaves. Self-hosted only: there is no hosted service.

## Layout

- `cmd/mutegate` — the gateway binary
- `internal/server` — HTTP routes: `/v1/chat/completions`, `/v1/responses`,
  `/v1/messages`, `/v1/models`, the console's `/api` and `/admin`
- `internal/inspector` — the DLP engine: detectors, policies, request and
  response scanning, streaming
- `internal/provider` — upstream clients (OpenAI, Anthropic with chat
  translation, Groq, OpenAI-compatible, Jev)
- `internal/pricing` — the cost catalog, generated from LiteLLM
  (`go generate ./internal/pricing`)
- `internal/storage` — Postgres and in-memory stores
- `web/` — the React console; `ner/` — the optional local NER service
- `docs/` — user documentation; [`docs/ROADMAP.md`](docs/ROADMAP.md) — what
  comes next

## Working on it

- Go 1.26, gofmt, the standard library first; `go test ./...` must pass.
- Console: `cd web && npm run lint && npm run build`. `docs/CONNECT.md` is
  generated from `web/src/connect/guides.ts` by `npm run docs:connect`.
- Forward what the gateway does not need to understand: unknown body fields
  and `anthropic-*` headers go upstream verbatim. Rewriting a request breaks
  agents (Claude Code's gateway guide, preserved thinking) — inspect, and
  rewrite only where a policy says redact.
- Never store or log raw prompt content or secrets: incidents keep masked
  samples only.
- Old names still accepted for compatibility: `APERTURE_*` environment
  variables, `X-Aperture-*` headers.
