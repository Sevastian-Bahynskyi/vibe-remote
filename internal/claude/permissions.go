package claude

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

type permissionRules struct {
	Allow []string `json:"allow,omitempty"`
	Ask   []string `json:"ask,omitempty"`
	Deny  []string `json:"deny,omitempty"`
}

type permissionSettings struct {
	Permissions                       permissionRules              `json:"permissions"`
	Env                               map[string]string            `json:"env,omitempty"`
	Hooks                             map[string][]json.RawMessage `json:"hooks,omitempty"`
	SkipDangerousModePermissionPrompt bool                         `json:"skipDangerousModePermissionPrompt"`
	EnableAllProjectMcpServers        bool                         `json:"enableAllProjectMcpServers"`
}

type sharedSessionSettings struct {
	Env   map[string]string            `json:"env,omitempty"`
	Hooks map[string][]json.RawMessage `json:"hooks,omitempty"`
}

func provisionPermissions(sourcePath, destinationPath string, additionalRules ...string) error {
	settings := permissionSettings{SkipDangerousModePermissionPrompt: true, EnableAllProjectMcpServers: true}
	if data, err := os.ReadFile(sourcePath); err == nil {
		var source struct {
			Permissions permissionRules `json:"permissions"`
		}
		if err := json.Unmarshal(data, &source); err != nil {
			return errors.New("shared Claude permissions are malformed")
		}
		settings.Permissions = source.Permissions
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read shared Claude permissions: %w", err)
	}
	sharedPath := filepath.Join(filepath.Dir(sourcePath), "settings.shared.json")
	if data, err := os.ReadFile(sharedPath); err == nil {
		var shared sharedSessionSettings
		if err := json.Unmarshal(data, &shared); err != nil {
			return errors.New("shared Claude session settings are malformed")
		}
		settings.Env = shared.Env
		settings.Hooks = shared.Hooks
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read shared Claude session settings: %w", err)
	}
	additionalRules = append(additionalRules, "Bash(gh issue edit *)")
	for _, additionalRule := range additionalRules {
		found := false
		for _, rule := range settings.Permissions.Allow {
			if rule == additionalRule {
				found = true
				break
			}
		}
		if !found {
			settings.Permissions.Allow = append(settings.Permissions.Allow, additionalRule)
		}
	}
	data, err := json.Marshal(settings)
	if err != nil {
		return fmt.Errorf("encode shared Claude permissions: %w", err)
	}
	file, err := os.CreateTemp(filepath.Dir(destinationPath), ".permissions-*")
	if err != nil {
		return fmt.Errorf("create shared Claude permissions: %w", err)
	}
	defer os.Remove(file.Name())
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return fmt.Errorf("write shared Claude permissions: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close shared Claude permissions: %w", err)
	}
	if err := os.Rename(file.Name(), destinationPath); err != nil {
		return fmt.Errorf("activate shared Claude permissions: %w", err)
	}
	return nil
}
