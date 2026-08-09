package websites

import (
	"fmt"
	"strings"
)

// RenderList formats sites as an aligned table — shared by the cobra command
// and the TUI /websites slash handler.
func RenderList(sites []Site) string {
	var b strings.Builder
	nameW, slugW := 4, 4
	for _, s := range sites {
		if len(s.Name) > nameW {
			nameW = len(s.Name)
		}
		if len(s.Slug) > slugW {
			slugW = len(s.Slug)
		}
	}
	for i, s := range sites {
		status := s.Status
		if status == "" {
			status = "draft"
		}
		line := fmt.Sprintf("%-*s  %-*s  %-9s", nameW, s.Name, slugW, s.Slug, status)
		if s.LiveURL != "" {
			line += "  " + s.LiveURL
		}
		b.WriteString(line)
		if i < len(sites)-1 {
			b.WriteByte('\n')
		}
	}
	return b.String()
}
