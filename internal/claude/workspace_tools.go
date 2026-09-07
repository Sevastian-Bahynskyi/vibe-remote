package claude

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const computerUseServer = "computer-use"

func enableWorkspaceTools(profileDir, workspace string) error {
	statePath := filepath.Join(profileDir, ".claude.json")
	state := map[string]any{}
	if data, err := os.ReadFile(statePath); err == nil {
		if err := json.Unmarshal(data, &state); err != nil {
			return fmt.Errorf("parse Claude state: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read Claude state: %w", err)
	}

	projects, exists := state["projects"].(map[string]any)
	if !exists {
		if state["projects"] != nil {
			return errors.New("Claude projects state is malformed")
		}
		projects = map[string]any{}
	}
	workspace = filepath.Clean(workspace)
	project, exists := projects[workspace].(map[string]any)
	if !exists {
		if projects[workspace] != nil {
			return errors.New("Claude workspace state is malformed")
		}
		project = map[string]any{}
	}

	servers := make([]any, 0, 1)
	if configured := project["enabledMcpServers"]; configured != nil {
		var ok bool
		servers, ok = configured.([]any)
		if !ok {
			return errors.New("Claude enabled MCP server state is malformed")
		}
	}
	found := false
	for _, server := range servers {
		name, ok := server.(string)
		if !ok {
			return errors.New("Claude enabled MCP server entry is malformed")
		}
		if name == computerUseServer {
			found = true
		}
	}
	if !found {
		servers = append(servers, computerUseServer)
	}
	project["enabledMcpServers"] = servers
	project["hasTrustDialogAccepted"] = true
	projects[workspace] = project
	state["projects"] = projects

	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("encode Claude state: %w", err)
	}
	data = append(data, '\n')
	temporary := statePath + ".new"
	if err := os.WriteFile(temporary, data, 0o600); err != nil {
		return fmt.Errorf("write Claude state: %w", err)
	}
	if err := os.Rename(temporary, statePath); err != nil {
		return fmt.Errorf("activate Claude state: %w", err)
	}
	return nil
}
