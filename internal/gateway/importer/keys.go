package importer

// providerKeyNames are the provider API-key environment variables memcode
// recognizes (the same set documented at /docs/cli/environment-variables). A
// migration carries these over verbatim: OpenClaw and Hermes store them under
// the identical names, so a key already in the source's .env drops straight into
// memcode's global .env with no remapping.
var providerKeyNames = []string{
	"ANTHROPIC_API_KEY",
	"OPENAI_API_KEY",
	"GEMINI_API_KEY",
	"XAI_API_KEY",
	"GROQ_API_KEY",
	"MISTRAL_API_KEY",
	"DEEPSEEK_API_KEY",
	"FIREWORKS_API_KEY",
	"TOGETHER_API_KEY",
	"OPENROUTER_API_KEY",
	"CEREBRAS_API_KEY",
}

// ProviderKeys returns the subset of env holding a recognized provider API key
// with a non-empty value — what a migration should copy into memcode's global
// .env so the agent keeps talking to the same models. Channel bot tokens are NOT
// here; those come from the channel importer.
func ProviderKeys(env map[string]string) map[string]string {
	out := map[string]string{}
	for _, name := range providerKeyNames {
		if v := env[name]; v != "" {
			out[name] = v
		}
	}
	return out
}
