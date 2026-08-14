package server

import (
	"os"
	"path/filepath"
	"time"
)

// pruneSpool deletes media spool files older than the cutoff — the same
// retention as the durable inbox, so an attachment outlives every task that
// could still reference it. Best-effort: a prune failure never blocks startup.
func pruneSpool(dir string, before time.Time) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		info, err := e.Info()
		if err != nil || !info.ModTime().Before(before) {
			continue
		}
		_ = os.Remove(filepath.Join(dir, e.Name()))
	}
}
