package claude

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSharedMCPRefreshesEveryProfileWithoutCopyingAccountState(t *testing.T) {
	source := filepath.Join(t.TempDir(), ".claude.json")
	profiles := []string{t.TempDir(), t.TempDir()}
	for _, servers := range []string{
		`{"rive":{"type":"http","url":"http://localhost:1979/mcp"},"local":{"command":"local-mcp","env":{"TOKEN":"${MCP_TOKEN}"}}}`,
		`{"replacement":{"type":"http","url":"http://localhost:1980/mcp"}}`,
		`{}`,
	} {
		if err := os.WriteFile(source, []byte(`{"oauthAccount":{"emailAddress":"private@example.com"},"env":{"SECRET":"private"},"projects":{"/private":{}},"mcpServers":`+servers+`}`), 0o600); err != nil {
			t.Fatal(err)
		}
		for _, profile := range profiles {
			destination := filepath.Join(profile, "session.mcp.json")
			if err := provisionSharedMCP(source, destination); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(destination)
			if err != nil {
				t.Fatal(err)
			}
			var got, want map[string]json.RawMessage
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(servers), &want); err != nil {
				t.Fatal(err)
			}
			var inherited map[string]json.RawMessage
			if err := json.Unmarshal(got["mcpServers"], &inherited); err != nil {
				t.Fatal(err)
			}
			if len(got) != 1 || len(inherited) != len(want) || strings.Contains(string(data), "private") {
				t.Fatal("MCP inheritance leaked account state or retained revoked servers")
			}
			for name, server := range want {
				if string(inherited[name]) != string(server) {
					t.Fatal("MCP definition changed during inheritance")
				}
			}
			info, err := os.Stat(destination)
			if err != nil || info.Mode().Perm() != 0o600 {
				t.Fatal("shared MCP configuration must remain private")
			}
		}
	}
}

func TestSharedMCPMalformedConfigurationPreservesPreviousSnapshot(t *testing.T) {
	dir := t.TempDir()
	source, destination := filepath.Join(dir, "source.json"), filepath.Join(dir, "session.json")
	for _, malformed := range []string{`{`, `{"mcpServers":[]}`, `{"mcpServers":{"rive":null}}`, `{"mcpServers":{"rive":"invalid"}}`} {
		if err := os.WriteFile(destination, []byte("previous"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(source, []byte(malformed), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := provisionSharedMCP(source, destination); err == nil {
			t.Fatal("malformed MCP configuration was accepted")
		}
		if data, err := os.ReadFile(destination); err != nil || string(data) != "previous" {
			t.Fatal("malformed configuration replaced the working snapshot")
		}
	}
	if err := os.Remove(source); err != nil {
		t.Fatal(err)
	}
	if err := provisionSharedMCP(source, destination); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(destination); err != nil || string(data) != `{"mcpServers":{}}` {
		t.Fatal("missing global MCP configuration was not handled")
	}
}
