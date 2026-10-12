package websites

import (
	"archive/zip"
	"bytes"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// Zip writes a shareable copy of the site at dir to out: the project's own
// files, with everything that is generated or machine-specific left out
// (node_modules, the build output, git internals, memcode's .memcode
// folder). In a git repo the file list is git's: tracked files plus new ones
// that aren't ignored, so the site's .gitignore decides. Returns the number
// of files written.
func Zip(dir, out string) (int, error) {
	files, err := shareableFiles(dir)
	if err != nil {
		return 0, err
	}
	if len(files) == 0 {
		return 0, errors.New("nothing to zip in " + dir)
	}
	absOut, _ := filepath.Abs(out)
	f, err := os.Create(out)
	if err != nil {
		return 0, err
	}
	zw := zip.NewWriter(f)
	n := 0
	for _, rel := range files {
		src := filepath.Join(dir, rel)
		if abs, _ := filepath.Abs(src); abs == absOut {
			continue // never zip the zip being written
		}
		info, err := os.Lstat(src)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		h, err := zip.FileInfoHeader(info)
		if err != nil {
			zw.Close()
			f.Close()
			return 0, err
		}
		h.Name = filepath.ToSlash(rel)
		h.Method = zip.Deflate
		w, err := zw.CreateHeader(h)
		if err != nil {
			zw.Close()
			f.Close()
			return 0, err
		}
		in, err := os.Open(src)
		if err != nil {
			zw.Close()
			f.Close()
			return 0, err
		}
		_, err = io.Copy(w, in)
		in.Close()
		if err != nil {
			zw.Close()
			f.Close()
			return 0, err
		}
		n++
	}
	if err := zw.Close(); err != nil {
		f.Close()
		return 0, err
	}
	return n, f.Close()
}

// excludedDir reports top-level folders that never belong in a shared copy,
// whatever .gitignore says.
func excludedPath(rel string) bool {
	first := strings.SplitN(filepath.ToSlash(rel), "/", 2)[0]
	switch first {
	case "node_modules", "dist", ".git", ".memcode":
		return true
	}
	return false
}

func shareableFiles(dir string) ([]string, error) {
	var files []string
	cmd := exec.Command("git", "-C", dir, "ls-files", "-z", "--cached", "--others", "--exclude-standard")
	if raw, err := cmd.Output(); err == nil {
		for _, p := range bytes.Split(raw, []byte{0}) {
			if rel := string(p); rel != "" && !excludedPath(rel) {
				files = append(files, rel)
			}
		}
	} else {
		// Not a git repo: walk the folder, skipping the excluded ones.
		err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(dir, path)
			if rel == "." {
				return nil
			}
			if excludedPath(rel) {
				if d.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			if !d.IsDir() {
				files = append(files, rel)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	sort.Strings(files)
	return files, nil
}
