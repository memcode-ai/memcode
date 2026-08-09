# memcode — Routing (the CLI is the agent; every backend is dumb serving)

> **North star (2026-08-08, the all-policy-client-side migration):** the **CLI owns model
> policy** — the semantic ladder, physical model selection, BYOK steering, escalation,
> fallback, and mid-call recovery all run client-side (`cli/internal/llm`). Every backend —
> the memcode gateway, Ollama, LM Studio, any OpenAI-compatible endpoint — is a **dumb
> serving surface**: it serves exactly the concrete model the agent asked for, or returns a
> **typed error**. This is the standard client-owns-policy shape (client-side model config + fallback
> chains), and it is what makes memcode behave **identically** across hosted, BYOK, local,
> and arbitrary-endpoint setups: one agent, one wire, one policy.
>
> The doctrine itself is unchanged: *spend intelligence only where uncertainty or risk
> demands it, and default toward CAPABLE when unsure — misrouting DOWN is the expensive
> failure.* What changed is WHERE it runs.
>
> Design/decision record; the **code is the source of truth**
> (`cli/internal/llm/{lane.go,resolve.go,recover.go}` + the shared catalog
> `models.json` (repo root, synced) + the serving gateway `api/internal/provider/`).
>
> Architecture → [ARCH.md](./ARCH.md) · Ops → [RUNBOOK.md](./RUNBOOK.md)

> **History:** the ladder lived server-side (`api/internal/provider/resolve.go` +
> `steer.go`) from 2026-06 to 2026-08-08 — the port was proven against parity goldens
> generated from that code before its deletion (`cli/internal/llm/testdata/*.json`, 6,608
> rows reproduced exactly). Before that, an even earlier era self-hosted vLLM on GPUs;
> deleted at the 2026-06-12 Fireworks cutover. If you find "lanes", "decideLane",
> "ResolveModel", "SteerResolvedModel", or "MEMCODE_WIRE" anywhere, it is stale — `git log`
> has the old designs.

## The division of responsibility

**CLI (`cli/internal/llm` — the agent, the single routing authority):**

- **Semantic ladder** (`lane.go`): intent → lane. Inputs are all CLI-produced — purpose,
  mode, the turn_intent judge's difficulty verdict, the room/risk signals, thinking effort.
- **Physical resolution** (`resolve.go`): lane → concrete catalog **label**, decided over
  the hosted **routing control plane** (`GET /v1/models`: role config, per-model vendor,
  capabilities, byok coverage, credits state) — or, in endpoint mode, the endpoint's
  session model.
- **Steering** — BYOK-first: Automatic prefers vendors the user brought keys for; at $0 an
  Automatic turn never targets an unfunded lane. Selection policy, not a server overlay.
- **Capability absorbs** — an image on a no-vision lane, a PDF on a lane without document
  input, a prompt past the window: remapped client-side BEFORE the call, visibly
  (`FallbackReason` feeds the ⇄ line). The gateway's typed errors are the backstop.
- **Recovery** (`recover.go`): on a model-class failure (after the transport's own
  transient retries), walk the catalog **fallback chain** — current call only, next turn
  re-selects the primary — and only while **nothing was emitted** to the user.
  Billing/entitlement/key/auth/overflow errors are terminal for the chain: they carry
  their own policy (compaction, the billing dialog, /apikeys, /login).
- **delegateDoctrine** — the cheap-coding-lane "hand non-code work to a strong agent"
  fragment is appended post-selection (routing-owned prose lives with the routing
  decision).

**Gateway (`api/` — a metered serving endpoint):**

- Auth door, entitlement enforcement (subscription/lock/credits as **typed 402s** that
  decline, never redirect), BYOK vault + key injection, metering/debit, rate limits.
- **Strict label gate**: `model` must be a servable catalog label — `auto`, vendor ids,
  and typos are `400 unknown_model`. There is no server-side Automatic.
- **Typed errors, no absorbs**: `413 context_overflow`, `400 model_capability`,
  `422 byok_key_failed`, `402 insufficient_credits` / `subscription_required` /
  `account_locked`, `502` for upstream failures. The gateway never reroutes a request to a
  different model — in either direction.
- **The control plane**: `GET /v1/models` serves every fact selection reads. Anything new
  the policy needs gets added there explicitly — never smuggled back into gateway routing.

## The semantic ladder (`lane.go`)

A lane is a chain of deployment **roles** with a vendor-**tier** fallback. Roles come from
the gateway's `config.json` via the control plane; tier triples are catalog data
(`models.json` `tiers`: frontier/balanced/cheap per vendor).

| Turn | Lane |
|---|---|
| `review` (plan critic) | role `reviewer`, else frontier |
| plan mode: classify / scouts | roles `classify`→`standard` / `standard`, else balanced |
| plan mode: executive draft | role `planner`, else frontier; **frontier directly** on `plan_review_escalate`, `plan_synth_incomplete`, `self_heal`, or a `high_risk_surface` plan (the plan is the binding contract) |
| `classify` (judges) | roles `classify`→`standard`, else cheap |
| `explore` / `route` | role `standard`, else cheap |
| `reflect` / `synth` | role `planner`, else frontier |
| `predict` / `learn` / `compact` / `shrinkwrap` / `overview` | role `standard`, else balanced |
| main loop: `self_heal` / `agent_frontier` | **frontier tier** (the error valve / background agents) |
| main loop: `agent_strong` | balanced tier (a dispatched strong agent) |
| main loop: judged `deep`, or effort-high unjudged | role `planner`, else frontier |
| main loop: judged `lookup`, no risk | role `standard`, else cheap |
| main loop: everything else | role `standard`, else balanced |

