# Context compaction — BUILT 2026-06-10

Status: **SHIPPED (CLI), 2026-06-10.** Auto-compaction + manual `/compact` are
live. memcode used to send the FULL append-only `ChatState.messages` every turn;
now, at a safe turn boundary, when the estimated prompt exceeds the budget
(`MEMCODE_COMPACT_BUDGET`, default ~45K; `off` disables), the older turns are
summarized by Anthropic into a warm block and only the last ~8 turns stay raw.

## Where it lives (built)

- `internal/agent/compaction/` — PURE core: `EstimateTokens`, `Plan`
  (safe-boundary split — never divides a tool_use/tool_result pair), `Render`
  (transcript with aggressive tool-output clipping), `CountTurns`. Fully tested
  (`compaction_test.go`): facts-survive, adjacency-never-broken, boundary-only.
- `internal/agent/runtime/compact.go` — orchestration: `compactBudget`,
  `compactIfNeeded` (auto, fired from `Submit` before the turn is assembled),
  `Compact` (manual /compact), the Anthropic-forced summarizer call, the
  synthetic summary turn, telemetry + episodic-log write.
- `compact` mode + `compactDoctrine` in `internal/doctrine/prompts.go` (the
  compactor prompt is composed client-side by the doctrine composer, like every
  other mode).
- TUI: `/compact` slash command (`internal/vxui`).
- `events.KindContextCompacted`, `sessionlog.KindCompaction`.

## Cold-layer recall (the post-compaction "where did that go?" question)

Compaction NEVER touches the cold layer: `.memcode/sessions/<id>/events.jsonl`
keeps growing append-only, so the full history survives on disk. The synthetic
summary turn signposts this — it tells the agent that raw older turns are
retrievable via `memcode{command:"session", target:"search", query:"…"}` (the
same shape as reading a code file it doesn't have in context). **Known gap
(deferred 2026-06-10):** raw tool OUTPUT is not in the canonical log (the "not a
stdout landfill" doctrine), so exact older tool output isn't recoverable beyond
what the summary captured.

### Forward path — selective EVIDENCE capture (backlog, NOT full stdout)

The decision (Tim, 2026-06-10): do **not** persist raw tool output — huge noisy
logs, secret-leak surface, junk search hits, and it would make compaction less
valuable (a stdout landfill). Instead, when dogfooding proves a real recall miss,
add a fourth, curated layer:

```
hot      = recent raw turns
warm     = compacted session summary
cold     = searchable durable memory (asks / answers / decisions / commits / $-output)
evidence = optional CURATED snippets (later)
```

`evidence` = compact records written ONLY when the agent used an output to make a
decision — decision-relevant snippets, capped + redacted + searchable, kept
SEPARATE from the canonical asks/answers/decisions log. Shape:

```json
{ "kind": "evidence", "source": "test", "command": "go test ./...",
  "summary": "provider tests failed due to undefined LaneRole",
  "snippets": ["undefined: LaneRole", "internal/provider/lane.go:42"],
  "paths": ["internal/provider/lane.go"] }
```

Gate: build this only if dogfooding shows the warm summary + cold search actually
fail to recall something load-bearing. Until then, ship as-is.

## Original design notes (for reference)

memcode sends the FULL append-only `ChatState.messages` every turn — no
compaction. Prompt caching makes resends cheap but the window still fills, so long
sessions drift off the cheap lane to Anthropic. It pays off on any backend (a long
Anthropic-only session also dies at its window limit without it).

## Three layers

```
hot    recent raw turns + current task + active file/tool context  (kept verbatim)
warm   structured session summary (compacted older turns)          (BUILD THIS)
cold   episodic session log (.memcode/sessions) — durable recall    (exists)
```

The model sees: doctrine + current objective + warm summary + retrieved memories
+ recent raw turns + current context pack + user request. NOT the whole transcript.

## Trigger (start simple)

Compact at a safe boundary (before accepting a new user turn) when the estimated
prompt approaches the **active model's window** (leave output/tool headroom). The
cheap lane (glm-5p1) is 202K and Anthropic is 1M, so spilling is rare — but a long
session still compacts to stay cheap and fast. Coarse is fine.

## What gets compacted

- Keep the **last ~6–10 exchanges raw**.
- Summarize older turns into a structured block: objective · current plan · files
  inspected · files modified · key decisions · rejected approaches · constraints ·
  failing tests/errors · open questions · session user-preferences.
- **Tool outputs compact most aggressively** — raw grep/test/build output bloats fast.

## Hard rules

1. **Compactor model = Anthropic** (v1). A bad summary becomes the session's truth.
   (Later: the cheap lane may summarize low-risk tool output.)
2. **Tool-use adjacency is sacred.** Never split an assistant tool_use from its
   tool_result. Only compact at a completed boundary (no pending tool call).
3. Store the warm summary in session state AND the episodic log (so /recap & recall
   see it, and a later turn can rebuild context).
4. Compaction runs BEFORE routing — a smaller prompt keeps the turn within the
   resolved model's window instead of being absorbed to Anthropic on `context_overflow`.

## Surface + telemetry

- Manual `/compact` command.
- Telemetry: `raw_history_tokens`, `summary_tokens`, `compacted_turns`,
  `context_after_compaction`.

## Tests (the point of doing it carefully)

- Old turns are removed from the assembled prompt, but their FACTS survive in the
  summary.
- No tool_use/tool_result adjacency is ever broken by compaction.
- Compaction only fires at safe boundaries.

## Sequence

Compaction is **substrate-independent** — it shrinks the prompt regardless of which backend
serves the turn (Fireworks or Anthropic). It is a permanent part of the runtime, not gated on
any inference rollout.
