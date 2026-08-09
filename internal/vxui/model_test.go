package vxui

import (
	"strings"
	"testing"
)

// TestModelPickerRowsByokColumn locks the /model picker's four-column layout:
// name, description, context window, then a trailing byok tag ONLY on models
// served by a vendor the user brought a key for — aligned as a straight column
// across rows, absent entirely when no entry is keyed.
func TestModelPickerRowsByokColumn(t *testing.T) {
	entries := []modelEntry{
		{}, // Automatic
		{label: "sonnet", name: "Sonnet 5", desc: "Everyday Claude", window: 1_000_000, byok: true},
		{label: "terra", name: "GPT-5.6 Terra", desc: "Everyday OpenAI", window: 400_000},
		{label: "kimi-k3", name: "Kimi K3", desc: "Moonshot flagship", window: 1_000_000, byok: true},
	}
	rows := modelPickerRows(entries, "")
	if len(rows) != len(entries) {
		t.Fatalf("rows = %d, want %d", len(rows), len(entries))
	}
	if strings.Contains(rows[0], "byok") || !strings.Contains(rows[0], "Automatic (recommended) ✔") {
		t.Fatalf("Automatic row must carry the ✔ and no byok tag: %q", rows[0])
	}
	for i, wantTag := range map[int]bool{1: true, 2: false, 3: true} {
		if got := strings.HasSuffix(rows[i], "byok"); got != wantTag {
			t.Errorf("row %d byok tag = %v, want %v: %q", i, got, wantTag, rows[i])
		}
	}
	// The tag is a straight column: both byok rows end at the same width.
	if len(rows[1]) != len(rows[3]) {
		t.Errorf("byok column must align: %q (%d) vs %q (%d)", rows[1], len(rows[1]), rows[3], len(rows[3]))
	}

	// No keyed entries → no byok column anywhere (layout identical to pre-BYOK).
	for i := range entries {
		entries[i].byok = false
	}
	for _, row := range modelPickerRows(entries, "") {
		if strings.Contains(row, "byok") {
			t.Errorf("unkeyed picker must not render a byok tag: %q", row)
		}
	}
}

// TestEndpointPickerRows locks the endpoint-mode /model layout: no Automatic
// row (the endpoint has none — entries simply never include it), model ids as
// both name and pin value, the window column blank for uncataloged local
// models, and the trailing free-text row rendered as the typing entry.
func TestEndpointPickerRows(t *testing.T) {
	entries := []modelEntry{
		{label: "mistral:latest", name: "mistral:latest"},
		{label: "sonnet", name: "sonnet", window: 1_000_000},
		{freeText: true},
	}
	rows := modelPickerRows(entries, "mistral:latest")
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want 3", len(rows))
	}
	for _, r := range rows {
		if strings.Contains(r, "Automatic") {
			t.Errorf("endpoint picker must have NO Automatic row: %q", r)
		}
	}
	if !strings.Contains(rows[0], "mistral:latest ✔") {
		t.Errorf("current endpoint model must carry the ✔: %q", rows[0])
	}
	// Uncataloged model: blank window column (no made-up number).
	if strings.Contains(rows[0], "K") || strings.Contains(rows[0], "M") {
		t.Errorf("uncataloged model must show no window: %q", rows[0])
	}
	// Cataloged id served locally still shows its real window.
	if !strings.Contains(rows[1], "1M") {
		t.Errorf("cataloged id must show its window: %q", rows[1])
	}
	if !strings.Contains(rows[2], "Type a model id…") {
		t.Errorf("free-text row missing: %q", rows[2])
	}
	if strings.Contains(rows[2], "✔") {
		t.Errorf("free-text row can never be current: %q", rows[2])
	}
}

// TestCostShowsUSD pins the /cost display rule: hosted always prices; a custom
// endpoint prices only cataloged models — uncataloged local ids show token
// counts, never a defaults-card fabrication.
func TestCostShowsUSD(t *testing.T) {
	if !costShowsUSD(false, "openhermes:latest") {
		t.Error("hosted sessions always show $ (the gateway meters for real)")
	}
	if costShowsUSD(true, "openhermes:latest") {
		t.Error("uncataloged endpoint model must hide $")
	}
	if !costShowsUSD(true, "sonnet") {
		t.Error("a cataloged id behind a local endpoint has a real rate card — show $")
	}
	if costShowsUSD(true, "") {
		t.Error("no model resolved yet must hide $ in endpoint mode")
	}
}
