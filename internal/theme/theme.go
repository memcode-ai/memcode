// Package theme defines memcode's color themes: a registry of named palettes
// (semantic color roles for TUI styles, diff RGBs, and chroma syntax-highlight
// style names) plus the active-theme accessor used by the renderer. Themes are
// registered at init time in themes.go; "aurora" is the default. A "random"
// meta-theme re-resolves to a different concrete theme on each launch.
package theme

import (
	"math/rand"
	"sort"
	"sync"

	"github.com/alecthomas/chroma/v2"
	"github.com/alecthomas/chroma/v2/styles"
)

// Palette holds the 13 semantic color roles used by the TUI's lipgloss styles.
// Each field is a hex color string (e.g. "#A78BFA") except ToolGlyph, which
// may be empty to indicate "use the terminal default".
type Palette struct {
	Brand        string // primary brand (purple in aurora)
	Info         string // primary info (blue)
	Secondary    string // secondary info (cyan)
	Success      string // positive/success (green)
	Warning      string // caution (amber)
	WarningAlt   string // caution variant — darker amber
	Danger       string // error/forbidden (red)
	DangerStrong string // error emphasis — lighter red
	Accent       string // accent highlight (magenta)
	Emphasis     string // high-emphasis text (near-white)
	Muted        string // secondary text (gray)
	Dim          string // gutter/divider (dark gray)
	ToolGlyph    string // tool glyph color — hex or "" for terminal default
}

// DiffColors holds the RGB/ANSI values used for diff rendering backgrounds and
// gutters. These are raw numeric values (not hex strings) because diff.go builds
// ANSI escape sequences directly from the RGB components.
type DiffColors struct {
	AddBg         [3]int // RGB for added-line background
	DelBg         [3]int // RGB for deleted-line background
	AddGutter     [3]int // RGB for added-line gutter foreground
	DelGutter     [3]int // RGB for deleted-line gutter foreground
	ContextGutter string // hex for context-line gutter foreground
}

// Theme is a complete color theme: a name, identity line (for the selector),
// palette (TUI styles), diff colors, and a chroma style name for syntax
// highlighting.
type Theme struct {
	Name       string // stable slug — the persisted key (e.g. "tokyonight")
	Display    string // user-facing name for the selector (e.g. "Tokyo Night")
	Identity   string // one-line description for the selector
	Palette    Palette
	Diff       DiffColors
	ChromaName string // chroma style name — must resolve via styles.Get
}

var (
	registry = map[string]Theme{}
	active   Theme
	chosen   = "aurora" // the literal name last Set (e.g. "random"), for persistence
	mu       sync.RWMutex
)

// Register adds a theme to the registry. Call from an init() function.
func Register(t Theme) {
	registry[t.Name] = t
}

// Names returns the sorted list of all registered theme names.
func Names() []string {
	mu.RLock()
	defer mu.RUnlock()
	names := make([]string, 0, len(registry))
	for n := range registry {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Get returns the Theme with the given name, or the zero value if not found.
func Get(name string) Theme {
	mu.RLock()
	defer mu.RUnlock()
	return registry[name]
}

// Active returns the current theme.
func Active() Theme {
	mu.RLock()
	defer mu.RUnlock()
	return active
}

// Set switches the active theme to the named theme. Returns true if the name
// was found and the theme was changed; returns false for unknown names (no
// mutation).
func Set(name string) bool {
	mu.Lock()
	defer mu.Unlock()
	chosen = name // remember the literal choice ("random" persists; it re-resolves each launch)
	// "random" is a meta-theme: resolve it to a different concrete theme each call, so a
	// persisted choice of "random" yields a fresh look on every launch. Excludes the light
	// "dawn" theme so a dark terminal doesn't randomly get a light palette.
	if name == "random" {
		names := make([]string, 0, len(registry))
		for n := range registry {
			if n == "random" || n == "dawn" {
				continue
			}
			names = append(names, n)
		}
		if len(names) == 0 {
			return false
		}
		sort.Strings(names)
		active = registry[names[rand.Intn(len(names))]]
		return true
	}
	t, ok := registry[name]
	if !ok {
		return false
	}
	active = t
	return true
}

// Chosen returns the literal theme name last passed to Set — e.g. "random" — so the
// front-end persists the user's choice rather than the concrete theme it resolved to.
func Chosen() string {
	mu.RLock()
	defer mu.RUnlock()
	return chosen
}

// chromaCache stores resolved *chroma.Style pointers by name, populated lazily
// by ChromaStyle().
var chromaCache = map[string]*chroma.Style{}
var chromaMu sync.RWMutex

// ChromaStyle resolves the active theme's ChromaName to a *chroma.Style,
// caching the result so repeated calls are cheap (map lookup + RLock).
func ChromaStyle() *chroma.Style {
	t := Active()
	name := t.ChromaName

	chromaMu.RLock()
	if s, ok := chromaCache[name]; ok {
		chromaMu.RUnlock()
		return s
	}
	chromaMu.RUnlock()

	s := styles.Get(name)
	if s == nil {
		// Unreachable in practice: themes.go's init() validates every
		// registered theme's ChromaName resolves, and Active() is always a
		// registered theme. But a panic on the engine goroutine's hot render
		// path is the wrong failure mode — degrade to the chroma fallback.
		s = styles.Fallback
	}

	chromaMu.Lock()
	chromaCache[name] = s
	chromaMu.Unlock()

	return s
}
