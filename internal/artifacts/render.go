package artifacts

import (
	"fmt"
	"strings"
)

// RenderList formats artifacts as an aligned table — shared by the cobra
// command and the TUI /artifacts slash handler.
func RenderList(list []Artifact) string {
	var b strings.Builder
	titleW, idW := 5, 2
	for _, a := range list {
		if w := len(displayTitle(a)); w > titleW {
			titleW = w
		}
		if len(a.ID) > idW {
			idW = len(a.ID)
		}
	}
	for i, a := range list {
		line := fmt.Sprintf("%-*s  %-*s", titleW, displayTitle(a), idW, a.ID)
		if a.UpdatedAt != "" {
			line += "  " + a.UpdatedAt
		}
		if a.URL != "" {
			line += "  " + a.URL
		}
		b.WriteString(line)
		if i < len(list)-1 {
			b.WriteByte('\n')
		}
	}
	return b.String()
}

func displayTitle(a Artifact) string {
	if strings.TrimSpace(a.Title) == "" {
		return "untitled"
	}
	return a.Title
}
