package policy

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/memcode-ai/memcode/internal/atomicfile"
)

// store.go — where policy lives on disk.
//
// Two plain JSON files, deliberately separate from .memcode/config.json and
// from ~/.config/memcode/prefs.json. Policy has to be listed, diffed and reset
// as a unit — "reset how explore agents behave" deletes one object — which a
// field scattered among unrelated settings cannot support.

const fileName = "policy.json"

type file struct {
	Policies Set `json:"policies"`
}

// WorkspacePath is <root>/.memcode/policy.json.
func WorkspacePath(root string) string { return filepath.Join(root, ".memcode", fileName) }

// UserPath is $XDG_CONFIG_HOME/memcode/policy.json, else
// ~/.config/memcode/policy.json. "" when no home directory can be determined.
func UserPath() string {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "memcode", fileName)
}

// Load reads a policy file. A missing or corrupt file is "no policy", never an
// error: a settings file that fails to parse must not stop a session starting.
func Load(path string) Set {
	if path == "" {
		return Set{}
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return Set{}
	}
	var f file
	if json.Unmarshal(b, &f) != nil || f.Policies == nil {
		return Set{}
	}
	return f.Policies
}

// Save writes a policy file atomically.
func Save(path string, s Set) error {
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(file{Policies: s}, "", "  ")
	if err != nil {
		return err
	}
	return atomicfile.WriteFile(path, append(b, '\n'), 0o600)
}

// SetField validates a value and writes it to one scope's file.
func SetField(path string, t Target, field string, raw any) error {
	v, err := Validate(t, field, raw)
	if err != nil {
		return err
	}
	s := Load(path)
	s.Put(t, field, v)
	return Save(path, s)
}

// UnsetTarget removes a whole target from one scope, leaving every other scope
// untouched — resetting "how explore agents behave" in this repo must not
// disturb what was set everywhere.
func UnsetTarget(path string, t Target) error {
	s := Load(path)
	if _, ok := s[t]; !ok {
		return nil
	}
	s.Clear(t)
	return Save(path, s)
}

// writeRaw is a test seam for producing a corrupt file.
func writeRaw(path, body string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(body), 0o600)
}
