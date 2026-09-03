# memcode — Model selection (one pin per session)

> **North star (v0.29.0, the routing removal):** there is **exactly one model per
> session**, and the **user chose it**. Nothing inspects a task's difficulty, risk, or
> cost to pick a model on the user's behalf. The pin is authoritative: what it names is
> what serves, or the turn fails with a reason.
>
> Two mechanisms are allowed to run a call on something other than the pin, and both are
> deliberately narrow: **utility plumbing** (classify, compact, shrinkwrap) rides a
> declared `utility_model`, and **infrastructure failure** may fall back down a declared
> chain for the current call only. Neither may ever be reached from a judgment about the
> work. Anything that influences *which model runs* based on *what the work looks like*
> is routing, and routing is gone.
>
> Design/decision record; the **code is the source of truth**
> (`internal/config/pin.go`, `internal/llm/{resolve.go,recover.go}`, `internal/policy`,
> and the shared catalog `models.json`).

> **History.** A semantic ladder chose a model per turn, first server-side (2026-06 to
> 2026-08-08), then ported into the CLI. v0.29.0 deleted it: lanes, roles, tier triples,
> BYOK steering, the $0 fundability remap, and capability substitution all went at once.
> The removal is recorded in tombstone comments at each site rather than here. If you
> find `decideLane`, `ResolveModel`, `SteerResolvedModel`, `laneFor`, or a *live* claim
> that selection prefers a vendor, it is stale — `git log` has the old designs.

## Where the model comes from

The pin resolver settles this once, at session start (`config.ResolvePin`):

| Source | Persisted? | Notes |
|---|---|---|
| `--model` / the session override | **No** | This invocation's model, not a new preference |
| Workspace (`.memcode` config) | Yes | This repo's answer |
| User store | Yes | Adopted into the workspace on first use here |
| `default_model` (catalog) | **Seeds once**, then persisted | So only one run ever consults it |

Each step is consulted only when the one above it is empty, so a session that has a pin
never re-derives one. If the seed cannot be recorded to either store, the run still works
and says so: the next run would otherwise seed again, and `default_model` legitimately
changes as models are added and retired.

The pin is a catalog **label** (`sonnet`, `glm-5p2`), never a vendor id or an alias
resolved at call time.

## What the pin governs

Everything the user's work runs on: the main loop, plan drafting, delegated workers, and
scouts. `/model` changes it mid-session.

**Delegated work** may run on a *different* pin, chosen by the user, through the policy
layer (`internal/policy`, target `agent.delegated`, which sub-agent and scout targets
inherit from). Unset means inherit the primary. This is the user spending the expensive
model where they decided it belongs, not the system deciding for them.

## The two exceptions

**1. Utility plumbing.** `classify`, `compact`, and `shrinkwrap` ride the catalog's
`utility_model`. These summarize and route internal state; none of them produces user
work, and none may select, substitute, escalate, downgrade, or steer the pinned model. If
no utility model is declared, the work falls through to the pin rather than inventing a
model: losing a classifier is better than a silent pick.

**2. Infrastructure failure.** See below.

Nothing else. A capability gap is *not* an exception.

## Capability gaps refuse, they do not substitute

Pasting a screenshot at a model without vision, a PDF at a model without document input,
or a prompt past the window is a **visible refusal that names the fix**
(`capabilityCheck`). This used to substitute a capable model silently, which moved the
turn onto a model the user never chose and never saw. That was routing wearing a
different hat.

## Fallback is infrastructure resilience only

Each catalog model declares a `fallback` chain. On a provider or transport failure the
Runner walks it and retries the same call, naming the substitute on the ⇄ line so a
rescue is never silent.

The rules that keep this from becoming routing again:

- **Current call only.** The next turn resolves the primary pin as normal.
- **Never after output was emitted.**
- **Never from a judgment.** No code path leads here from "the result looked weak" or
  "this task looks hard". Only from an error.
- **The first hop always changes vendor.** `gemini-flash` falling back to `gemini-pro` is
  not a fallback: same adapter, same API, same failure. Enforced by
  `TestFallbackFirstHopLeavesTheVendor`.

### Which errors walk the chain

The distinction is whether a *different model* could plausibly succeed:

| Class | Behavior | Why |
|---|---|---|
| 400 malformed, 401/403 auth, 404 unknown model | **Terminal** | The request is what is wrong; every model rejects it identically |
| 408 timeout, 429 rate limit | Walk | Timing, not shape |
| 5xx, transport failures, stream cuts | Walk | Infrastructure |
| Context overflow | Terminal for the chain | Compaction handles it, then retries |
| Billing, entitlement, BYOK key, sign-out | Terminal for the chain | Each carries its own policy: the billing dialog, `/apikeys`, `/login` |

Getting this wrong is expensive in a way that hides: a single malformed request walked
across a chain becomes one bug reported as many failures, and a terminal failure treated
as retryable becomes a run that never ends.

## The gateway serves; it does not choose

The gateway is a **metered serving endpoint**, deployed separately from this repo.

- **Strict label gate.** `model` must be a servable catalog label. Vendor ids, aliases,
  `auto`, and typos are `400 unknown_model`.
- **Typed errors, no absorbs.** `413 context_overflow`, `400 model_capability`,
  `422 byok_key_failed`, `402 insufficient_credits` / `subscription_required` /
  `account_locked`, `400 upstream_request_invalid` for a provider 4xx we caused, `502`
  for genuine upstream trouble. It never reroutes a request to a different model in
  either direction.
- **Enforcement stays server-side.** Auth, entitlement, BYOK key injection, metering,
  rate limits. The client stops doomed requests early as a courtesy; the gateway is what
  actually holds the line.
- **The control plane.** `GET /v1/models` serves every fact selection reads: labels,
  vendor identity, capabilities, windows, BYOK coverage, credits state. Anything new the
  client needs gets added there explicitly, never smuggled back into gateway routing.

## Endpoint and local backends

With `MEMCODE_ENDPOINT_URL` (or Ollama, LM Studio, any OpenAI-compatible server), the
endpoint's configured model is the session model and the pin chain is not consulted. The
same one-model-per-session rule holds; it is simply the endpoint that names it.

## The rule, restated

> The pinned model is authoritative. Utility plumbing and infrastructure failure are the
> only paths to another model, and neither may be reached from a judgment about the work.

If a change would let memcode run a turn on a model the user did not choose, because of
something about the *task*, it is reintroducing routing.
