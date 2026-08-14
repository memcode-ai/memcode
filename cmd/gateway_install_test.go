package cmd

import (
	"strings"
	"testing"
)

func TestGatewayUnitDarwin(t *testing.T) {
	path, content, start, err := gatewayUnit("darwin", "/Users/tim", "/usr/local/bin/memcode", "/work/proj")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(path, "Library/LaunchAgents/ai.memcode.gateway.plist") {
		t.Errorf("path = %q", path)
	}
	for _, want := range []string{"/usr/local/bin/memcode", "<string>gateway</string>", "/work/proj", "KeepAlive"} {
		if !strings.Contains(content, want) {
			t.Errorf("plist missing %q:\n%s", want, content)
		}
	}
	if !strings.Contains(start, "launchctl load") {
		t.Errorf("start cmd = %q", start)
	}
}

func TestGatewayUnitLinux(t *testing.T) {
	path, content, start, err := gatewayUnit("linux", "/home/tim", "/usr/bin/memcode", "/work/proj")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(path, ".config/systemd/user/memcode-gateway.service") {
		t.Errorf("path = %q", path)
	}
	for _, want := range []string{"ExecStart=/usr/bin/memcode gateway", "WorkingDirectory=/work/proj", "Restart=always"} {
		if !strings.Contains(content, want) {
			t.Errorf("unit missing %q:\n%s", want, content)
		}
	}
	if !strings.Contains(start, "systemctl --user") {
		t.Errorf("start cmd = %q", start)
	}
}

func TestGatewayUnitUnsupported(t *testing.T) {
	if _, _, _, err := gatewayUnit("plan9", "/home/tim", "/bin/memcode", "/work"); err == nil {
		t.Error("an unsupported OS must return an error")
	}
}
