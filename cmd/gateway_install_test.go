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
	for _, want := range []string{`ExecStart="/usr/bin/memcode" gateway`, `WorkingDirectory="/work/proj"`, "Restart=always"} {
		if !strings.Contains(content, want) {
			t.Errorf("unit missing %q:\n%s", want, content)
		}
	}
	if !strings.Contains(start, "systemctl --user") {
		t.Errorf("start cmd = %q", start)
	}
}

func TestGatewayUnitLinuxEscapesPaths(t *testing.T) {
	// A path with a space must stay one argument (quoted), and a literal % must be
	// doubled so systemd does not read it as a specifier.
	_, content, _, err := gatewayUnit("linux", "/home/tim", "/opt/my apps/memcode", "/work/100%done")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(content, `ExecStart="/opt/my apps/memcode" gateway`) {
		t.Errorf("space in binary path not quoted:\n%s", content)
	}
	if !strings.Contains(content, `WorkingDirectory="/work/100%%done"`) {
		t.Errorf("percent not escaped:\n%s", content)
	}
	// A double quote can't be represented safely; reject rather than emit garbage.
	if _, _, _, err := gatewayUnit("linux", "/home/tim", `/opt/m"emcode`, "/work"); err == nil {
		t.Error("a double-quote in a path must be rejected")
	}
}

func TestGatewayUnitUnsupported(t *testing.T) {
	if _, _, _, err := gatewayUnit("plan9", "/home/tim", "/bin/memcode", "/work"); err == nil {
		t.Error("an unsupported OS must return an error")
	}
}
