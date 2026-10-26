package claude

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

func validateSettingsBoundary(directory string) error {
	for _, name := range []string{"settings.json", "settings.local.json"} {
		data, err := os.ReadFile(filepath.Join(directory, name))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return errors.New("Claude settings could not be read")
		}
		var settings struct {
			Env          map[string]string `json:"env"`
			APIKeyHelper json.RawMessage   `json:"apiKeyHelper"`
		}
		if json.Unmarshal(data, &settings) != nil {
			return errors.New("Claude settings are malformed")
		}
		if len(settings.APIKeyHelper) > 0 && string(settings.APIKeyHelper) != "null" && string(settings.APIKeyHelper) != `""` {
			return errors.New("Claude settings contain an authentication override; repair the profile or project settings")
		}
		for key := range settings.Env {
			if protectedEnvironmentKey(key) {
				return errors.New("Claude settings contain a protected environment override; repair the profile or project settings")
			}
		}
	}
	return nil
}

func protectedEnvironmentKey(key string) bool {
	switch key {
	case "ANTHROPIC_MODEL", "ANTHROPIC_DEFAULT_OPUS_MODEL", "ANTHROPIC_DEFAULT_SONNET_MODEL", "ANTHROPIC_DEFAULT_HAIKU_MODEL", "ANTHROPIC_SMALL_FAST_MODEL", "CLAUDE_CODE_SUBAGENT_MODEL", "CLAUDE_CODE_EFFORT_LEVEL", "CLAUDE_CODE_PROJECT_DIR_NAME", "CLAUDECODE",
		"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL", "ANTHROPIC_CUSTOM_HEADERS",
		"ANTHROPIC_BEDROCK_BASE_URL", "ANTHROPIC_VERTEX_BASE_URL", "ANTHROPIC_FOUNDRY_BASE_URL",
		"CLAUDE_CODE_OAUTH_TOKEN", "CLAUDE_CODE_OAUTH_REFRESH_TOKEN", "CLAUDE_CODE_OAUTH_SCOPES",
		"CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_FOUNDRY",
		"CLAUDE_CONFIG_DIR", "VIBE_REMOTE_ACCOUNT_ID", "VIBE_REMOTE_SLOT_ID", "VIBE_REMOTE_ACTIVITY_RUN_ID":
		return true
	default:
		return false
	}
}

func filteredEnvironment(input []string) []string {
	result := make([]string, 0, len(input))
	for _, item := range input {
		key, _, found := strings.Cut(item, "=")
		if found && protectedEnvironmentKey(key) {
			continue
		}
		result = append(result, item)
	}
	return result
}

func (m *Manager) enforceWorkerEnvironment(w *worker, environment []string, runID string) []string {
	return mergeEnv(filteredEnvironment(environment), []string{
		"CLAUDE_CONFIG_DIR=" + w.account.ProfileDir,
		"VIBE_REMOTE_ACCOUNT_ID=" + w.account.ID,
		"VIBE_REMOTE_SLOT_ID=" + w.id,
		"VIBE_REMOTE_ACTIVITY_RUN_ID=" + runID,
	})
}
