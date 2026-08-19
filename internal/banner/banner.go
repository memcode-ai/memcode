// Package banner renders memcode's startup wordmark banners. It is the original art from the
// (retired) Bubble Tea TUI, extracted verbatim and de-coupled from the model: the matrix theme's
// digital-rain card is reproduced exactly, parameterized by width/frame/recall so any renderer
// can drive it. Styling is lipgloss (ANSI strings) — no Bubble Tea dependency.
package banner

import (
	"os"
	"path/filepath"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// bigWordmark is the startup "MEMCODE" — 7 rows of chunky 2-thick block letters.
var bigWordmark = []string{
	"██     ██   ████████   ██     ██    ██████     ██████    ███████    ████████",
	"███   ███   ██         ███   ███   ██    ██   ██    ██   ██    ██   ██      ",
	"██ █ █ ██   ██         ██ █ █ ██   ██         ██    ██   ██    ██   ██      ",
	"██  █  ██   ██████     ██  █  ██   ██         ██    ██   ██    ██   ██████  ",
	"██     ██   ██         ██     ██   ██         ██    ██   ██    ██   ██      ",
	"██     ██   ██         ██     ██   ██    ██   ██    ██   ██    ██   ██      ",
	"██     ██   ████████   ██     ██    ██████     ██████    ███████    ████████",
}

const (
	wmCols           = 76
	wmRows           = 7
	bigWordmarkRows  = wmRows + 1
	bigWordmarkWidth = wmCols + 1

	matrixAnimFrames = 150
	matrixInnerH     = 12
	matrixWordTop    = 2
	matrixFallEvery  = 8

	matrixBorderHex = "#00FF41"
)

// glyph is the composited wordmark grid: 0 = empty, 1 = fill, 2 = shadow (3D extrude).
var glyph [bigWordmarkRows][bigWordmarkWidth]byte

func init() {
	for r := 0; r < wmRows; r++ {
		row := []rune(bigWordmark[r])
		for c := 0; c < wmCols; c++ {
			if c < len(row) && row[c] == '█' {
				glyph[r][c] = 1
			}
		}
	}
	for r := bigWordmarkRows - 1; r >= 1; r-- {
		for c := bigWordmarkWidth - 1; c >= 1; c-- {
			if glyph[r][c] == 0 && glyph[r-1][c-1] == 1 {
				glyph[r][c] = 2
			}
		}
	}
}

var matrixGlyphs = []rune("ﾊﾐﾋｰｳｼﾅﾓﾆｻﾜｱｲｳｴｵｶｷｸｹｺｱｦﾝﾘﾂﾃﾅﾆｾﾈ0123456789Zｱ:.=*+<>|╱╲")

var matrixRamp = func() []lipgloss.Style {
	hexes := []string{"#9CFFA8", "#54FF6A", "#22F048", "#00DD38", "#00C230", "#00A828", "#009023", "#007A1E", "#006419", "#004F14", "#003D0F", "#002A0A", "#001B06"}
	out := make([]lipgloss.Style, len(hexes))
	for i, h := range hexes {
		out[i] = lipgloss.NewStyle().Foreground(lipgloss.Color(h))
	}
	return out
}()

var matrixDimStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#008F11"))

var matrixShadowStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#0A5C1A"))

var matrixWordShades = func() []lipgloss.Style {
	hexes := []string{"#39FF14", "#FFFFFF", "#00FF41", "#2BFF52", "#EAFFEA", "#00F03A", "#5EFF6E", "#FFFFFF", "#00FF41", "#39FF14", "#D8FFDF", "#22E84A"}
	out := make([]lipgloss.Style, len(hexes))
	for i, h := range hexes {
		out[i] = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(h))
	}
	return out
}()

// MatrixFrames is the number of animation frames before the landed hold.
func MatrixFrames() int { return matrixAnimFrames }

// IntroSeen reports the animated intro has already played once on this
// machine. The animation is a first-boot moment, not a per-launch toll:
// every later launch rests on the static settled wordmark immediately.
func IntroSeen() bool {
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	_, err = os.Stat(filepath.Join(home, ".memcode", "intro-seen"))
	return err == nil
}

// MarkIntroSeen records that the intro played (called when the animation
// starts, so even a skipped or interrupted first run counts as seen).
func MarkIntroSeen() {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	dir := filepath.Join(home, ".memcode")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(dir, "intro-seen"), []byte{}, 0o644)
}

