package websites

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func testKit() Kit {
	return Kit{
		Dependencies:    map[string]string{"react": "19.3.1", "zod": "4.6.7", "gsap": "3.15.0"},
		DevDependencies: map[string]string{"vite": "8.3.5"},
		History: KitHistory{Epoch: 2, Versions: map[string]map[string]int{
			"react": {"19.2.8": 2, "19.3.1": 2},
			"zod":   {"3.25.0": 1, "4.4.3": 2, "4.6.7": 2},
			"gsap":  {"3.15.0": 2},
			"vite":  {"8.1.5": 2, "8.3.5": 2},
		}},
	}
}

func TestPlanKitSyncFollowsKitVersions(t *testing.T) {
	site := Kit{
		Dependencies: map[string]string{
			"react":    "19.2.8", // the kit's old pin: follows
			"gsap":     "3.13.0", // a version the owner chose: kept
			"left-pad": "1.3.0",  // not a kit package: untouched
			"zod":      "4.6.7",  // already current
		},
		DevDependencies: map[string]string{"vite": "8.1.5"},
	}
	p := PlanKitSync(site, testKit())
	want := []KitMove{
		{Name: "react", From: "19.2.8", To: "19.3.1"},
		{Name: "vite", From: "8.1.5", To: "8.3.5", Dev: true},
	}
	if !reflect.DeepEqual(p.Moves, want) {
		t.Fatalf("moves = %+v, want %+v", p.Moves, want)
	}
	if !reflect.DeepEqual(p.Kept, []string{"gsap@3.13.0"}) || p.Migration != nil {
		t.Fatalf("kept = %v, migration = %v", p.Kept, p.Migration)
	}
}

func TestPlanKitSyncOlderEpochNeedsMigration(t *testing.T) {
	site := Kit{Dependencies: map[string]string{"react": "19.2.8", "zod": "3.25.0"}}
	p := PlanKitSync(site, testKit())
	if p.Moves != nil {
		t.Fatalf("moved across an epoch: %+v", p.Moves)
	}
	if !reflect.DeepEqual(p.Migration, []string{"zod@3.25.0"}) {
		t.Fatalf("migration = %v", p.Migration)
	}
}

func TestPlanKitSyncWithoutHistoryMovesNothing(t *testing.T) {
	k := testKit()
	k.History = KitHistory{}
	if p := PlanKitSync(Kit{Dependencies: map[string]string{"react": "19.2.8"}}, k); p.Moves != nil || p.Kept != nil {
		t.Fatalf("%+v", p)
	}
}

func TestKitFromTemplateReadsHistory(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range map[string]string{
		"./package.json":      `{"dependencies":{"react":"19.3.1"}}`,
		"./" + KitHistoryFile: `{"epoch":1,"versions":{"react":{"19.2.8":1,"19.3.1":1}}}`,
		"./src/main.jsx":      `x`,
	} {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		tw.Write([]byte(body))
	}
	tw.Close()
	gz.Close()
	k, err := kitFromTarGz(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if k.Dependencies["react"] != "19.3.1" || k.History.Epoch != 1 || k.History.Versions["react"]["19.2.8"] != 1 {
		t.Fatalf("%+v", k)
	}
}

func TestReadKitReadsHistoryBeside(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"dependencies":{"react":"19.3.1"}}`), 0o644)
	if k, err := ReadKit(filepath.Join(dir, "package.json")); err != nil || k.History.Epoch != 0 {
		t.Fatalf("no history: %+v %v", k.History, err)
	}
	os.WriteFile(filepath.Join(dir, KitHistoryFile), []byte(`{"epoch":3,"versions":{}}`), 0o644)
	if k, err := ReadKit(filepath.Join(dir, "package.json")); err != nil || k.History.Epoch != 3 {
		t.Fatalf("history: %+v %v", k.History, err)
	}
	if err := removeKitHistory(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, KitHistoryFile)); !os.IsNotExist(err) {
		t.Fatal("history file still present")
	}
	if err := removeKitHistory(dir); err != nil {
		t.Fatal("removing twice:", err)
	}
}
