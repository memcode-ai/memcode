package vxui

import (
	"strconv"
	"strings"

	"github.com/memcode-ai/memcode/internal/forks/vaxis/ui"

	"github.com/memcode-ai/memcode/internal/agent/runtime"
	"github.com/memcode-ai/memcode/internal/config"
	"github.com/memcode-ai/memcode/internal/theme"
)

// uiTheme maps the memcode palette onto a ui.Theme. Crucially every surface fill is the
// terminal default (transparent) — the framework's default dark palette fills focused inputs
// with a colored surface (the unwanted pink composer background); memcode draws its own chrome
// with explicit span styles, so the widget surfaces must stay transparent.
func uiTheme(p theme.Palette) ui.Theme {
	th := ui.DefaultTheme()
	transparent := ui.Color(0) // terminal default — no fill
	th.Background = transparent
	th.Surface = transparent
	th.SurfaceRaised = transparent
	th.SurfaceHovered = transparent
	th.SurfacePressed = transparent
	th.Selection = transparent
	th.Foreground = uiColor(p.Emphasis)
	th.MutedForeground = uiColor(p.Muted)
	th.Primary = uiColor(p.Brand)
	th.Border = uiColor(p.Dim)
	return th
}

// persistTheme remembers the chosen theme in the project config so it survives across sessions.
// "random" is persisted literally and re-resolves to a fresh theme each launch.
func (s *appState) persistTheme() {
	s.updateConfig(func(cfg *config.Config) {
		if name := theme.Chosen(); name == "aurora" {
			cfg.Theme = "" // omitempty default
		} else {
			cfg.Theme = name
		}
	})
}

// handleThemeKey drives the theme picker while it's open: ↑↓ live-preview, Enter applies +
// persists, Esc restores the original. Owns the keyboard (every key is consumed).
func (s *appState) handleThemeKey(k string) ui.EventResult {
	switch k {
	case "Up":
		if s.themeSel > 0 {
			s.SetState(func() {
				s.themeSel--
				theme.Set(s.themeNames[s.themeSel])
				s.sty = makeStyles(theme.Active().Palette)
			})
		}
	case "Down":
		if s.themeSel < len(s.themeNames)-1 {
			s.SetState(func() {
				s.themeSel++
				theme.Set(s.themeNames[s.themeSel])
				s.sty = makeStyles(theme.Active().Palette)
			})
		}
	case "Enter":
		applied := theme.Chosen()
		s.SetState(func() { s.themePicking = false })
		s.persistTheme() // survive across sessions ("random" persists + re-resolves each launch)
		s.sysln("theme → " + applied)
	case "Escape":
		s.SetState(func() {
			theme.Set(s.themeOrig)
			s.sty = makeStyles(theme.Active().Palette)
			s.themePicking = false
		})
	}
	return ui.EventHandled
}

// themePickerView is the modal theme overlay: live-preview list + hint.
func (s *appState) themePickerView() ui.Widget {
	rows := []ui.Widget{ui.RichText{Spans: []ui.TextSpan{{Text: "Select a theme", Style: s.sty.brand}}}, ui.SizedBox{Height: 1}}
	for i, name := range s.themeNames {
		marker, nameStyle := "  ", s.sty.muted
		if i == s.themeSel {
			marker, nameStyle = "❯ ", s.sty.emph
		}
		saved := "  "
		if name == s.themeOrig {
			saved = "● "
		}
		rows = append(rows, ui.RichText{Spans: []ui.TextSpan{
			{Text: marker + saved, Style: s.sty.brand},
			{Text: name, Style: nameStyle},
		}})
	}
	// Live sample of real review output (diff + syntax) in the previewed theme's colors —
	// the most theme-distinguishing surface, far clearer than the chrome alone.
	rows = append(rows, ui.SizedBox{Height: 1})
	for _, line := range strings.Split(runtime.RenderThemeSample(72), "\n") {
		rows = append(rows, ui.RichText{Spans: parseANSISpans(line)})
	}
	rows = append(rows, ui.SizedBox{Height: 1},
		ui.RichText{Spans: []ui.TextSpan{{Text: "↑↓ preview · Enter apply · Esc cancel", Style: s.sty.muted}}})
	return ui.Flex{Axis: ui.Vertical, MainAxisSize: ui.MainAxisSizeMin, CrossAxisAlignment: ui.CrossAxisStart, Children: rows}
}

// uiStyles are the live-region styles derived from the active theme's palette.
type uiStyles struct{ brand, muted, warn, emph, info, danger, dim, user ui.Style }

func makeStyles(p theme.Palette) uiStyles {
	return uiStyles{
		brand:  ui.Style{Foreground: uiColor(p.Brand), Attribute: ui.AttrBold},
		muted:  ui.Style{Foreground: uiColor(p.Muted)},
		warn:   ui.Style{Foreground: uiColor(p.Warning), Attribute: ui.AttrBold},
		emph:   ui.Style{Foreground: uiColor(p.Emphasis)},
		info:   ui.Style{Foreground: uiColor(p.Info)},
		danger: ui.Style{Foreground: uiColor(p.Danger), Attribute: ui.AttrBold},
		dim:    ui.Style{Foreground: uiColor(p.Dim)},
		user:   ui.Style{Foreground: uiColor(p.Accent), Attribute: ui.AttrBold},
	}
}

func (st uiStyles) modeStyle(mode string) ui.Style {
	switch {
	case strings.HasPrefix(mode, "plan"): // plan / plan+auto — a distinct cognitive mode, make it stand out
		return st.brand
	case mode == "auto":
		return st.warn
	case mode == "allow-all":
		return st.danger
	default: // ask
		return st.info
	}
}

func uiColor(hex string) ui.Color {
	hex = strings.TrimPrefix(hex, "#")
	if len(hex) != 6 {
		return ui.Color(0)
	}
	v, err := strconv.ParseUint(hex, 16, 32)
	if err != nil {
		return ui.Color(0)
	}
	return ui.RGB(uint8(v>>16), uint8(v>>8), uint8(v))
}
