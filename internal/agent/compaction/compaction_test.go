package compaction

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/memcode-ai/memcode/internal/wire"
)

// helpers to build the message shapes the runtime produces.
func userText(s string) wire.Message {
	return wire.Message{Role: "user", Blocks: []wire.Block{{Type: "text", Text: s}}}
}
func asstText(s string) wire.Message {
	return wire.Message{Role: "assistant", Blocks: []wire.Block{{Type: "text", Text: s}}}
}
func asstToolUse(id, name, input string) wire.Message {
	return wire.Message{Role: "assistant", Blocks: []wire.Block{{Type: "tool_use", ID: id, Name: name, Input: []byte(input)}}}
}
func toolResult(id, content string) wire.Message {
	return wire.Message{Role: "user", Blocks: []wire.Block{{Type: "tool_result", ToolUseID: id, Content: content}}}
}

// a realistic transcript: N turns, some with a tool round-trip.
func session(n int) []wire.Message {
	var ms []wire.Message
	for i := 0; i < n; i++ {
		ms = append(ms, userText("ask "+string(rune('A'+i))))
		// every other turn does a tool round-trip before answering
		if i%2 == 0 {
			ms = append(ms, asstToolUse("t"+string(rune('A'+i)), "read_file", `{"path":"x.go"}`))
			ms = append(ms, toolResult("t"+string(rune('A'+i)), "file contents"))
		}
		ms = append(ms, asstText("answer "+string(rune('A'+i))))
	}
	return ms
}

func TestPlanNotEnoughTurns(t *testing.T) {
	ms := session(3)
	if _, _, ok := Plan(ms, 8); ok {
		t.Fatal("Plan should be a no-op when there are fewer turns than keepRecent")
	}
}

func TestPlanKeepsLastKTurnsRaw(t *testing.T) {
	ms := session(12)
	head, tail, ok := Plan(ms, 4)
	if !ok {
		t.Fatal("Plan should compact a 12-turn session keeping 4")
	}
	if got := CountTurns(tail); got != 4 {
		t.Fatalf("tail should hold exactly the last 4 turns, got %d", got)
	}
	if CountTurns(head)+CountTurns(tail) != CountTurns(ms) {
		t.Fatal("head+tail turns must equal the whole session (no turn lost or duplicated)")
	}
	// tail must BEGIN at a real user turn (a safe boundary), never mid-turn.
	if !isTurnStart(tail[0]) {
		t.Fatalf("tail must start at a user-turn boundary, got role=%q blocks=%d", tail[0].Role, len(tail[0].Blocks))
	}
}

// The load-bearing invariant: a tool_use and its tool_result must never end up on
// opposite sides of the cut.
func TestPlanNeverSplitsToolPair(t *testing.T) {
	for k := 1; k <= 10; k++ {
		ms := session(12)
		head, tail, ok := Plan(ms, k)
		if !ok {
			continue
		}
		assertPairsIntact(t, head, k)
		assertPairsIntact(t, tail, k)
	}
}

// assertPairsIntact checks every tool_use in ms has its matching tool_result in
// the SAME slice (so the cut didn't orphan either side).
func assertPairsIntact(t *testing.T, ms []wire.Message, k int) {
	t.Helper()
	uses := map[string]bool{}
	results := map[string]bool{}
	for _, m := range ms {
		for _, b := range m.Blocks {
			switch b.Type {
			case "tool_use":
				uses[b.ID] = true
			case "tool_result":
				results[b.ToolUseID] = true
			}
		}
	}
	for id := range uses {
		if !results[id] {
			t.Fatalf("keepRecent=%d: tool_use %q has no matching tool_result in its slice — pair was split", k, id)
		}
	}
	for id := range results {
		if !uses[id] {
			t.Fatalf("keepRecent=%d: tool_result %q has no matching tool_use in its slice — pair was split", k, id)
		}
	}
}

