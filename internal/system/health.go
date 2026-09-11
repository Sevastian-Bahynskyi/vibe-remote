package system

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type Health struct {
	TailscaleOnline     bool   `json:"tailscaleOnline"`
	TailscaleURL        string `json:"tailscaleUrl,omitempty"`
	TailnetOnly         bool   `json:"tailnetOnly"`
	ServeReady          bool   `json:"serveReady"`
	FunnelOff           bool   `json:"funnelOff"`
	OnACPower           bool   `json:"onAcPower"`
	CodexHooks          bool   `json:"codexHooks"`
	CodexHooksInstalled bool   `json:"codexHooksInstalled"`
	ClaudeBinary        bool   `json:"claudeBinary"`
	CodexBinary         bool   `json:"codexBinary"`
	ClaudeDesktop       bool   `json:"claudeDesktop"`
}

func Inspect(ctx context.Context, codexHooksPath, codexHookVerified, binary string) Health {
	installed := codexHooksInstalled(codexHooksPath, binary)
	verified := CodexHooksVerified(codexHooksPath, codexHookVerified, binary)
	health := Health{
		OnACPower:           onACPower(ctx),
		CodexHooks:          installed && verified,
		CodexHooksInstalled: installed,
		ClaudeBinary:        commandExists("claude"),
		CodexBinary:         commandExists("codex"),
		ClaudeDesktop:       ClaudeDesktopApp() != "",
	}
	health.TailscaleOnline, health.TailscaleURL = tailscaleState(ctx)
	health.ServeReady, health.FunnelOff = tailscaleExposure(ctx)
	health.TailnetOnly = health.TailscaleOnline && health.ServeReady && health.FunnelOff
	return health
}

// ClaudeDesktopApp returns the installed Claude Desktop bundle path, or an
// empty string when the app is absent. A Remote Control link can only be
// handed to the app on this Mac when the bundle is present.
func ClaudeDesktopApp() string {
	candidates := []string{"/Applications/Claude.app"}
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates, filepath.Join(home, "Applications", "Claude.app"))
	}
	for _, candidate := range candidates {
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			return candidate
		}
	}
	return ""
}

func CodexHooksVerified(hooksPath, markerPath, binary string) bool {
	fingerprint, err := HookFingerprint(hooksPath, binary)
	if err != nil {
		return false
	}
	marker, err := os.ReadFile(markerPath)
	return err == nil && strings.TrimSpace(string(marker)) == fingerprint
}

func HookFingerprint(hooksPath, binary string) (string, error) {
	hooks, err := os.ReadFile(hooksPath)
	if err != nil {
		return "", err
	}
	executable, err := os.ReadFile(binary)
	if err != nil {
		return "", err
	}
	digest := sha256.New()
	_, _ = digest.Write(hooks)
	_, _ = digest.Write([]byte{0})
	_, _ = digest.Write(executable)
	return fmt.Sprintf("%x", digest.Sum(nil)), nil
}

func codexHooksInstalled(path, binary string) bool {
	data, err := os.ReadFile(path)
	return err == nil && strings.Contains(string(data), binary+"' hook codex")
}

func tailscaleState(ctx context.Context) (bool, string) {
	output, err := exec.CommandContext(ctx, "tailscale", "status", "--json").Output()
	if err != nil {
		return false, ""
	}
	var payload struct {
		BackendState string `json:"BackendState"`
		Self         struct {
			DNSName string `json:"DNSName"`
			Online  bool   `json:"Online"`
		} `json:"Self"`
	}
	if json.Unmarshal(output, &payload) != nil {
		return false, ""
	}
	host := strings.TrimSuffix(payload.Self.DNSName, ".")
	url := ""
	if host != "" {
		url = "https://" + host + "/vibe-remote/"
	}
	return payload.BackendState == "Running" && payload.Self.Online, url
}

func tailscaleExposure(ctx context.Context) (bool, bool) {
	target := "http://" + "127.0.0.1:47173"
	serveOutput, serveErr := exec.CommandContext(ctx, "tailscale", "serve", "status").Output()
	funnelOutput, funnelErr := exec.CommandContext(ctx, "tailscale", "funnel", "status").Output()
	return parseTailscaleExposure(string(serveOutput), serveErr, string(funnelOutput), funnelErr, target)
}

func parseTailscaleExposure(serveText string, serveErr error, funnelText string, funnelErr error, target string) (bool, bool) {
	serveReady := serveErr == nil &&
		strings.Contains(serveText, "(tailnet only)") &&
		strings.Contains(serveText, target) &&
		strings.Contains(serveText, "/vibe-remote")
	funnelOff := funnelErr == nil && strings.Contains(funnelText, "(tailnet only)")
	return serveReady, funnelOff
}

func onACPower(ctx context.Context) bool {
	output, err := exec.CommandContext(ctx, "/usr/bin/pmset", "-g", "batt").Output()
	return err == nil && strings.Contains(string(output), "AC Power")
}

func commandExists(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}
