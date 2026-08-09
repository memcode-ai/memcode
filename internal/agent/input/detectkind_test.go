package input

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDetectKindByContent(t *testing.T) {
	dir := t.TempDir()
	w := func(name string, b []byte) string { p := filepath.Join(dir, name); os.WriteFile(p, b, 0o644); return p }
	cases := []struct {
		name  string
		bytes []byte
		want  Kind
	}{
		{"real.png", []byte("\x89PNG\r\n\x1a\nrest of the file"), KindImage},
		{"fake.png", []byte("this is plainly text, not an image at all"), KindText}, // mislabeled → NOT image
		{"x.go", []byte("package main\nfunc main(){}"), KindText},
		{"a.bin", []byte{0, 1, 2, 3, 0xff, 0xfe, 0x00, 0x10}, KindBinary},
		{"app.exe", []byte("MZ\x90\x00\x03\x00\x00\x00binary"), KindBinary},
		{"deck.pdf", []byte("%PDF-1.7\nfake body"), KindPDF},           // sniffed as pdf
		{"renamed.dat", []byte("%PDF-1.4\nno pdf extension"), KindPDF}, // magic wins over extension
	}
	for _, c := range cases {
		if k, _ := detectKind(w(c.name, c.bytes)); k != c.want {
			t.Errorf("%s: got %s, want %s", c.name, k, c.want)
		}
	}
}