// Even when the cut would land right after a tool round-trip, the boundary snaps to
// the next user turn, never between a tool_use and its result.
func TestPlanBoundaryWithToolHeavyTail(t *testing.T) {
	ms := []wire.Message{
		userText("first"),
		asstToolUse("t1", "bash", `{"command":"go test"}`),
		toolResult("t1", "ok"),
		asstText("done first"),
		userText("second"),
		asstToolUse("t2", "bash", `{"command":"go build"}`),
		toolResult("t2", "ok"),
		asstText("done second"),
		userText("third"),
		asstText("done third"),
	}
	head, tail, ok := Plan(ms, 1)
	if !ok {
		t.Fatal("should compact")
	}
	// keepRecent=1 → tail is just the final turn ("third").
	if !isTurnStart(tail[0]) || tail[0].Blocks[0].Text != "third" {
		t.Fatalf("tail should start at the 'third' user turn, got %+v", tail[0])
	}
	assertPairsIntact(t, head, 1)
	assertPairsIntact(t, tail, 1)
}

// Render must drop tool mechanics down to one-liners and clip output, while keeping
// the user/assistant PROSE — that prose is the fact substrate the summary draws on.
func TestRenderKeepsFactsClipsOutput(t *testing.T) {
	huge := strings.Repeat("X", 5000)
	ms := []wire.Message{
		userText("please fix the auth bug in login.go"),
		asstToolUse("t1", "read_file", `{"path":"login.go"}`),
		toolResult("t1", huge),
		asstText("found it: missing nil check at login.go:42"),
	}
	out := Render(ms)
	// facts survive
	for _, want := range []string{"please fix the auth bug in login.go", "login.go:42", "read_file"} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered transcript dropped a load-bearing fact: %q\n%s", want, out)
		}
	}
	// the 5000-char tool dump must be clipped, not pasted whole
	if strings.Contains(out, huge) {
		t.Error("tool output should be clipped in the transcript, not included verbatim")
	}
	if len(out) > 2000 {
		t.Errorf("transcript should be compact after clipping, got %d bytes", len(out))
	}
}

func TestEvictStaleToolResults(t *testing.T) {
	// A turn with several tool round-trips: keep the last 2 tool_results raw, elide older.
	ms := []wire.Message{
		userText("do the task"),
		asstToolUse("t1", "ripgrep", `{"q":"a"}`),
		toolResult("t1", "result ONE (big dump)"),
		asstToolUse("t2", "ripgrep", `{"q":"b"}`),
		toolResult("t2", "result TWO (big dump)"),
		asstToolUse("t3", "read_file", `{"path":"x"}`),
		toolResult("t3", "result THREE"),
		asstToolUse("t4", "read_file", `{"path":"y"}`),
		toolResult("t4", "result FOUR"),
	}
	n := EvictStaleToolResults(ms, 2)
	if n != 2 {
		t.Fatalf("should elide the 2 oldest of 4 tool_results, got %d", n)
	}
	// Adjacency intact: every tool_use still has its tool_result block (content elided or not).
	assertPairsIntact(t, ms, 2)
	// The two oldest are elided; the two newest survive verbatim.
	got := map[string]string{}
	for _, m := range ms {
		for _, b := range m.Blocks {
			if b.Type == "tool_result" {
				got[b.ToolUseID] = b.Content
			}
		}
	}
	if !isOffloaded(got["t1"]) || !isOffloaded(got["t2"]) {
		t.Errorf("t1/t2 should be offloaded to a pointer: %q %q", got["t1"], got["t2"])
	}
	if !strings.Contains(got["t1"], "ripgrep") {
		t.Errorf("the offload pointer should name the source tool so the model can re-fetch: %q", got["t1"])
	}
	if got["t3"] != "result THREE" || got["t4"] != "result FOUR" {
		t.Errorf("t3/t4 should survive verbatim: %q %q", got["t3"], got["t4"])
	}
	// Idempotent: re-running elides nothing new (already-elided aren't re-counted).
	if again := EvictStaleToolResults(ms, 2); again != 0 {
		t.Errorf("second pass should elide nothing, got %d", again)
	}
}

func TestEstimateTokensGrowsWithContent(t *testing.T) {
	small := EstimateTokens(session(2))
	big := EstimateTokens(session(20))
	if big <= small {
		t.Fatalf("estimate should grow with history (small=%d big=%d)", small, big)
	}
}

