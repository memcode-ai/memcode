package compat

// The wire TYPES are shared now (providers/compat): one declaration serves
// the CLI transport, this inbound surface, the lane client, and the
// conformance suite. This package keeps the gateway's inbound/outbound
// TRANSLATION (translate.go) — the serving side of the same shared types.

import "github.com/memcode-ai/memcode/internal/providers/compat"

type (
	ChatRequest         = compat.ChatRequest
	StreamOptions       = compat.StreamOptions
	ChatMessage         = compat.ChatMessage
	MessageContent      = compat.MessageContent
	ContentPart         = compat.ContentPart
	ImageURLPart        = compat.ImageURLPart
	FilePart            = compat.FilePart
	Tool                = compat.Tool
	FunctionDef         = compat.FunctionDef
	ToolCall            = compat.ToolCall
	FunctionCall        = compat.FunctionCall
	ChatResponse        = compat.ChatResponse
	Choice              = compat.Choice
	ResponseMessage     = compat.ResponseMessage
	Usage               = compat.Usage
	ChatChunk           = compat.ChatChunk
	ChunkChoice         = compat.ChunkChoice
	Delta               = compat.Delta
	ToolCallDelta       = compat.ToolCallDelta
	MemcodeExt          = compat.MemcodeExt
	PromptTokensDetails = compat.PromptTokensDetails
	ErrorResponse       = compat.ErrorResponse
	ErrorBody           = compat.ErrorBody
	ModelList           = compat.ModelList
	ModelEntry          = compat.ModelEntry
	ModelMeta           = compat.ModelMeta
	ModelsExt           = compat.ModelsExt
	RoleEntry           = compat.RoleEntry
)

// Constructors re-exported for the translation layer + tests.
var (
	StringContent = compat.StringContent
	PartsContent  = compat.PartsContent
	TextPart      = compat.TextPart
)
