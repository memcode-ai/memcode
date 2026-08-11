# The memcode wire protocol

This documents the two wire surfaces third parties can build against. The Go
types in this repo are the source implementation; this file is documentation,
not a second source of truth.

## 1. The serving wire: OpenAI-compatible /v1

Every backend memcode speaks to — the hosted gateway, Ollama, any compat
endpoint — serves standard OpenAI chat completions:

- `POST /v1/chat/completions` (streamed and non-streamed)
- `GET  /v1/models`

The `model` field always carries a concrete catalog label; there is no
server-side "auto". Four memcode extensions ride the standard shapes, and any
compat client can ignore them (implementation: `internal/providers/compat/wire.go`):

1. Two-system convention: the first `system` message is the stable
   (prompt-cacheable) prefix, the second is the per-turn volatile suffix.
2. `memcode_opaque` on assistant messages: vendor reasoning blocks round-trip
   verbatim (Anthropic thinking signatures, OpenAI Responses reasoning items).
3. `memcode` object on responses / final stream chunks: serving telemetry the
   footer reads (`byok`, `fallback_reason`, `context_window`, `input_budget`,
   `pool`, `search_count`).
4. `memcode_billing` on requests: the enforced billing lane —
   `byok_preferred` | `byok_only` | `credits`. The gateway enforces, never
   chooses.

Session affinity rides the standard `user` field.

### Typed errors

Machine-readable `error.code` values the client recovery policy keys on:

| HTTP | code | meaning |
|---|---|---|
| 400 | `unknown_model` | not a servable catalog label |
| 400 | `model_capability` | the model can't take this turn (vision/pdf) |
| 402 | `insufficient_credits` | wallet empty (cloud mode) |
| 413 | `context_overflow` | prompt exceeds the model's window |
| 422 | `byok_key_failed` | the user's own provider key was rejected |
| 502 | — | upstream failure (client fallback-chain territory) |

### The control plane

`GET /v1/models` returns the standard list where each entry's id is a catalog
label, extended with an ignorable `memcode` object of selection facts
(vendor, window, vision, pdf, reasoning, pinnable, byok) plus a list-level
`memcode` object (backend, vendors, role config, credits state). Model facts
originate from `models.json` at the repo root.

## 2. The subprocess protocol: stream-JSON

`memcode --output stream-json` (and the protocol mode the builder and other
drivers use) speaks a line-delimited JSON envelope over stdin/stdout,
version "1" (implementation: `internal/wire/streamjson.go`):

Envelope: `{"v":"1","type":<kind>,"data":{…}}`. Kinds: `initialize` /
`initialized` (handshake; carries pin + capabilities), `user_turn`,
`assistant_delta`, `tool_call`, `tool_result`, `permission_request` /
`permission_response`, `ask_request` / `ask_response`, `cancel`, `result`,
`error`. Additive evolution only within version "1".
