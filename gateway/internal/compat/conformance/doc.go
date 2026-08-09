// Package conformance is the Phase A0 compat-subset contract, executable: a
// test suite that exercises ANY OpenAI-compatible base URL and reports where it
// sits against the two-tier contract (plans/flickering-soaring-falcon).
//
// Run it against a real endpoint via env:
//
//	CONFORMANCE_BASE_URL=https://api.openai.com/v1 \
//	CONFORMANCE_API_KEY=sk-… \
//	CONFORMANCE_MODEL=gpt-… \
//	go test ./internal/compat/conformance -run Conformance -v
//
// With CONFORMANCE_BASE_URL unset the suite skips cleanly (normal CI). With
// CONFORMANCE_MODEL unset it autodetects a chat model from GET {base}/models.
//
// The tiers:
//
//   - HARD REQUIRED (must-pass — an endpoint missing any of these is
//     unsupported for coding-agent sessions): model in body; multiple system
//     messages honored; streaming SSE chunks + [DONE]; tool definitions;
//     streamed tool-call deltas; forced tool_choice exactly as the classifiers
//     use it.
//
//   - OPTIONAL / PREFERRED (probed, reported, never failed): usage/token
//     reporting (the CLI estimates locally when absent); GET /models.
//
//   - OPTIONAL CAPABILITIES (probed, reported, never failed, never
//     vendor-special-cased): reasoning_effort; vision/image parts; file/PDF
//     parts; cached-token reporting.
//
// The suite prints a capability matrix at the end of the run. Acceptance
// targets: the memcode gateway ({host}/v1), OpenAI, Groq or Fireworks,
// Ollama or vLLM — same suite, same requests, all direct.
package conformance