// tool_result-only messages are NOT turn boundaries (they belong to the assistant's
// turn), so they can't be chosen as a split point.
func TestToolResultIsNotATurnStart(t *testing.T) {
	if isTurnStart(toolResult("t1", "x")) {
		t.Fatal("a tool_result carrier must not count as a user-turn start")
	}
	if !isTurnStart(userText("hi")) {
		t.Fatal("a user text message must count as a turn start")
	}
}

func TestOffloadRefTypedAndLosslessForFiles(t *testing.T) {
	// A file read points back to the file — re-reading restores it (lossless; the file IS the store).
	r := offloadRef("read_file", json.RawMessage(`{"path":"internal/x.go"}`))
	if !isOffloaded(r) || !strings.Contains(r, "internal/x.go") || !strings.Contains(r, "re-read") {
		t.Fatalf("read_file pointer must name the path and say re-read: %q", r)
	}
	// A grep names how to re-fetch (carries the query).
	if r := offloadRef("ripgrep", json.RawMessage(`{"query":"needle"}`)); !strings.Contains(r, "needle") {
		t.Fatalf("ripgrep pointer should carry the query: %q", r)
	}
	// Unknown/empty tool falls back to the legacy placeholder (still recognized as offloaded).
	if r := offloadRef("", nil); r != OmittedToolResult || !isOffloaded(r) {
		t.Fatalf("empty tool should fall back to the legacy placeholder: %q", r)
	}
}

func TestEvictOffloadsSupersededReads(t *testing.T) {
	// Read x, an unrelated y, then read x AGAIN. The older read of x is superseded and must be
	// offloaded even though keepRecent (3 ≥ all results) would keep it by recency — relevance wins.
	ms := []wire.Message{
		userText("work"),
		asstToolUse("a", "read_file", `{"path":"x.go"}`),
		toolResult("a", "OLD x contents"),
		asstToolUse("b", "read_file", `{"path":"y.go"}`),
		toolResult("b", "y contents"),
		asstToolUse("c", "read_file", `{"path":"x.go"}`),
		toolResult("c", "NEW x contents"),
	}
	n := EvictStaleToolResults(ms, 3)
	got := map[string]string{}
	for _, m := range ms {
		for _, b := range m.Blocks {
			if b.Type == "tool_result" {
				got[b.ToolUseID] = b.Content
			}
		}
	}
	if !isOffloaded(got["a"]) {
		t.Fatalf("superseded older read of x.go should be offloaded even within keepRecent: %q", got["a"])
	}
	if got["c"] != "NEW x contents" {
		t.Fatalf("the latest read of x.go must survive verbatim: %q", got["c"])
	}
	if got["b"] != "y contents" {
		t.Fatalf("an unrelated recent read should be kept: %q", got["b"])
	}
	if n != 1 {
		t.Fatalf("only the 1 superseded read should be offloaded, got %d", n)
	}
}

// TestSupersededReadsRangeAware: with line ranges, superseding is per-SLICE —
// reading lines 500-600 must not supersede a live read of lines 1-100 — while a
// re-read of the SAME slice still supersedes its older copy, and a later FULL
// read covers (supersedes) every earlier range of that path.
func TestSupersededReadsRangeAware(t *testing.T) {
	ms := []wire.Message{
		userText("work"),
		asstToolUse("r1", "read_file", `{"path":"x.go","start_line":1,"end_line":100}`),
		toolResult("r1", "lines 1-100"),
		asstToolUse("r2", "read_file", `{"path":"x.go","start_line":500,"end_line":600}`),
		toolResult("r2", "lines 500-600"),
		asstToolUse("r3", "read_file", `{"path":"x.go","start_line":500,"end_line":600}`),
		toolResult("r3", "lines 500-600 AGAIN"),
	}
	n := EvictStaleToolResults(ms, 10)
	got := map[string]string{}
	for _, m := range ms {
		for _, b := range m.Blocks {
			if b.Type == "tool_result" {
				got[b.ToolUseID] = b.Content
			}
		}
	}
	if isOffloaded(got["r1"]) {
		t.Fatalf("a different range must NOT be superseded by a later range read: %q", got["r1"])
	}
	if !isOffloaded(got["r2"]) || got["r3"] != "lines 500-600 AGAIN" {
		t.Fatalf("same-slice re-read supersedes its older copy only: r2=%q r3=%q", got["r2"], got["r3"])
	}
	if !strings.Contains(got["r2"], "lines 500-600") {
		t.Fatalf("a range read's pointer must carry the range: %q", got["r2"])
	}
	if n != 1 {
		t.Fatalf("exactly the older same-slice copy should offload, got %d", n)
	}

	// A later FULL read covers every earlier range of the path.
	ms2 := []wire.Message{
		userText("work"),
		asstToolUse("a", "read_file", `{"path":"y.go","start_line":1,"end_line":50}`),
		toolResult("a", "y range"),
		asstToolUse("b", "read_file", `{"path":"y.go"}`),
		toolResult("b", "y FULL"),
	}
	EvictStaleToolResults(ms2, 10)
	got2 := map[string]string{}
	for _, m := range ms2 {
		for _, b := range m.Blocks {
			if b.Type == "tool_result" {
				got2[b.ToolUseID] = b.Content
			}
		}
	}
	if !isOffloaded(got2["a"]) || got2["b"] != "y FULL" {
		t.Fatalf("a later full read must supersede earlier ranges: a=%q b=%q", got2["a"], got2["b"])
	}
}

