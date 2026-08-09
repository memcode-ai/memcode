package wire

import "errors"

// ErrContextOverflow is returned when a turn's prompt exceeds the served context
// window on EVERY available backend (the gateway already absorbs vLLM overflow up
// to Anthropic; this fires only when even Anthropic's window is exceeded). The CLI
// runtime watches for it with errors.Is and compacts-then-retries the turn rather
// than surfacing a raw error — the reactive end of the routing ladder. On the wire
// it is signalled by HTTP 413 with code "context_overflow" and by an SSE error event
// carrying the same code.
var ErrContextOverflow = errors.New("context window overflow")

// ErrStreamIncomplete is returned when an SSE stream ends WITHOUT a terminal response
// event (a mid-stream read error, or the connection closing early — e.g. a Cloud Run
// request-timeout cutting off a long model call). It is a transient TRANSPORT failure,
// not a model/logic error: the failed call appended nothing, so the runtime can retry
// it from the same history. Watched with errors.Is and retried (bounded) rather than
// dying mid-turn with work half-done.
var ErrStreamIncomplete = errors.New("gateway stream ended without a response")

// ErrInsufficientCredit is returned when the org's prepaid credit balance is
// exhausted — the gateway returns 402 with code "insufficient_credits". The CLI
// surfaces a friendly "add credits" message and stops the turn (no retry: buying
// credits is a user action, not a transient failure).
var ErrInsufficientCredit = errors.New("credits exhausted — add credits at memcode.ai/account/billing")

// ErrSubscriptionRequired is returned when the org has no active subscription —
// the gateway returns 402 with code "subscription_required". Subscription is
// mandatory for every LLM call, BYOK included (2026-07-26). Never retried:
// choosing a plan is a user action.
var ErrSubscriptionRequired = errors.New("subscription required — choose a plan at memcode.ai/account/billing")

// ErrAccountLocked is returned when the org's balance went negative from a
// post-hoc debit overrun — the gateway returns 402 with code "account_locked".
// Everything is refused, BYOK included, until credits are added. Never retried.
var ErrAccountLocked = errors.New("account locked — your balance is negative; add credits at memcode.ai/account/billing")

// ErrByokKeyFailed is returned when the turn died on the USER's own provider
// key (fail-the-turn doctrine: a failing BYOK key is never absorbed onto
// memcode's keys) — the gateway returns 422 with code "byok_key_failed".
// Never retried: the fix is /apikeys, not another attempt.
var ErrByokKeyFailed = errors.New("your API key was rejected — fix or remove it with /apikeys")

// ErrUnauthorized is returned when the gateway rejects the bearer token (HTTP
// 401): expired, revoked, or the retired legacy static token. It means the
// SESSION is signed out, not that the turn transiently failed — hosts watch
// with errors.Is, flip to their signed-out state, and prompt for /login
// instead of surfacing a raw HTTP error. Never retried.
var ErrUnauthorized = errors.New("not signed in — your session expired or the token was revoked; run /login to reconnect")

// CodeContextOverflow is the gateway's machine-readable error code for a context-window
// overflow — carried on the HTTP 413 body and the SSE error event, mapped to
// ErrContextOverflow by the client.
const CodeContextOverflow = "context_overflow"

// CodeInsufficientCredit is the gateway's machine-readable error code for an
// exhausted credit wallet — carried on the HTTP 402 body and the SSE error event,
// mapped to ErrInsufficientCredit by the client.
const CodeInsufficientCredit = "insufficient_credits"

// CodeSubscriptionRequired is the gateway's machine-readable error code for an
// org with no active subscription (HTTP 402), mapped to ErrSubscriptionRequired.
const CodeSubscriptionRequired = "subscription_required"

// CodeAccountLocked is the gateway's machine-readable error code for an org
// locked by a negative balance (HTTP 402), mapped to ErrAccountLocked.
const CodeAccountLocked = "account_locked"

// CodeByokKeyFailed is the gateway's machine-readable error code for a turn
// killed by the user's own provider key (HTTP 422 body and the SSE error
// event), mapped to ErrByokKeyFailed.
const CodeByokKeyFailed = "byok_key_failed"
