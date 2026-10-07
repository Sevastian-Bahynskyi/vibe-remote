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
	Permissions permissionRules `json:"permissions"`
}

func provisionPermissions(sourcePath, destinationPath string) error {
	settings := permissionSettings{}
	if data, err := os.ReadFile(sourcePath); err == nil {
		if err := json.Unmarshal(data, &settings); err != nil {
			return errors.New("shared Claude permissions are malformed")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read shared Claude permissions: %w", err)
	}
	const issueEditRule = "Bash(gh issue edit *)"
	found := false
	for _, rule := range settings.Permissions.Allow {
		if rule == issueEditRule {
			found = true
			break
		}
	}
	if !found {
		settings.Permissions.Allow = append(settings.Permissions.Allow, issueEditRule)
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
