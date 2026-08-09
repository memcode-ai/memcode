// Command vxspike proves vaxis PrimaryScreen is the right foundation for memcode's TUI:
// a persistent, themed, DYNAMICALLY-SIZED live region (status bar + composer, and an
// expandable menu) with durable native scrollback above it — and no cursor probes, so the
// glitch class the fork fights simply can't occur.
//
//	cd cli && go run ./internal/vxui/spike
//
// Type to edit the composer. Enter appends the line to native scrollback. Tab toggles a
// fake slash-menu (proving the live region grows/shrinks via SetPrimaryScreenRegionHeight,
// memcode's dynamic case: composer growth, slash menu, cards, pickers). Ctrl+C quits.
package main

import (
	"fmt"
	"os"

	vaxis "github.com/memcode-ai/memcode/internal/forks/vaxis"
	"github.com/memcode-ai/memcode/internal/forks/vaxis/widgets/textinput"
)

// memcode-ish theme colors, drawn natively as vaxis Styles (proves the palette renders).
var (
	brand  = vaxis.Style{Foreground: vaxis.HexColor(0xc792ea), Attribute: vaxis.AttrBold}
	muted  = vaxis.Style{Foreground: vaxis.HexColor(0x6b7280)}
	menuHi = vaxis.Style{Foreground: vaxis.HexColor(0x82aaff), Attribute: vaxis.AttrBold}
)

var menu = []string{"/help", "/plan", "/mode", "/theme", "/cost"}

func main() {
	vx, err := vaxis.New(vaxis.Options{
		DisableMouse:  true,
		PrimaryScreen: &vaxis.PrimaryScreenOptions{RegionHeight: 2},
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "vaxis init:", err)
		os.Exit(1)
	}
	defer vx.Close()

	ti := textinput.New()
	ti.SetPrompt("› ")

	menuOpen := false
	sent := 0

	draw := func() {
		// Dynamic live-region height, recomputed every frame: status(1) + composer(1)
		// + the menu rows when open. This is exactly how memcode would size its region
		// (status + composer + any active card/menu) — one call, no anchor gymnastics.
		h := 2
		if menuOpen {
			h += len(menu)
		}
		vx.SetPrimaryScreenRegionHeight(h)

		win := vx.Window()
		win.Clear()
		win.Println(0,
			vaxis.Segment{Text: "memcode", Style: brand},
			vaxis.Segment{Text: fmt.Sprintf("  · vaxis spike · main · clean · %d sent · Tab=menu", sent), Style: muted},
		)
		ti.Draw(win.New(0, 1, win.Width, 1))
		if menuOpen {
			for i, item := range menu {
				st := muted
				if i == 0 {
					st = menuHi
				}
				win.Println(2+i, vaxis.Segment{Text: "  " + item, Style: st})
			}
		}
		vx.Render()
	}
	draw()

	for ev := range vx.Events() {
		switch ev := ev.(type) {
		case vaxis.Key:
			switch {
			case ev.MatchString("Ctrl+c"):
				return
			case ev.MatchString("Enter"):
				if s := ti.String(); s != "" {
					// Scrollback takes raw ANSI — lipgloss-styled output renders natively
					// here (the terminal draws it), no shim needed for scrollback.
					vx.AppendString("\x1b[38;2;130;170;255m› you:\x1b[0m " + s + "\n")
					ti.SetContent("")
					sent++
				}
				menuOpen = false
			case ev.MatchString("Tab"):
				menuOpen = !menuOpen
			default:
				ti.Update(ev)
			}
			draw()
		case vaxis.Resize:
			draw()
		}
	}
}
