package paths

import (
	"fmt"
	"os"
	"path/filepath"
)

const (
	ServiceLabel  = "com.seva.vibe-remote"
	ListenAddr    = "127.0.0.1:47173"
	DashboardPath = "/vibe-remote"
)

type Layout struct {
	Root              string
	Binary            string
	Database          string
	Profiles          string
	Logs              string
	LaunchAgent       string
	CodexHooks        string
	CodexConfig       string
	InstallRecord     string
	CodexHookVerified string
}

func Resolve() (Layout, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Layout{}, fmt.Errorf("resolve home directory: %w", err)
	}
	root := filepath.Join(home, "Library", "Application Support", "Vibe Remote")
	return Layout{
		Root:              root,
		Binary:            filepath.Join(root, "bin", "vibe-remote"),
		Database:          filepath.Join(root, "state.sqlite3"),
		Profiles:          filepath.Join(root, "claude-profiles"),
		Logs:              filepath.Join(root, "logs"),
		LaunchAgent:       filepath.Join(home, "Library", "LaunchAgents", ServiceLabel+".plist"),
		CodexHooks:        filepath.Join(home, ".codex", "hooks.json"),
		CodexConfig:       filepath.Join(home, ".codex", "config.toml"),
		InstallRecord:     filepath.Join(root, "install-record.json"),
		CodexHookVerified: filepath.Join(root, "codex-hook-verified"),
	}, nil
}

func Ensure(layout Layout) error {
	for _, dir := range []string{layout.Root, filepath.Dir(layout.Binary), layout.Profiles, layout.Logs} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
		if err := os.Chmod(dir, 0o700); err != nil {
			return fmt.Errorf("secure %s: %w", dir, err)
		}
	}
	return nil
}
