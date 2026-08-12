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

`memcode agent --protocol stream-json` speaks a line-delimited JSON envelope
over stdin/stdout — one envelope per line — that an SDK wrapper or desktop
client drives the agent with (implementation: `internal/wire/streamjson.go`;
the builder job and Memcode Desktop are both clients). Diagnostics go to stderr.

Envelope:

```json
{"version":"1","type":<kind>,"id":"<corr>","turn_id":"<turn>","data":{…}}
```

`version` is always `"1"`. `id` correlates a request with its response
(permission/ask). `turn_id` tags every event to the `user_turn` that produced
it. Evolution within version "1" is **additive only** — new kinds and new
optional fields; a client ignores kinds and fields it doesn't recognize.

### Client → CLI

| kind | data | notes |
|---|---|---|
| `initialize` | `{cwd, mode, pin?, resume?}` | first message. `mode` = `ask` \| `auto` \| `allow-all`. `resume` = a prior session id/ref to continue. |
| `user_turn` | `{text, attachments?}` | `attachments` = `[{path, name?, mime?}]` — first-class local files (drag-and-drop), read by the runtime; not `@path` text. |
| `permission_response` | `{allow, command?, reason?, interrupt?, remember?, remember_scope?}` | answers a `permission_request` (match `id`). `remember`/`remember_scope` persist a "don't ask again" rule. |
| `ask_response` | `{answer}` | answers an `ask_request` (match `id`). |
| `cancel` | `{}` | interrupt the in-flight turn. |

### CLI → Client

| kind | data | notes |
|---|---|---|
| `initialized` | `{session_id, protocol}` | session ready; emitted after `initialize` (and after resume resolves). |
| `assistant_delta` | `{text}` | streamed assistant text; concatenate within a `turn_id`. |
| `tool_call` | `{name, target?, detail?}` | a tool started. |
| `tool_result` | `{name, status?}` | that tool finished (`status` = `ok` \| `failed`). |
| `diff` | `{path, language?, unified?, added, removed, new_file?}` | a structured file change; the client renders its own highlighted diff. |
| `todos` | `{items:[{text, status}]}` | the plan/checklist (`status` = `pending` \| `in_progress` \| `done` \| `blocked` \| `skipped`). |
| `permission_request` | `{title, label?, detail?, command?, cwd?, risk?, editable?}` | approval gate; reply with `permission_response` (match `id`). |
| `ask_request` | `{question, options?}` | a question; reply with `ask_response` (match `id`). |
| `session_state` | `{busy, mode?}` | busy/idle + room/mode telemetry. |
| `usage` | `{output_tokens}` | running token count. |
| `result` | `{text?, completed}` | the turn ended. |
| `error` | `{message}` | a turn-level error. |
