package anthropic

import (
	"encoding/json"
	"testing"
)

// The SDK migration dumped memcode's FULL tool schema into ToolInputSchemaParam.Properties,
// producing a double-nested, draft-2020-12-invalid input_schema that Anthropic 400s on
// (`tools.0.custom.input_schema: JSON schema is invalid`). Lock the correct split.
func TestToolInputSchemaSplitsFullSchema(t *testing.T) {
	full := map[string]any{
		"type":                 "object",
		"properties":           map[string]any{"city": map[string]any{"type": "string"}},
		"required":             []any{"city"},
		"additionalProperties": false,
	}
	s := toolInputSchema(full)

	props, ok := s.Properties.(map[string]any)
	if !ok || props["city"] == nil {
		t.Fatalf("Properties must be the inner properties map, got %#v", s.Properties)
	}
	if _, leaked := props["type"]; leaked {
		t.Fatal("the full schema leaked into Properties (the double-nesting bug)")
	}
	if len(s.Required) != 1 || s.Required[0] != "city" {
		t.Fatalf("Required must be extracted from the schema, got %v", s.Required)
	}
	if s.ExtraFields["additionalProperties"] != false {
		t.Fatalf("other schema keywords must ride ExtraFields, got %v", s.ExtraFields)
	}

	// The marshaled wire must be a valid object schema: type=object, properties holding
	// `city` directly (NOT a nested {type,properties,required}).
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if err := json.Unmarshal(b, &wire); err != nil {
		t.Fatal(err)
	}
	if wire["type"] != "object" {
		t.Fatalf("wire input_schema must be type=object: %s", b)
	}
	wp, _ := wire["properties"].(map[string]any)
	if _, doubled := wp["type"]; doubled {
		t.Fatalf("double-nested properties on the wire (the bug): %s", b)
	}
	if wp["city"] == nil {
		t.Fatalf("properties must hold the real fields: %s", b)
	}
}
