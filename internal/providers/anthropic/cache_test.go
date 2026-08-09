package anthropic

import (
	"encoding/json"
	"testing"

	"github.com/memcode-ai/memcode/internal/wire"
)

func TestBuildWireCacheBreakpoints(t *testing.T) {
	r := wire.Request{
		System: "you are memcode",
		Tools: []wire.ToolDef{
			{Name: "read_file", Description: "read"},
			{Name: "bash", Description: "run"},
		},
		Messages: []wire.Message{
			{Role: "user", Blocks: []wire.Block{{Type: "text", Text: "hi"}}},
			{Role: "assistant", Blocks: []wire.Block{
				{Type: "text", Text: "thinking"},
				{Type: "tool_use", ID: "t1", Name: "bash"},
			}},
		},
	}
	w := buildWire(r, 4096, false)

	// System becomes a cache-marked text block array.
	sys, ok := w.System.([]sysBlock)
	if !ok || len(sys) != 1 || sys[0].CacheControl == nil || sys[0].Text != "you are memcode" {
		t.Fatalf("system not a cached block: %#v", w.System)
	}
	// Only the LAST tool carries cache_control.
	if w.Tools[0].CacheControl != nil || w.Tools[len(w.Tools)-1].CacheControl == nil {
		t.Fatalf("cache_control must be on last tool only: %#v", w.Tools)
	}
	// Only the last block of the last message carries cache_control.
	lastMsg := w.Messages[len(w.Messages)-1]
	if lastMsg.Blocks[0].CacheControl != nil || lastMsg.Blocks[len(lastMsg.Blocks)-1].CacheControl == nil {
		t.Fatalf("cache_control must be on the last block of the last message only: %#v", lastMsg.Blocks)
	}

	// CRITICAL: the caller's structs must be untouched (no in-place mutation).
	if r.Tools[len(r.Tools)-1].CacheControl != nil {
		t.Fatal("buildWire mutated the caller's tools slice")
	}
	if r.Messages[len(r.Messages)-1].Blocks[1].CacheControl != nil {
		t.Fatal("buildWire mutated the caller's message blocks")
	}

	// And it must serialize to the documented JSON shape — the stable doctrine block
	// carries the 1h breakpoint (it's byte-identical across turns; see the volatile-split test).
	b, _ := json.Marshal(w.System)
	if got := string(b); got != `[{"type":"text","text":"you are memcode","cache_control":{"type":"ephemeral","ttl":"1h"}}]` {
		t.Fatalf("system JSON = %s", got)
	}
}

// The volatile facts ride as a SECOND, uncached system block AFTER the stable one, so a
// per-turn fact change never busts the cached doctrine prefix.
func TestBuildWireSplitsVolatileSystem(t *testing.T) {
	r := wire.Request{System: "DOCTRINE", SystemVolatile: "[voice — tone only] be playful"}
	w := buildWire(r, 4096, false)
	sys, ok := w.System.([]sysBlock)
	if !ok || len(sys) != 2 {
		t.Fatalf("system should split into stable + volatile blocks, got %#v", w.System)
	}
	if sys[0].Text != "DOCTRINE" || sys[0].CacheControl == nil || sys[0].CacheControl.TTL != "1h" {
		t.Fatalf("stable block must be the doctrine with a 1h breakpoint: %#v", sys[0])
	}
	if sys[1].Text != "[voice — tone only] be playful" || sys[1].CacheControl != nil {
		t.Fatalf("volatile block must follow uncached: %#v", sys[1])
	}
	// No volatile → a single cached block (unchanged behaviour).
	if w := buildWire(wire.Request{System: "DOCTRINE"}, 4096, false); len(w.System.([]sysBlock)) != 1 {
		t.Fatalf("no volatile → single system block, got %#v", w.System)
	}
}

func TestBuildWireEmpty(t *testing.T) {
	w := buildWire(wire.Request{}, 4096, false)
	if w.System != nil || len(w.Tools) != 0 || len(w.Messages) != 0 {
		t.Fatalf("empty request should stay empty: %#v", w)
	}
}
