package cmd

import (
	"io"
	"os"

	"github.com/charmbracelet/colorprofile"
)

// userOut wraps stdout so styled output — lipgloss-rendered text AND the raw-ANSI diff —
// is downgraded to plain text when stdout isn't a TTY (piped/redirected) or NO_COLOR is set.
// lipgloss v2 renders color unconditionally (it dropped v1's global TTY auto-detect), so
// without this a headless `memcode "<task>" > out.txt` would carry escape codes. In a real
// terminal the colors pass through unchanged. Use this for the user-facing headless commands.
func userOut() io.Writer {
	return colorprofile.NewWriter(os.Stdout, os.Environ())
}
