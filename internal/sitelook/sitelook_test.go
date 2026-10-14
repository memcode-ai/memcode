package sitelook

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// fakeBrowser is a page of a given height that scrolls like a real one:
// the last screen clamps to the bottom.
type fakeBrowser struct {
	height, vh int
	y          int
	shots      []int
	emulated   string
}

func (f *fakeBrowser) Emulate(_ context.Context, w, h int, mobile bool) error {
	f.vh, f.emulated = h, fmt.Sprintf("%dx%d mobile=%v", w, h, mobile)
	return nil
}
func (f *fakeBrowser) Navigate(context.Context, string) error { return nil }
func (f *fakeBrowser) Eval(_ context.Context, js string) (string, error) {
	switch {
	case strings.Contains(js, "document.fonts.status"):
		return "loaded", nil
	case strings.Contains(js, "__mc.height()"):
		return fmt.Sprintf(`{"h":%d,"title":"Ode"}`, f.height), nil
	case strings.Contains(js, "__mc.scrollTo("):
		var y int
		fmt.Sscanf(js[strings.Index(js, "__mc.scrollTo(")+len("__mc.scrollTo("):], "%d", &y)
		if max := f.height - f.vh; y > max {
			y = max
		}
		f.y = y
		return fmt.Sprint(y), nil
	}
	return "", nil
}
func (f *fakeBrowser) Screenshot(context.Context, bool) ([]byte, string, error) {
	f.shots = append(f.shots, f.y)
	return []byte("png"), "image/png", nil
}
func (f *fakeBrowser) ClearConsole()           {}
func (f *fakeBrowser) Console(string) []string { return nil }

func TestCaptureScrollsScreenByScreen(t *testing.T) {
	loadSettle, scrollSettle = 0, 0
	t.Cleanup(func() { loadSettle, scrollSettle = 1500*time.Millisecond, 900*time.Millisecond })
	vp, _ := ViewportNamed("desktop")

	f := &fakeBrowser{height: 2500}
	look, err := Capture(context.Background(), f, "https://ode.com", vp, 0, 8)
	if err != nil {
		t.Fatal(err)
	}
	// 0, 900, then the bottom (1600): never a repeat, never past the end.
	if got := fmt.Sprint(f.shots); got != "[0 900 1600]" || look.PageHeight != 2500 || look.Title != "Ode" {
		t.Fatalf("shots %s, look %+v", got, look)
	}

	f = &fakeBrowser{height: 10000}
	if _, err := Capture(context.Background(), f, "x", vp, 900, 2); err != nil || fmt.Sprint(f.shots) != "[900 1800]" {
		t.Fatalf("from/screens not honored: %v %v", f.shots, err)
	}
	m, _ := ViewportNamed("mobile")
	if _, err := Capture(context.Background(), f, "x", m, 0, 99); err != nil || len(f.shots) != 2+MaxScreens || !strings.Contains(f.emulated, "mobile=true") {
		t.Fatalf("mobile capture: %d shots, %s", len(f.shots), f.emulated)
	}
}

func TestReportFormat(t *testing.T) {
	r := Report{URL: "https://ode.com", Viewport: Viewports[0], Height: 4461, Title: "Ode",
		Fonts:    []string{"ABC Diatype 400"},
		Type:     []TypeStyle{{Tag: "p", Family: "ABC Diatype", Size: 18, Weight: "400", Line: 1.3, Track: 0.01, Color: "#3c2e2a", Chars: 700, Sample: "We're the end-to-end partner"}},
		Surfaces: []Share{{Color: "#fbfbf8", Pct: 100}},
		Layout:   Layout{ContentWidth: 1267, Gutter: 116, StickyHeader: true, HeaderHeight: 74, ButtonRadius: "8px"},
		Sections: []Section{{Top: 0, Height: 659, BG: "#fbfbf8", Heading: "The team", Size: 18, Tag: "header", Media: "1 canvas"}},
		Motion:   Motion{Libraries: []string{"GSAP 3.15.0", "ScrollTrigger"}, Animated: 4},
	}
	out := r.Format()
	for _, want := range []string{"ABC Diatype 18px/1.30 w400 track +0.010em", "#fbfbf8 100%", "content width ~1267px", "sticky header 74px", "GSAP 3.15.0, ScrollTrigger", `"The team"`, "[1 canvas]"} {
		if !strings.Contains(out, want) {
			t.Errorf("format missing %q:\n%s", want, out)
		}
	}
}