// Matrix renders one frame of the digital-rain intro for the given terminal width. recall is the
// "↺ recalled …" line, shown near the end. frame >= MatrixFrames() yields the landed splash.
func Matrix(width, frame int, recall string) string {
	innerW := width - 2
	if innerW < bigWordmarkWidth {
		innerW = bigWordmarkWidth
	}
	wordLeft := (innerW - bigWordmarkWidth) / 2
	if wordLeft < 0 {
		wordLeft = 0
	}
	animEnd := matrixAnimFrames
	dim := 0
	if left := animEnd - frame; left < 28 {
		dim = (28 - left) / 6
	}
	lockStart := animEnd - 12
	gframe := frame
	if frame >= lockStart {
		gframe = lockStart
	}
	showRecall := frame >= animEnd-5

	lines := make([]string, matrixInnerH)
	for y := 0; y < matrixInnerH; y++ {
		var b strings.Builder
		for x := 0; x < innerW; x++ {
			wc, wy := x-wordLeft, y-matrixWordTop
			revealed := frame/matrixFallEvery >= y+1
			switch {
			case revealed && matrixWordFill(wy, wc):
				b.WriteString(matrixWordGlyph(wc, wy, gframe))
			case revealed && matrixWordShadow(wy, wc):
				b.WriteString(matrixShadowGlyph(wc, wy, gframe))
			case matrixWordHalo(wy, wc):
				b.WriteString(matrixRainCell(x, y, frame, dim+9))
			default:
				b.WriteString(matrixRainCell(x, y, frame, dim))
			}
		}
		lines[y] = b.String()
	}
	if showRecall && recall != "" {
		lines[matrixInnerH-1] = centerPad(matrixDimStyle.Render(recall), innerW)
	}
	return box(innerW, lines)
}

func box(innerW int, lines []string) string {
	bs := lipgloss.NewStyle().Foreground(lipgloss.Color(matrixBorderHex))
	bar := bs.Render("│")
	var b strings.Builder
	b.WriteString(bs.Render("╭"+strings.Repeat("─", innerW)+"╮") + "\n")
	for _, ln := range lines {
		b.WriteString(bar + centerPad(ln, innerW) + bar + "\n")
	}
	b.WriteString(bs.Render("╰" + strings.Repeat("─", innerW) + "╯"))
	return b.String()
}

func centerPad(s string, w int) string {
	vis := ansi.StringWidth(s)
	if vis >= w {
		return s
	}
	left := (w - vis) / 2
	return strings.Repeat(" ", left) + s + strings.Repeat(" ", w-vis-left)
}

func matrixWordFill(wy, wc int) bool {
	if wy < 0 || wy >= bigWordmarkRows || wc < 0 || wc >= bigWordmarkWidth {
		return false
	}
	for _, d := range [][2]int{{0, 0}, {0, -1}, {-1, 0}} {
		r, c := wy+d[0], wc+d[1]
		if r >= 0 && r < bigWordmarkRows && c >= 0 && c < bigWordmarkWidth && glyph[r][c] == 1 {
			return true
		}
	}
	return false
}

func matrixWordHalo(wy, wc int) bool {
	for dy := -1; dy <= 1; dy++ {
		for dx := -2; dx <= 2; dx++ {
			if matrixWordFill(wy+dy, wc+dx) {
				return true
			}
		}
	}
	return false
}

func matrixWordShadow(wy, wc int) bool {
	return matrixWordFill(wy-1, wc-1) && !matrixWordFill(wy, wc)
}

func matrixWordGlyph(wc, wy, frame int) string {
	g := matrixGlyphs[(wc*131+wy*17+frame/12)%len(matrixGlyphs)]
	i := (matrixHash(wc*31+wy*7) + frame/10) % len(matrixWordShades)
	return matrixWordShades[i].Render(string(g))
}

func matrixShadowGlyph(wc, wy, frame int) string {
	g := matrixGlyphs[(wc*131+wy*17+frame/12)%len(matrixGlyphs)]
	return matrixShadowStyle.Render(string(g))
}

func matrixRainCell(c, y, frame, dim int) string {
	trail := 5 + matrixHash(c*3)%9
	rest := 4 + matrixHash(c*5)%12
	period := matrixInnerH + trail + rest
	speed := matrixFallEvery + matrixHash(c*13)%3
	start := matrixHash(c*7) % 7

	tick := frame/speed - start
	if tick < 0 {
		return " "
	}
	head := tick % period
	dist := head - y
	if dist < 0 || dist >= trail {
		return " "
	}
	depth := matrixHash(c*11) % 2
	idx := dist + depth + dim
	if idx >= len(matrixRamp) {
		idx = len(matrixRamp) - 1
	}
	g := matrixGlyphs[(c*131+y*17+frame/6)%len(matrixGlyphs)]
	return matrixRamp[idx].Render(string(g))
}

func matrixHash(n int) int {
	h := uint32(n)*2654435761 + 2246822519
	h ^= h >> 15
	return int(h & 0x7fffffff)
}
