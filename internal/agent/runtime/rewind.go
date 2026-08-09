package runtime

import (
	"encoding/json"

	"github.com/memcode-ai/memcode/internal/agent/tools"
	"github.com/memcode-ai/memcode/internal/checkpoint"
	"github.com/memcode-ai/memcode/internal/wire"
)

// Checkpoint/rewind glue (see internal/checkpoint): every turn opens a
// checkpoint (runTurn / Run), edit_file calls snapshot their pre-image before
// executing (executeBatchHooked), and /rewind restores the tree to before a
// chosen turn. Agent-edit undo only — bash side effects and human edits are
// out of scope by design (that's git's job).

// snapshotEdits captures the pre-image of every file this batch's edit_file AND apply_patch
// calls are about to touch. Runs before execution, unconditionally (hooks or not) — a
// snapshot miss would silently break rewind, and apply_patch is exactly the multi-file
// refactor a user most wants to undo.
func (s *Session) snapshotEdits(uses []wire.Block) {
	if s.curCkpt == nil {
		return
	}
	for _, u := range uses {
		switch u.Name {
		case tools.EditFile:
			var in struct {
				Path string `json:"path"`
			}
			if json.Unmarshal(u.Input, &in) == nil && in.Path != "" {
				s.curCkpt.Snapshot(in.Path)
			}
		case tools.ApplyPatch:
			var in tools.ApplyPatchInput
			if json.Unmarshal(u.Input, &in) == nil {
				for _, e := range in.Edits {
					if e.Path != "" {
						s.curCkpt.Snapshot(e.Path)
					}
				}
			}
		}
	}
}

// Checkpoints lists this session's rewind points (turns that edited files),
// oldest first.
func (s *Session) Checkpoints() []checkpoint.Manifest { return s.ckpt.List() }

// Rewind restores every file edited from checkpoint seq onward to its state
// before that turn ran. Returns the restored paths.
func (s *Session) Rewind(seq int) ([]string, error) { return s.ckpt.Restore(seq) }