// TestEvictKeepsPinnedHotPaths: a HOT path's latest read survives the
// keepRecent cutoff — the anti-thrash pin. Everything else evicts normally.
func TestEvictKeepsPinnedHotPaths(t *testing.T) {
	ms := []wire.Message{
		userText("work"),
		asstToolUse("hot", "read_file", `{"path":"hot.go"}`),
		toolResult("hot", "HOT contents"),
		asstToolUse("c1", "ripgrep", `{"q":"a"}`),
		toolResult("c1", "cold one"),
		asstToolUse("c2", "ripgrep", `{"q":"b"}`),
		toolResult("c2", "cold two"),
		asstToolUse("c3", "ripgrep", `{"q":"c"}`),
		toolResult("c3", "cold three"),
	}
	// keep=2: without the pin, hot.go (oldest) and c1 would offload.
	n := EvictStaleToolResultsOpts(ms, 2, EvictOpts{Pinned: func(p string) bool { return p == "hot.go" }})
	got := map[string]string{}
	for _, m := range ms {
		for _, b := range m.Blocks {
			if b.Type == "tool_result" {
				got[b.ToolUseID] = b.Content
			}
		}
	}
	if got["hot"] != "HOT contents" {
		t.Fatalf("pinned hot path must survive the keep cutoff: %q", got["hot"])
	}
	if !isOffloaded(got["c1"]) {
		t.Fatalf("unpinned old result should still offload: %q", got["c1"])
	}
	if got["c2"] != "cold two" || got["c3"] != "cold three" {
		t.Fatalf("keepRecent window intact: %q %q", got["c2"], got["c3"])
	}
	if n != 1 {
		t.Fatalf("exactly the 1 unpinned old result should offload, got %d", n)
	}
}

// TestEvictStillDedupesSupersededHotReads: pinning protects the LATEST copy of
// a hot path only — older superseded copies are dead weight and must still
// offload, or the pin would re-inflate the bloat dedup exists to remove.
func TestEvictStillDedupesSupersededHotReads(t *testing.T) {
	ms := []wire.Message{
		userText("work"),
		asstToolUse("old", "read_file", `{"path":"hot.go"}`),
		toolResult("old", "OLD hot contents"),
		asstToolUse("new", "read_file", `{"path":"hot.go"}`),
		toolResult("new", "NEW hot contents"),
	}
	n := EvictStaleToolResultsOpts(ms, 10, EvictOpts{Pinned: func(p string) bool { return p == "hot.go" }})
	got := map[string]string{}
	for _, m := range ms {
		for _, b := range m.Blocks {
			if b.Type == "tool_result" {
				got[b.ToolUseID] = b.Content
			}
		}
	}
	if !isOffloaded(got["old"]) {
		t.Fatalf("superseded copy of a pinned path must still offload: %q", got["old"])
	}
	if got["new"] != "NEW hot contents" {
		t.Fatalf("latest copy of the pinned path must survive: %q", got["new"])
	}
	if n != 1 {
		t.Fatalf("exactly the superseded copy should offload, got %d", n)
	}
}

