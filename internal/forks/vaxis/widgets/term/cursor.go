package term

import (
	"github.com/memcode-ai/memcode/internal/forks/vaxis"
)

type cursor struct {
	vaxis.Cell
	style vaxis.CursorStyle

	// position
	row row    // 0-indexed
	col column // 0-indexed

	protected bool

	semanticContent  semanticContent
	semanticClearEOL bool
}
