package runtime

import (
	"strings"
	"testing"
)

func TestFormatTraceFindsCorruption(t *testing.T) {
	out := formatTrace("trace x", []traceStage{
		{"fetch.raw", 235900, "<!doctype html><html lang=en>"},
		{"extract.text", 33, "When AI builds itself | Anthropic"},
		{"truncate.out", 33, "When AI builds itself | Anthropic"},
	})
	if !strings.Contains(out, "first corruption: extract.text") {
		t.Errorf("should pinpoint extract.text:\n%s", out)
	}
	if !strings.Contains(out, "DROPPED") {
		t.Errorf("should mark the dropped stage:\n%s", out)
	}
	if !strings.Contains(out, "When AI builds itself") {
		t.Errorf("stage preview must be shown (the proof):\n%s", out)
	}
}

func TestFormatTraceRatioDrop(t *testing.T) {
	// >99% loss but output > 500 chars — caught by the <5% ratio flag, not the absolute one.
	out := formatTrace("trace x", []traceStage{
		{"in", 100000, "x"},
		{"out", 1000, "y"},
	})
	if !strings.Contains(out, "DROPPED") {
		t.Errorf("a <5%% survival ratio should flag DROPPED:\n%s", out)
	}
}

func TestFormatTraceHealthyPipeline(t *testing.T) {
	out := formatTrace("trace x", []traceStage{
		{"fetch.raw", 235900, "<!doctype html>"},
		{"extract.text", 34933, "When AI builds itself"},
		{"truncate.out", 34933, "When AI builds itself"},
	})
	if !strings.Contains(out, "survives every stage") {
		t.Errorf("healthy pipeline should not flag corruption:\n%s", out)
	}
}
