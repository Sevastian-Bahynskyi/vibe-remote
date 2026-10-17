package claude

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

func provisionSharedMCP(sourcePath, destinationPath string) error {
	config := struct {
		Servers map[string]json.RawMessage `json:"mcpServers"`
	}{Servers: map[string]json.RawMessage{}}
	if data, err := os.ReadFile(sourcePath); err == nil {
		if err := json.Unmarshal(data, &config); err != nil {
			return errors.New("shared Claude MCP configuration is malformed")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read shared Claude MCP configuration: %w", err)
	}
	if config.Servers == nil {
		config.Servers = map[string]json.RawMessage{}
	}
	for _, server := range config.Servers {
		var definition map[string]json.RawMessage
		if err := json.Unmarshal(server, &definition); err != nil || definition == nil {
			return errors.New("shared Claude MCP server is malformed")
		}
	}
	data, err := json.Marshal(config)
	if err != nil {
		return fmt.Errorf("encode shared Claude MCP configuration: %w", err)
	}
	file, err := os.CreateTemp(filepath.Dir(destinationPath), ".mcp-*")
	if err != nil {
		return fmt.Errorf("create shared Claude MCP configuration: %w", err)
	}
	defer os.Remove(file.Name())
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return fmt.Errorf("write shared Claude MCP configuration: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close shared Claude MCP configuration: %w", err)
	}
	if err := os.Rename(file.Name(), destinationPath); err != nil {
		return fmt.Errorf("activate shared Claude MCP configuration: %w", err)
	}
	return nil
}
