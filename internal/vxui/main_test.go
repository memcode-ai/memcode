package vxui

import (
	"os"
	"testing"
)

// TestMain redirects HOME to a throwaway dir for the whole package, so any test that drives a plan
// turn (which writes ~/.memcode/plans via savePlan) lands in a temp HOME instead of polluting the
// developer's real ~/.memcode/plans. os.UserHomeDir honors $HOME on unix.
func TestMain(m *testing.M) {
	tmp, err := os.MkdirTemp("", "memcode-vxui-home-")
	if err == nil {
		os.Setenv("HOME", tmp)
	}
	code := m.Run()
	if tmp != "" {
		os.RemoveAll(tmp)
	}
	os.Exit(code)
}