Roles today: `planner`/`standard` = glm-5p2 (Fireworks, 1M), `reviewer` = luna (OpenAI),
`classify` = gpt-oss-120b (Fireworks). The swap knob is the gateway's `config.json` +
redeploy — the CLI reads the roles off `/v1/models`, so a role swap needs no CLI release.

## Steering + the $0 invariant (`resolve.go`)

BYOK-first, decided at selection time: an Automatic turn that resolved to a strong vendor
the user has **no key** for remaps to the **keyed preference** (deployment default vendor
first, then catalog order) at the **same tier altitude**. Explicit choices are never
overridden: a pin, or a non-default `/model <vendor>` flavor, rides through untouched.
With no BYOK keys the whole pass is a byte-identical no-op.

At **$0 credits** with keys present, an Automatic selection must land on a keyed (fundable)
lane — the old `credits_byok` absorb, now decided up-front and visibly. Pins are exempt: an
unkeyed pin at $0 gets the gateway's clean 402 naming the vendor, never a coercion. The
gateway still **enforces** all money invariants server-side; client selection just stops
doomed requests before they burn a round trip.

**The billing lane is explicit on the wire** (`memcode_billing`: `byok_preferred` default |
`byok_only` | `credits`): the gateway enforces the requested lane and never chooses one —
silent rerouting between the user's keys and credits is impossible in either direction. A
`byok_key_failed` turn is NOT hard-terminal anymore: the CLI's default policy surfaces the
■ notice and never silently retries on credits, but an **explicit, consented** "retry this
turn on credits" is legitimate client policy (consent is not silence).

## Fallback + recovery (`recover.go`, catalog `fallback` chains)

Each catalog model declares its mid-turn failure chain (labels), e.g. `glm-5p2 →
[kimi-k2p7-code, terra]`, `sol → [terra]`, `gemini-pro → [gemini-flash, terra]`. On a
model-class error the Runner walks the chain — filtered by availability on this backend,
capability fit for this payload, and fundability under the org's credit state — and
retries the same call (≤2 hops). Standard client-fallback semantics: **current call only** (the
next turn retries the primary), **never after output was emitted**, and terminal classes
(402s, key/auth failures, overflow, stream cuts) never enter the chain. The ⇄ line shows
`model_error: …` so rescue is visible, never silent.

## Pinned models — `/model`

Unchanged in spirit: the picker pins one catalog label for every real request; invisible
plumbing (`classify`/`compact`/`shrinkwrap`) stays on the utility lanes; a stale/unknown
pin falls through to Automatic. The pin is now simply the selection policy's first branch
(`resolve.go`), and "serve exactly what was asked" is the entire gateway contract rather
than a special `servePinned` path.

## Endpoint mode (Ollama, LM Studio, vLLM, provider clouds — no gateway)

The same agent, no gateway: the endpoint's session model (picked via `/model`,
remembered per-endpoint) serves **every** lane — there are no roles or tiers to resolve,
which IS the uniformity story: the ladder degrades to the available model set. Fallback
chains apply only to cataloged labels, so an uncataloged local model fails honestly
instead of being silently swapped. No memcode extensions leave the machine.

**Wire selection** (ONE implementation per provider protocol, shared by gateway and
direct mode — `packages/sdk/go/providers/{provcore,openai,anthropic,gemini,compat,
memcode}`): a provider's OWN API gets its full-fidelity native dialect —
`api.openai.com`/`api.x.ai` speak the **Responses API**, `api.anthropic.com` the
**Messages API**, `generativelanguage.googleapis.com` the **Gemini API** — via the exact
adapters the hosted gateway runs. Everything else — local runtimes, compat clouds —
speaks the generic chat/completions engine (`providers/compat`, which is ALSO the
gateway's Fireworks lane client: salvage net + lane error contract as configuration),
with a probe-and-degrade retry (`reasoning_effort:"none"`) for compat endpoints that
refuse tools while reasoning is active. The memcode dialect itself is a provider
(`providers/memcode`): the compat engine + the memcode extensions + the /v1/models
control-plane client. The gateway keeps NO wire code: `api/internal/provider` is
construction + serving policy (key injection, capability gates, money) only. **Keys**: explicit `MEMCODE_ENDPOINT_KEY` > config `key_env` > the
ecosystem-standard env var for well-known hosts (`OPENAI_API_KEY`, `XAI_API_KEY`,
`GROQ_API_KEY`, …); local hosts stay keyless.

## Verification

- `cli/internal/llm/parity_test.go` — 5,992 ladder rows + 616 steering rows reproduced
  from the pre-deletion gateway goldens.
- `cli/internal/llm/policy_test.go` — capability absorbs, $0 rule, chain walk, emitted
  guard, delegate append, endpoint bypass, control-plane outage degradation.
- `cli/internal/provider/uniformity_test.go` — one scripted session against a
  gateway-shaped server AND a bare endpoint: identical wire shape (zero memcode headers),
  identical policy, different model sets only.
- `api/internal/server/compat_test.go` + `internal/compat/conformance` — the serving
  contract: strict label gate, typed errors, money canary, self-conformance.