// TestEvictClearsStructuredPayload: providers PREFER ContentBlocks — eviction
// that leaves them intact is a wire no-op. The pointer must be the ONLY payload.
func TestEvictClearsStructuredPayload(t *testing.T) {
	big := strings.Repeat("x", 4000)
	msgs := []wire.Message{
		{Role: "user", Blocks: []wire.Block{{Type: "text", Text: "task"}}},
		{Role: "assistant", Blocks: []wire.Block{{Type: "tool_use", ID: "t1", Name: "ripgrep", Input: []byte(`{"query":"needle"}`)}}},
		{Role: "user", Blocks: []wire.Block{{Type: "tool_result", ToolUseID: "t1", Content: big,
			ContentBlocks: []wire.Block{{Type: "text", Text: big}}}}},
		{Role: "assistant", Blocks: []wire.Block{{Type: "text", Text: "found it"}}},
	}
	if n := EvictStaleToolResults(msgs, 0); n != 1 {
		t.Fatalf("expected 1 eviction, got %d", n)
	}
	res := msgs[2].Blocks[0]
	if len(res.ContentBlocks) != 0 {
		t.Error("ContentBlocks must be dropped — providers prefer them, so leaving them ships the full dump anyway")
	}
	if !isOffloaded(res.Content) || strings.Contains(res.Content, "xxxx") {
		t.Errorf("Content should be the typed pointer, got %q", clip(res.Content, 80))
	}
	if res.ToolUseID != "t1" {
		t.Error("adjacency must hold: ToolUseID preserved")
	}
}

// TestEvictHandlesImageOnlyResults: structured-only results (screenshots — the
// biggest blocks, with empty flat Content) must be evictable and visible to the
// token estimate.
func TestEvictHandlesImageOnlyResults(t *testing.T) {
	img := wire.Block{Type: "image", Source: &wire.MediaSource{MediaType: "image/png", Data: strings.Repeat("A", 40_000)}}
	msgs := []wire.Message{
		{Role: "user", Blocks: []wire.Block{{Type: "text", Text: "look"}}},
		{Role: "assistant", Blocks: []wire.Block{{Type: "tool_use", ID: "s1", Name: "browser_screenshot", Input: []byte(`{}`)}}},
		{Role: "user", Blocks: []wire.Block{{Type: "tool_result", ToolUseID: "s1", ContentBlocks: []wire.Block{img}}}},
		{Role: "assistant", Blocks: []wire.Block{{Type: "text", Text: "seen"}}},
	}
	before := EstimateTokens(msgs)
	if before < 4000 {
		t.Fatalf("estimate must SEE the image payload, got %d tokens", before)
	}
	if n := EvictStaleToolResults(msgs, 0); n != 1 {
		t.Fatalf("image-only result must be evictable, got %d evictions", n)
	}
	after := EstimateTokens(msgs)
	if after >= before/10 {
		t.Errorf("estimate should collapse after evicting the image: %d -> %d", before, after)
	}
	if len(msgs[2].Blocks[0].ContentBlocks) != 0 || msgs[2].Blocks[0].Content == "" {
		t.Errorf("image payload must be swapped for a pointer: %+v", msgs[2].Blocks[0])
	}
}

// TestEvictIdempotent: a second pass finds nothing new to evict.
func TestEvictIdempotent(t *testing.T) {
	msgs := []wire.Message{
		{Role: "assistant", Blocks: []wire.Block{{Type: "tool_use", ID: "t1", Name: "ripgrep", Input: []byte(`{"query":"q"}`)}}},
		{Role: "user", Blocks: []wire.Block{{Type: "tool_result", ToolUseID: "t1", Content: strings.Repeat("y", 2000)}}},
	}
	if n := EvictStaleToolResults(msgs, 0); n != 1 {
		t.Fatalf("first pass should evict 1, got %d", n)
	}
	if n := EvictStaleToolResults(msgs, 0); n != 0 {
		t.Errorf("second pass must be a no-op, got %d", n)
	}
}
