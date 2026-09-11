package install

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/Sevastian-Bahynskyi/vibe-remote/internal/paths"
	systemstate "github.com/Sevastian-Bahynskyi/vibe-remote/internal/system"
)

type Record struct {
	InstalledAt        time.Time `json:"installedAt"`
	PreviousNotifyLine string    `json:"previousNotifyLine,omitempty"`
	CodexHooksExisted  bool      `json:"codexHooksExisted"`
}

func Install(layout paths.Layout, executable string) error {
	if runtime.GOOS != "darwin" {
		return errors.New("Vibe Remote installation currently requires macOS")
	}
	if err := paths.Ensure(layout); err != nil {
		return err
	}
	if err := installManagedFiles(layout, executable); err != nil {
		return err
	}
	if err := writeLaunchAgent(layout); err != nil {
		return err
	}
	if err := ensureFunnelDisabled(); err != nil {
		return err
	}
	uid := fmt.Sprintf("gui/%d", os.Getuid())
	_ = exec.Command("launchctl", "bootout", uid, layout.LaunchAgent).Run()
	if output, err := exec.Command("launchctl", "bootstrap", uid, layout.LaunchAgent).CombinedOutput(); err != nil {
		return fmt.Errorf("start launch agent: %w: %s", err, strings.TrimSpace(string(output)))
	}
	if output, err := exec.Command("tailscale", "serve", "--bg", "--yes", "--set-path", paths.DashboardPath, "http://"+paths.ListenAddr).CombinedOutput(); err != nil {
		return fmt.Errorf("configure Tailscale Serve: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func installManagedFiles(layout paths.Layout, executable string) error {
	preserveVerification := systemstate.CodexHooksVerified(layout.CodexHooks, layout.CodexHookVerified, layout.Binary)
	if err := copyExecutable(executable, layout.Binary); err != nil {
		return err
	}
	if err := InstallHooks(layout); err != nil {
		return err
	}
	if !preserveVerification {
		return nil
	}
	fingerprint, err := systemstate.HookFingerprint(layout.CodexHooks, layout.Binary)
	if err != nil {
		return fmt.Errorf("refresh Codex hook verification: %w", err)
	}
	return writeFileAtomic(layout.CodexHookVerified, []byte(fingerprint+"\n"), 0o600)
}

func ensureFunnelDisabled() error {
	output, err := exec.Command("tailscale", "funnel", "status").CombinedOutput()
	if err != nil {
		return fmt.Errorf("verify Tailscale Funnel is disabled: %w: %s", err, strings.TrimSpace(string(output)))
	}
	if funnelActive(string(output)) {
		return errors.New("Tailscale Funnel is active; disable it before installing Vibe Remote")
	}
	return nil
}

func funnelActive(status string) bool {
	return strings.Contains(strings.ToLower(status), "available on the internet")
}

func InstallHooks(layout paths.Layout) error {
	record := Record{}
	if data, err := os.ReadFile(layout.InstallRecord); err == nil {
		_ = json.Unmarshal(data, &record)
	}
	updated, err := installCodexIntegration(layout)
	if err != nil {
		return err
	}
	if record.InstalledAt.IsZero() {
		record.InstalledAt = updated.InstalledAt
	}
	if record.PreviousNotifyLine == "" {
		record.PreviousNotifyLine = updated.PreviousNotifyLine
	}
	if updated.CodexHooksExisted {
		record.CodexHooksExisted = true
	}
	return writeJSONAtomic(layout.InstallRecord, record, 0o600)
}

func InstallClaudeHooks(profileDir, binary string) error {
	clean, err := secureProfilePath(profileDir)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(clean, 0o700); err != nil {
		return fmt.Errorf("create Claude profile: %w", err)
	}
	settingsPath := filepath.Join(clean, "settings.json")
	settings := map[string]any{}
	if data, readErr := os.ReadFile(settingsPath); readErr == nil {
		if err := json.Unmarshal(data, &settings); err != nil {
			return fmt.Errorf("parse Claude settings: %w", err)
		}
	} else if !errors.Is(readErr, os.ErrNotExist) {
		return fmt.Errorf("read Claude settings: %w", readErr)
	}
	hooks, _ := settings["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
	}
	for _, event := range []string{"SessionStart", "UserPromptSubmit", "Stop", "StopFailure"} {
		updated, err := appendHookGroup(hooks[event], hookCommand(binary, "claude", event), event == "UserPromptSubmit")
		if err != nil {
			return fmt.Errorf("repair Claude %s hook: %w", event, err)
		}
		hooks[event] = updated
	}
	settings["hooks"] = hooks
	return writeJSONAtomic(settingsPath, settings, 0o600)
}

func copyExecutable(source, destination string) error {
	in, err := os.Open(source)
	if err != nil {
		return fmt.Errorf("open executable: %w", err)
	}
	defer in.Close()
	temporary := destination + ".new"
	out, err := os.OpenFile(temporary, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return fmt.Errorf("create installed executable: %w", err)
	}
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil {
		return fmt.Errorf("copy executable: %w", copyErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close installed executable: %w", closeErr)
	}
	if err := os.Rename(temporary, destination); err != nil {
		return fmt.Errorf("activate installed executable: %w", err)
	}
	return nil
}

func installCodexIntegration(layout paths.Layout) (Record, error) {
	record := Record{InstalledAt: time.Now().UTC()}
	if _, err := os.Stat(layout.CodexHooks); err == nil {
		record.CodexHooksExisted = true
	}
	if err := mergeCodexHooks(layout.CodexHooks, layout.Binary); err != nil {
		return record, err
	}
	previous, err := replaceNotify(layout.CodexConfig, layout.Binary)
	if err != nil {
		return record, err
	}
	record.PreviousNotifyLine = previous
	return record, nil
}

func mergeCodexHooks(path, binary string) error {
	document := map[string]any{}
	if data, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(data, &document); err != nil {
			return fmt.Errorf("parse Codex hooks: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read Codex hooks: %w", err)
	}
	hooks, _ := document["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
	}
	for _, event := range []string{"UserPromptSubmit", "Stop", "Interrupt"} {
		updated, err := appendHookGroup(hooks[event], hookCommand(binary, "codex", event), event == "UserPromptSubmit")
		if err != nil {
			return fmt.Errorf("repair Codex %s hook: %w", event, err)
		}
		hooks[event] = updated
	}
	document["hooks"] = hooks
	return writeJSONAtomic(path, document, 0o600)
}

func appendHookGroup(existing any, command string, context bool) ([]any, error) {
	groups := []any{}
	if existing != nil {
		var ok bool
		groups, ok = existing.([]any)
		if !ok {
			return nil, errors.New("existing hook collection is malformed")
		}
	}
	handler := map[string]any{"type": "command", "command": command, "timeout": 3}
	if context {
		handler["additionalContextLimit"] = 12000
	}
	found := false
	result := make([]any, 0, len(groups)+1)
	for _, item := range groups {
		group, ok := item.(map[string]any)
		if !ok {
			return nil, errors.New("existing hook group is malformed")
		}
		handlers, ok := group["hooks"].([]any)
		if !ok {
			return nil, errors.New("existing hook handlers are malformed")
		}
		kept := make([]any, 0, len(handlers))
		for _, handlerValue := range handlers {
			existingHandler, ok := handlerValue.(map[string]any)
			if !ok {
				return nil, errors.New("existing hook handler is malformed")
			}
			if existingHandler["command"] == command {
				if !found {
					kept = append(kept, handler)
					found = true
				}
				continue
			}
			kept = append(kept, existingHandler)
		}
		group["hooks"] = kept
		result = append(result, group)
	}
	if !found {
		result = append(result, map[string]any{"hooks": []any{handler}})
	}
	return result, nil
}

func hookCommand(binary, provider, event string) string {
	return shellQuote(binary) + " hook " + provider + " " + event
}

func replaceNotify(configPath, binary string) (string, error) {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return "", fmt.Errorf("read Codex config: %w", err)
	}
	lines := strings.Split(string(data), "\n")
	previous := ""
	replacement := fmt.Sprintf("notify = [%q, %q, %q]", binary, "notify", "codex")
	for index, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "notify =") {
			if strings.Contains(line, fmt.Sprintf("%q", binary)) && strings.Contains(line, `"notify"`) && strings.Contains(line, `"codex"`) {
				return "", nil
			}
			previous = line
			encoded := base64.RawURLEncoding.EncodeToString([]byte(line))
			replacement = fmt.Sprintf("notify = [%q, %q, %q, %q]", binary, "notify", "codex", encoded)
			lines[index] = replacement
			return previous, writeFileAtomic(configPath, []byte(strings.Join(lines, "\n")), 0o600)
		}
	}
	lines = append([]string{replacement}, lines...)
	return previous, writeFileAtomic(configPath, []byte(strings.Join(lines, "\n")), 0o600)
}

