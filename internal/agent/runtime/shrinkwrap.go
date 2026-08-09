package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/memcode-ai/memcode/internal/llm"
	"github.com/memcode-ai/memcode/internal/wire"
)

// Instruction size tiers (bytes). Below shrinkwrap: load MEMCODE.md verbatim — cheap,
// lossless, no model call. Between the two: shrinkwrap it (LLM compression, cached). Above
// refuse: it's a doc-dump, not instructions — don't load it (a notice fires at startup),
// because even compressing something that large is itself a context/cost blowup.
const (
	shrinkwrapBytes = 16 * 1024  // ~4K tokens
	refuseBytes     = 256 * 1024 // ~64K tokens
)

// shrinkwrapTimeout bounds the one-time compression call; shrinkwrapMaxTokens caps its
// output generously — the compressed form is smaller than the input, but a large input
// still needs room so it isn't truncated mid-rule.
const (
	shrinkwrapTimeout   = 90 * time.Second
	shrinkwrapMaxTokens = 32768
)

// tier classifies instruction size into the action to take.
type tier int

const (
	tierLoad   tier = iota // load verbatim
	tierShrink             // compress, then load
	tierRefuse             // too large — refuse to load
)

func instructionTier(n int) tier {
	switch {
	case n > refuseBytes:
		return tierRefuse
	case n > shrinkwrapBytes:
		return tierShrink
	default:
		return tierLoad
	}
}

// shrinkwrapCachePath is where the compressed form of `raw` is cached — under .memcode
// (gitignored, rebuildable state), keyed by the SHA-256 of the ORIGINAL so any edit to
// MEMCODE.md changes the key and auto-invalidates the cache. The user's own MEMCODE.md is
// never touched or renamed; only this derived artifact carries the hash.
func shrinkwrapCachePath(root, raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return filepath.Join(root, ".memcode", "instructions", "MEMCODE."+hex.EncodeToString(sum[:])[:16]+".md")
}

// shrinkwrap returns a compressed form of large instruction text, cached on disk by the
// original's hash. On a cache hit it loads instantly; on a miss it compresses once and
// stores. It FAILS SAFE: if compression errors (e.g. the gateway mode isn't deployed) or
// doesn't actually shrink the text, the full ORIGINAL is returned — a faithful, if larger,
// instruction set beats a lossy or empty one. Caller has already decided this is tierShrink.
func (s *Session) shrinkwrap(ctx context.Context, raw string) string {
	cachePath := shrinkwrapCachePath(s.root, raw)
	if b, err := os.ReadFile(cachePath); err == nil && len(strings.TrimSpace(string(b))) > 0 {
		return string(b)
	}
	compressed, err := s.compressInstructions(ctx, raw)
	if err != nil || compressed == "" || len(compressed) >= len(raw) {
		s.printf("  ⚠ couldn't shrinkwrap %s — loading it in full this session.\n", memcodeMdName)
		return raw
	}
	_ = os.MkdirAll(filepath.Dir(cachePath), 0o755)
	_ = os.WriteFile(cachePath, []byte(compressed), 0o644)
	s.printf("  ⊙ shrinkwrapped %s (%d KB → %d KB; cached under .memcode/instructions).\n",
		memcodeMdName, len(raw)/1024, len(compressed)/1024)
	return compressed
}

// compressInstructions runs the one-time LLM compression via the dedicated "shrinkwrap"
// doctrine mode (its OWN doctrine: preserve every rule, cut only prose — not the compaction
// summarizer). The ladder routes shrinkwrap to the strong balanced tier on its own
// (llm/lane.go's purpose switch), like compaction — a dropped rule is a silently
// violated instruction.
func (s *Session) compressInstructions(ctx context.Context, raw string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, shrinkwrapTimeout)
	defer cancel()
	resp, err := s.sideComplete(cctx, llm.Shrinkwrap, wire.Request{
		Mode:      "shrinkwrap",
		MaxTokens: shrinkwrapMaxTokens,
		Messages:  []wire.Message{{Role: "user", Blocks: []wire.Block{{Type: "text", Text: raw}}}},
	})
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(resp.Text()), nil
}
