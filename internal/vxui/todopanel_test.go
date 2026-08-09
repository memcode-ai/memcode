package vxui

import (
	"testing"

	"github.com/memcode-ai/memcode/internal/theme"
	"github.com/memcode-ai/memcode/internal/todos"
)

func TestTodoPanelRenders(t *testing.T) {
	theme.Set("aurora")
	s := &appState{sty: makeStyles(theme.Active().Palette)}

	if rows := s.todoPanel(); rows != nil {
		t.Fatalf("no todos → no panel, got %d rows", len(rows))
	}

	s.todos = todos.List{
		{Title: "first", Status: todos.StatusDone},
		{Title: "second", Status: todos.StatusActive},
		{Title: "third", Status: todos.StatusPending},
	}
	rows := s.todoPanel()
	if len(rows) != 4 { // a "Tasks 1/3" header + one row per item
		t.Fatalf("panel rows = %d, want 4 (header + 3 items)", len(rows))
	}
}
