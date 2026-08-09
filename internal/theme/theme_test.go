package theme

import (
	"reflect"
	"testing"

	"github.com/alecthomas/chroma/v2/styles"
)

func TestAllThemesHaveAllPaletteRoles(t *testing.T) {
	for _, name := range Names() {
		t.Run(name, func(t *testing.T) {
			th := registry[name]
			v := reflect.ValueOf(th.Palette)
			pt := reflect.TypeOf(th.Palette)
			for i := 0; i < v.NumField(); i++ {
				fn := pt.Field(i).Name
				f := v.Field(i).String()
				if fn == "ToolGlyph" {
					continue // may be empty (aurora uses terminal default)
				}
				if f == "" {
					t.Errorf("Palette.%s is empty", fn)
				}
			}
			// Diff RGBs non-zero
			for _, label := range []string{"AddBg", "DelBg", "AddGutter", "DelGutter"} {
				var rgb [3]int
				switch label {
				case "AddBg":
					rgb = th.Diff.AddBg
				case "DelBg":
					rgb = th.Diff.DelBg
				case "AddGutter":
					rgb = th.Diff.AddGutter
				case "DelGutter":
					rgb = th.Diff.DelGutter
				}
				if rgb == [3]int{} {
					t.Errorf("Diff.%s is zero", label)
				}
			}
			if th.Diff.ContextGutter == "" {
				t.Error("Diff.ContextGutter is empty")
			}
			if th.ChromaName == "" {
				t.Error("ChromaName is empty")
			}
		})
	}
}

func TestAllChromaStylesResolve(t *testing.T) {
	for _, name := range Names() {
		t.Run(name, func(t *testing.T) {
			th := registry[name]
			s := styles.Get(th.ChromaName)
			if s == nil {
				t.Errorf("chroma style %q not found", th.ChromaName)
			}
		})
	}
}

func TestSetActiveRoundTrip(t *testing.T) {
	orig := Active().Name
	if !Set("dracula") {
		t.Fatal("Set(dracula) returned false")
	}
	if Active().Name != "dracula" {
		t.Fatalf("Active().Name = %q, want %q", Active().Name, "dracula")
	}
	// Restore
	Set(orig)
}

func TestSetUnknownIsNoop(t *testing.T) {
	orig := Active().Name
	if Set("nonexistent_theme_xyz") {
		t.Fatal("Set(unknown) returned true")
	}
	if Active().Name != orig {
		t.Fatalf("Active().Name changed from %q to %q", orig, Active().Name)
	}
}

func TestAuroraMatchesCurrentColors(t *testing.T) {
	// This is the pin test — if someone changes a hardcoded color in styles.go
	// or diff.go, this test breaks to remind them to update aurora too.
	th := registry["aurora"]

	// Palette checks (from styles.go)
	checks := []struct {
		field string
		want  string
		got   string
	}{
		{"Brand", "#A78BFA", th.Palette.Brand},
		{"Info", "#60A5FA", th.Palette.Info},
		{"Secondary", "#22D3EE", th.Palette.Secondary},
		{"Success", "#34D399", th.Palette.Success},
		{"Warning", "#FBBF24", th.Palette.Warning},
		{"WarningAlt", "#F59E0B", th.Palette.WarningAlt},
		{"Danger", "#EF4444", th.Palette.Danger},
		{"DangerStrong", "#F87171", th.Palette.DangerStrong},
		{"Accent", "#818CF8", th.Palette.Accent}, // indigo since 2026-07-18 — the fuchsia user-text color clashed with the purple family
		{"Emphasis", "#E5E7EB", th.Palette.Emphasis},
		{"Muted", "#9CA3AF", th.Palette.Muted},
		{"Dim", "#4B5563", th.Palette.Dim},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("aurora Palette.%s = %q, want %q", c.field, c.got, c.want)
		}
	}
	// ToolGlyph is empty for aurora (terminal default)
	if th.Palette.ToolGlyph != "" {
		t.Errorf("aurora Palette.ToolGlyph = %q, want empty", th.Palette.ToolGlyph)
	}
	// Diff checks: ON-THEME — gutters are the palette's own Success/DangerStrong,
	// backgrounds a deep tint of the same hues (aurora previously carried generic
	// github-dark values, the one theme whose diffs clashed with its own chrome).
	if th.Diff.AddBg != [3]int{18, 46, 38} {
		t.Errorf("aurora Diff.AddBg = %v, want [18 46 38]", th.Diff.AddBg)
	}
	if th.Diff.DelBg != [3]int{50, 27, 29} {
		t.Errorf("aurora Diff.DelBg = %v, want [50 27 29]", th.Diff.DelBg)
	}
	if th.Diff.AddGutter != [3]int{52, 211, 153} { // == Palette.Success #34D399
		t.Errorf("aurora Diff.AddGutter = %v, want [52 211 153]", th.Diff.AddGutter)
	}
	if th.Diff.DelGutter != [3]int{248, 113, 113} { // == Palette.DangerStrong #F87171
		t.Errorf("aurora Diff.DelGutter = %v, want [248 113 113]", th.Diff.DelGutter)
	}
	if th.Diff.ContextGutter != "#8b949e" {
		t.Errorf("aurora Diff.ContextGutter = %q, want %q", th.Diff.ContextGutter, "#8b949e")
	}
}

func TestChromaStyleCached(t *testing.T) {
	orig := Active().Name
	Set("aurora")
	defer Set(orig)

	a := ChromaStyle()
	b := ChromaStyle()
	if a != b {
		t.Error("ChromaStyle() returned different pointers — cache not working")
	}
}
