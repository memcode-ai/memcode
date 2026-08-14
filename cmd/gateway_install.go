package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/spf13/cobra"

	"github.com/memcode-ai/memcode/internal/atomicfile"
)

// gatewayInstallCmd installs the gateway as a managed background service so it
// survives logout/reboot instead of living in a foreground terminal. It writes an
// OS-native unit (launchd on macOS, systemd --user on Linux) that runs
// `memcode gateway` in the current project, then prints how to start it.
var gatewayInstallCmd = &cobra.Command{
	Use:   "install",
	Short: "Install the gateway as a background service (launchd/systemd)",
	RunE: func(cmd *cobra.Command, args []string) error {
		bin, err := os.Executable()
		if err != nil {
			return fmt.Errorf("locating memcode binary: %w", err)
		}
		workDir, err := os.Getwd()
		if err != nil {
			return err
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		path, content, start, err := gatewayUnit(runtime.GOOS, home, bin, workDir)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		if err := atomicfile.WriteFile(path, []byte(content), 0o644); err != nil {
			return err
		}
		cmd.Printf("Installed gateway service for %s\n", workDir)
		cmd.Printf("Unit: %s\n", path)
		cmd.Printf("Start it with:\n  %s\n", start)
		return nil
	},
}

// gatewayUninstallCmd removes the service unit. It doesn't stop a running service
// (the user unloads it with the printed command); it just deletes the unit.
var gatewayUninstallCmd = &cobra.Command{
	Use:   "uninstall",
	Short: "Remove the installed gateway service unit",
	RunE: func(cmd *cobra.Command, args []string) error {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		path, _, _, err := gatewayUnit(runtime.GOOS, home, "", "")
		if err != nil {
			return err
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		cmd.Printf("Removed %s\n", path)
		switch runtime.GOOS {
		case "darwin":
			cmd.Printf("Stop it (if running) with:\n  launchctl unload %s\n", path)
		case "linux":
			cmd.Printf("Stop it (if running) with:\n  systemctl --user disable --now memcode-gateway\n")
		}
		return nil
	},
}

// hasControlChars rejects paths carrying newlines/NULs, which could inject extra
// unit directives (a systemd ExecStart= line, a plist element) via the binary or
// working-directory path.
func hasControlChars(s string) bool { return strings.ContainsAny(s, "\n\r\x00") }

// xmlEscape escapes a value for safe interpolation into the plist XML.
var xmlEscape = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&apos;").Replace

// gatewayUnit builds the service unit for goos: its file path, contents, and the
// command to start it. bin/workDir may be empty when only the path is needed
// (uninstall). Returns an error for an unsupported OS or a path with control
// characters.
func gatewayUnit(goos, home, bin, workDir string) (path, content, start string, err error) {
	if hasControlChars(bin) || hasControlChars(workDir) {
		return "", "", "", fmt.Errorf("binary or working-directory path contains control characters; refusing to write a service unit")
	}
	switch goos {
	case "darwin":
		bin, workDir := xmlEscape(bin), xmlEscape(workDir)
		path = filepath.Join(home, "Library", "LaunchAgents", "ai.memcode.gateway.plist")
		content = fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key><string>ai.memcode.gateway</string>
	<key>ProgramArguments</key>
	<array>
		<string>%s</string>
		<string>gateway</string>
	</array>
	<key>WorkingDirectory</key><string>%s</string>
	<key>RunAtLoad</key><true/>
	<key>KeepAlive</key><true/>
	<key>StandardOutPath</key><string>%s/.memcode/gateway.log</string>
	<key>StandardErrorPath</key><string>%s/.memcode/gateway.log</string>
</dict>
</plist>
`, bin, workDir, workDir, workDir)
		start = "launchctl load " + path
		return path, content, start, nil
	case "linux":
		path = filepath.Join(home, ".config", "systemd", "user", "memcode-gateway.service")
		content = fmt.Sprintf(`[Unit]
Description=memcode gateway
After=network-online.target

[Service]
ExecStart=%s gateway
WorkingDirectory=%s
Restart=always
RestartSec=5

[Install]
WantedBy=default.target
`, bin, workDir)
		start = "systemctl --user daemon-reload && systemctl --user enable --now memcode-gateway"
		return path, content, start, nil
	default:
		return "", "", "", fmt.Errorf("gateway install is not supported on %s (run `memcode gateway` directly)", goos)
	}
}

func init() {
	gatewayCmd.AddCommand(gatewayInstallCmd)
	gatewayCmd.AddCommand(gatewayUninstallCmd)
}
