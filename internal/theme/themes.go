package theme

import (
	"fmt"
	"reflect"

	"github.com/alecthomas/chroma/v2"
	"github.com/alecthomas/chroma/v2/styles"
)

func init() {
	// A custom monochrome-green chroma style for the matrix theme — code renders like a
	// phosphor terminal (bright green on black), the look in the reference screenshot.
	styles.Register(chroma.MustNewStyle("matrix", chroma.StyleEntries{
		chroma.Background:      "#00FF41 bg:#000000",
		chroma.Comment:         "#008F11 italic",
		chroma.CommentPreproc:  "#00B22D",
		chroma.Keyword:         "#39FF14 bold",
		chroma.KeywordType:     "#00FFA3",
		chroma.NameBuiltin:     "#00FFA3",
		chroma.NameFunction:    "#7CFF7C bold",
		chroma.NameClass:       "#7CFF7C bold",
		chroma.Name:            "#00FF41",
		chroma.String:          "#00CC33",
		chroma.Number:          "#66FF66",
		chroma.Operator:        "#00AA22",
		chroma.Punctuation:     "#00BB2E",
		chroma.GenericInserted: "#00FF41",
		chroma.GenericDeleted:  "#FF5555",
		chroma.Error:           "#FF5555",
	}))

	// --- aurora (default) — reproduces the current hardcoded colors exactly ---
	Register(Theme{
		Name: "aurora",
		Palette: Palette{
			Brand:        "#A78BFA",
			Info:         "#60A5FA",
			Secondary:    "#22D3EE",
			Success:      "#34D399",
			Warning:      "#FBBF24",
			WarningAlt:   "#F59E0B",
			Danger:       "#EF4444",
			DangerStrong: "#F87171",
			Accent:       "#818CF8", // indigo — in the purple family (was fuchsia #E879F9, which clashed as the user-text color)
			Emphasis:     "#E5E7EB",
			Muted:        "#9CA3AF",
			Dim:          "#4B5563",
			ToolGlyph:    "", // empty = terminal default green (ANSI SGR 2)
		},
		Diff: DiffColors{
			// On-theme like every other dark theme: gutters carry the palette's own
			// Success (#34D399) and DangerStrong (#F87171), backgrounds a deep tint
			// of the same hues. Aurora was the ONE theme left with generic
			// github-dark values — the default theme clashing with its own chrome.
			AddBg:         [3]int{18, 46, 38},
			DelBg:         [3]int{50, 27, 29},
			AddGutter:     [3]int{52, 211, 153},
			DelGutter:     [3]int{248, 113, 113},
			ContextGutter: "#8b949e",
		},
		ChromaName: "github-dark",
	})

	// --- dawn — the one LIGHT theme. Auto-selected when memcode detects a light terminal
	// background (see the BackgroundColorMsg handler); also pickable in /theme. Colors are
	// chosen dark enough to read on a light background. ---
	Register(Theme{
		Name: "dawn",
		Palette: Palette{
			Brand:        "#7C3AED",
			Info:         "#2563EB",
			Secondary:    "#0891B2",
			Success:      "#059669",
			Warning:      "#D97706",
			WarningAlt:   "#B45309",
			Danger:       "#DC2626",
			DangerStrong: "#E11D48",
			Accent:       "#DB2777",
			Emphasis:     "#111827",
			Muted:        "#6B7280",
			Dim:          "#D1D5DB",
			ToolGlyph:    "#16A34A",
		},
		Diff: DiffColors{
			AddBg:         [3]int{220, 252, 231},
			DelBg:         [3]int{254, 226, 226},
			AddGutter:     [3]int{22, 163, 74},
			DelGutter:     [3]int{220, 38, 38},
			ContextGutter: "#6e7781",
		},
		ChromaName: "github",
	})

	// --- dracula ---
	Register(Theme{
		Name: "dracula",
		Palette: Palette{
			Brand:        "#bd93f9",
			Info:         "#8be9fd",
			Secondary:    "#50fa7b",
			Success:      "#50fa7b",
			Warning:      "#f1fa8c",
			WarningAlt:   "#ffb86c",
			Danger:       "#ff5555",
			DangerStrong: "#ff6e6e",
			Accent:       "#ff79c6",
			Emphasis:     "#f8f8f2",
			Muted:        "#6272a4",
			Dim:          "#44475a",
			ToolGlyph:    "#50fa7b",
		},
		Diff: DiffColors{
			AddBg:         [3]int{40, 60, 40},
			DelBg:         [3]int{70, 30, 35},
			AddGutter:     [3]int{80, 250, 123},
			DelGutter:     [3]int{255, 85, 85},
			ContextGutter: "#6272a4",
		},
		ChromaName: "dracula",
	})

	// --- tokyonight ---
	Register(Theme{
		Name: "tokyonight",
		Palette: Palette{
			Brand:        "#bb9af7",
			Info:         "#7aa2f7",
			Secondary:    "#7dcfff",
			Success:      "#9ece6a",
			Warning:      "#e0af68",
			WarningAlt:   "#ff9e64",
			Danger:       "#f7768e",
			DangerStrong: "#ff9eb0",
			Accent:       "#ff007c",
			Emphasis:     "#c0caf5",
			Muted:        "#565f89",
			Dim:          "#3b4261",
			ToolGlyph:    "#9ece6a",
		},
		Diff: DiffColors{
			AddBg:         [3]int{30, 60, 30},
			DelBg:         [3]int{65, 25, 35},
			AddGutter:     [3]int{158, 206, 106},
			DelGutter:     [3]int{247, 118, 142},
			ContextGutter: "#565f89",
		},
		ChromaName: "tokyonight-night",
	})

	// --- nord ---
	Register(Theme{
		Name: "nord",
		Palette: Palette{
			Brand:        "#b48ead",
			Info:         "#81a1c1",
			Secondary:    "#88c0d0",
			Success:      "#a3be8c",
			Warning:      "#ebcb8b",
			WarningAlt:   "#d08770",
			Danger:       "#bf616a",
			DangerStrong: "#d07880",
			Accent:       "#b48ead",
			Emphasis:     "#eceff4",
			Muted:        "#d08770",
			Dim:          "#4c566a",
			ToolGlyph:    "#a3be8c",
		},
		Diff: DiffColors{
			AddBg:         [3]int{35, 55, 40},
			DelBg:         [3]int{60, 25, 30},
			AddGutter:     [3]int{163, 190, 140},
			DelGutter:     [3]int{191, 97, 106},
			ContextGutter: "#4c566a",
		},
		ChromaName: "nord",
	})

	// --- gruvbox ---
	Register(Theme{
		Name: "gruvbox",
		Palette: Palette{
			Brand:        "#b16286",
			Info:         "#458588",
			Secondary:    "#689d6a",
			Success:      "#98971a",
			Warning:      "#d79921",
			WarningAlt:   "#cc241d",
			Danger:       "#cc241d",
			DangerStrong: "#fb4934",
			Accent:       "#d3869b",
			Emphasis:     "#ebdbb2",
			Muted:        "#928374",
			Dim:          "#504945",
			ToolGlyph:    "#98971a",
		},
		Diff: DiffColors{
			AddBg:         [3]int{50, 55, 20},
			DelBg:         [3]int{60, 20, 20},
			AddGutter:     [3]int{152, 151, 26},
			DelGutter:     [3]int{251, 73, 52},
			ContextGutter: "#928374",
		},
		ChromaName: "gruvbox",
	})

	// --- solarized ---
	Register(Theme{
		Name: "solarized",
		Palette: Palette{
			Brand:        "#6c71c4",
			Info:         "#268bd2",
			Secondary:    "#2aa198",
			Success:      "#859900",
			Warning:      "#b58900",
			WarningAlt:   "#cb4b16",
			Danger:       "#dc322f",
			DangerStrong: "#e05050",
			Accent:       "#d33682",
			Emphasis:     "#eee8d5",
			Muted:        "#839496",
			Dim:          "#586e75",
			ToolGlyph:    "#859900",
		},
		Diff: DiffColors{
			AddBg:         [3]int{30, 50, 25},
			DelBg:         [3]int{60, 22, 22},
			AddGutter:     [3]int{133, 153, 0},
			DelGutter:     [3]int{220, 50, 47},
			ContextGutter: "#586e75",
		},
		ChromaName: "solarized-dark",
	})

	// --- catppuccin ---
	Register(Theme{
		Name: "catppuccin",
		Palette: Palette{
			Brand:        "#cba6f7",
			Info:         "#89b4fa",
			Secondary:    "#94e2d5",
			Success:      "#a6e3a1",
			Warning:      "#f9e2af",
			WarningAlt:   "#fab387",
			Danger:       "#f38ba8",
			DangerStrong: "#eba0ac",
			Accent:       "#f5c2e7",
			Emphasis:     "#cdd6f4",
			Muted:        "#a6adc8",
			Dim:          "#45475a",
			ToolGlyph:    "#a6e3a1",
		},
		Diff: DiffColors{
			AddBg:         [3]int{30, 50, 35},
			DelBg:         [3]int{55, 25, 35},
			AddGutter:     [3]int{166, 227, 161},
			DelGutter:     [3]int{243, 139, 168},
			ContextGutter: "#6c7086",
		},
		ChromaName: "catppuccin-mocha",
	})

	// --- onedark ---
	Register(Theme{
		Name: "onedark",
		Palette: Palette{
			Brand:        "#c678dd",
			Info:         "#61afef",
			Secondary:    "#56b6c2",
			Success:      "#98c379",
			Warning:      "#e5c07b",
			WarningAlt:   "#d19a66",
			Danger:       "#e06c75",
			DangerStrong: "#e88991",
			Accent:       "#c678dd",
			Emphasis:     "#abb2bf",
			Muted:        "#7f848e",
			Dim:          "#4b5263",
			ToolGlyph:    "#98c379",
		},
		Diff: DiffColors{
			AddBg:         [3]int{30, 50, 30},
			DelBg:         [3]int{55, 25, 30},
			AddGutter:     [3]int{152, 195, 121},
			DelGutter:     [3]int{224, 108, 117},
			ContextGutter: "#7f848e",
		},
		ChromaName: "onedark",
	})

	// --- rosepine ---
	Register(Theme{
		Name: "rosepine",
		Palette: Palette{
			Brand:        "#c4a7e7",
			Info:         "#9ccfd8",
			Secondary:    "#ea9a97",
			Success:      "#31748f",
			Warning:      "#f6c177",
			WarningAlt:   "#ea9a97",
			Danger:       "#eb6f92",
			DangerStrong: "#eb6f92",
			Accent:       "#c4a7e7",
			Emphasis:     "#e0def4",
			Muted:        "#908caa",
			Dim:          "#393552",
			ToolGlyph:    "#31748f",
		},
		Diff: DiffColors{
			AddBg:         [3]int{25, 45, 40},
			DelBg:         [3]int{55, 20, 30},
			AddGutter:     [3]int{49, 116, 143},
			DelGutter:     [3]int{235, 111, 146},
			ContextGutter: "#6e6a86",
		},
		ChromaName: "rose-pine-moon",
	})

	// --- highcontrast ---
	Register(Theme{
		Name: "highcontrast",
		Palette: Palette{
			Brand:        "#d4bfff",
			Info:         "#82c4ff",
			Secondary:    "#40f0f0",
			Success:      "#40ff90",
			Warning:      "#ffe040",
			WarningAlt:   "#ffaa20",
			Danger:       "#ff4040",
			DangerStrong: "#ff7070",
			Accent:       "#ff60d0",
			Emphasis:     "#ffffff",
			Muted:        "#b0b0b0",
			Dim:          "#606060",
			ToolGlyph:    "#40ff90",
		},
		Diff: DiffColors{
			AddBg:         [3]int{20, 60, 30},
			DelBg:         [3]int{70, 20, 25},
			AddGutter:     [3]int{64, 255, 144},
			DelGutter:     [3]int{255, 64, 64},
			ContextGutter: "#888888",
		},
		ChromaName: "hr_high_contrast",
	})

	// --- matrix — green phosphor on black (digital rain). Pairs with the matrix intro banner. ---
	Register(Theme{
		Name: "matrix",
		Palette: Palette{
			Brand:        "#00FF41",
			Info:         "#22FF66",
			Secondary:    "#00FFA3",
			Success:      "#00FF41",
			Warning:      "#CCFF33",
			WarningAlt:   "#9ACD32",
			Danger:       "#FF5555",
			DangerStrong: "#FF7777",
			Accent:       "#39FF14",
			Emphasis:     "#CCFFCC",
			Muted:        "#1F9F3A",
			Dim:          "#0A3D14",
			ToolGlyph:    "#00FF41",
		},
		Diff: DiffColors{
			AddBg:         [3]int{0, 45, 12},
			DelBg:         [3]int{55, 15, 15},
			AddGutter:     [3]int{0, 255, 65},
			DelGutter:     [3]int{255, 85, 85},
			ContextGutter: "#1F9F3A",
		},
		ChromaName: "matrix",
	})

	// --- pinkpanther — hot-pink brand on dark; the look Tim liked from the spike prompt ---
	Register(Theme{
		Name: "pinkpanther",
		Palette: Palette{
			Brand:        "#F472B6",
			Info:         "#7DD3FC",
			Secondary:    "#F9A8D4",
			Success:      "#34D399",
			Warning:      "#FBBF24",
			WarningAlt:   "#F59E0B",
			Danger:       "#EF4444",
			DangerStrong: "#F87171",
			Accent:       "#EC4899",
			Emphasis:     "#FCE7F3",
			Muted:        "#C4A0B5",
			Dim:          "#5B3A4D",
			ToolGlyph:    "#F472B6",
		},
		Diff: DiffColors{
			// On-theme: adds use the palette's own teal-green Success (#34D399) — not the
			// generic aurora green that was copy-pasted here — and deletes keep the brand
			// hot-pink, so the diff reads as intentional against the pink chrome.
			AddBg:         [3]int{18, 46, 38},
			DelBg:         [3]int{54, 26, 36},
			AddGutter:     [3]int{52, 211, 153},
			DelGutter:     [3]int{244, 114, 182},
			ContextGutter: "#9C7A8E",
		},
		ChromaName: "github-dark",
	})

	// --- random — a meta-theme: theme.Set("random") resolves to a different concrete theme
	// each launch (see theme.go). The placeholder palette below is never used (Set swaps to a
	// real theme); it exists only so the entry validates and shows in the /theme picker. ---
	Register(Theme{
		Name: "random",
		Palette: Palette{
			Brand:        "#A78BFA",
			Info:         "#60A5FA",
			Secondary:    "#22D3EE",
			Success:      "#34D399",
			Warning:      "#FBBF24",
			WarningAlt:   "#F59E0B",
			Danger:       "#EF4444",
			DangerStrong: "#F87171",
			Accent:       "#818CF8", // indigo — in the purple family (was fuchsia #E879F9, which clashed as the user-text color)
			Emphasis:     "#E5E7EB",
			Muted:        "#9CA3AF",
			Dim:          "#4B5563",
			ToolGlyph:    "",
		},
		Diff: DiffColors{
			AddBg:         [3]int{18, 46, 29},
			DelBg:         [3]int{52, 25, 27},
			AddGutter:     [3]int{126, 231, 135},
			DelGutter:     [3]int{255, 130, 130},
			ContextGutter: "#8b949e",
		},
		ChromaName: "github-dark",
	})

	// Set aurora as the default active theme.
	active = registry["aurora"]

	// Validate every registered theme: all Palette fields non-empty (except
	// ToolGlyph which may be empty), all Diff RGBs non-zero, ChromaName
	// resolves to a non-nil style. Panic on invalid registration — this is
	// a developer error.
	for name, t := range registry {
		v := reflect.ValueOf(t.Palette)
		pt := reflect.TypeOf(t.Palette)
		for i := 0; i < v.NumField(); i++ {
			f := v.Field(i).String()
			fn := pt.Field(i).Name
			if fn == "ToolGlyph" {
				continue // may be empty
			}
			if f == "" {
				panic(fmt.Sprintf("theme %q: Palette.%s is empty", name, fn))
			}
		}
		// Diff RGBs must be non-zero
		for _, rgb := range [][3]int{t.Diff.AddBg, t.Diff.DelBg, t.Diff.AddGutter, t.Diff.DelGutter} {
			if rgb == [3]int{} {
				panic(fmt.Sprintf("theme %q: Diff has zero RGB", name))
			}
		}
		if t.Diff.ContextGutter == "" {
			panic(fmt.Sprintf("theme %q: Diff.ContextGutter is empty", name))
		}
		if t.ChromaName == "" {
			panic(fmt.Sprintf("theme %q: ChromaName is empty", name))
		}
		if styles.Get(t.ChromaName) == nil {
			panic(fmt.Sprintf("theme %q: chroma style %q not found", name, t.ChromaName))
		}
	}
}