func writeLaunchAgent(layout paths.Layout) error {
	content := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>Label</key><string>%s</string>
<key>ProgramArguments</key><array><string>%s</string><string>serve</string></array>
<key>RunAtLoad</key><true/>
<key>KeepAlive</key><true/>
<key>ProcessType</key><string>Interactive</string>
<key>StandardOutPath</key><string>%s</string>
<key>StandardErrorPath</key><string>%s</string>
<key>EnvironmentVariables</key><dict><key>PATH</key><string>/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin</string></dict>
</dict></plist>
`, paths.ServiceLabel, html.EscapeString(layout.Binary), html.EscapeString(filepath.Join(layout.Logs, "service.log")), html.EscapeString(filepath.Join(layout.Logs, "service-error.log")))
	return writeFileAtomic(layout.LaunchAgent, []byte(content), 0o600)
}

func writeJSONAtomic(path string, value any, mode os.FileMode) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("encode %s: %w", path, err)
	}
	return writeFileAtomic(path, append(data, '\n'), mode)
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create parent for %s: %w", path, err)
	}
	temporary := path + ".new"
	if err := os.WriteFile(temporary, data, mode); err != nil {
		return fmt.Errorf("write %s: %w", temporary, err)
	}
	if err := os.Chmod(temporary, mode); err != nil {
		return fmt.Errorf("secure %s: %w", temporary, err)
	}
	if err := os.Rename(temporary, path); err != nil {
		return fmt.Errorf("activate %s: %w", path, err)
	}
	return nil
}

func secureProfilePath(profileDir string) (string, error) {
	clean := filepath.Clean(profileDir)
	if !filepath.IsAbs(clean) || clean == string(filepath.Separator) {
		return "", errors.New("Claude profile path must be an absolute non-root path")
	}
	return clean, nil
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func DecodeForwardedNotify(encoded string) ([]string, error) {
	if encoded == "" {
		return nil, nil
	}
	lineBytes, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("decode prior notifier: %w", err)
	}
	line := strings.TrimSpace(string(lineBytes))
	start := strings.Index(line, "[")
	end := strings.LastIndex(line, "]")
	if start < 0 || end <= start {
		return nil, errors.New("invalid prior notifier record")
	}
	var command []string
	if err := json.Unmarshal([]byte(line[start:end+1]), &command); err != nil {
		return nil, fmt.Errorf("parse prior notifier: %w", err)
	}
	return command, nil
}

func ScanNotifyLine(reader io.Reader) string {
	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(strings.TrimSpace(line), "notify =") {
			return line
		}
	}
	return ""
}
