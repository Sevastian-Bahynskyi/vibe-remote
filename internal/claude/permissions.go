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
	extra map[string]json.RawMessage
}

func (p *permissionRules) UnmarshalJSON(data []byte) error {
	type fields permissionRules
	var parsed fields
	if err := json.Unmarshal(data, &parsed); err != nil {
		return err
	}
	var extra map[string]json.RawMessage
	if err := json.Unmarshal(data, &extra); err != nil {
		return err
	}
	for _, key := range []string{"allow", "ask", "deny", "defaultMode"} {
		delete(extra, key)
	}
	*p = permissionRules(parsed)
	p.extra = extra
	return nil
}

func (p permissionRules) MarshalJSON() ([]byte, error) {
	type fields permissionRules
	data, err := json.Marshal(fields(p))
	if err != nil || len(p.extra) == 0 {
		return data, err
	}
	var result map[string]json.RawMessage
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, err
	}
	for key, value := range p.extra {
		result[key] = value
	}
	return json.Marshal(result)
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
		for key := range shared.Env {
			if protectedEnvironmentKey(key) {
				return errors.New("shared Claude session settings contain protected environment overrides")
			}
		}
		settings.Env = shared.Env
		settings.Hooks = shared.Hooks
		for event, groups := range settings.Hooks {
			seen := map[string]bool{}
			kept := []json.RawMessage{}
			for _, group := range groups {
				var value map[string]json.RawMessage
				if json.Unmarshal(group, &value) != nil {
					return errors.New("shared hook group is malformed")
				}
				var handlers []json.RawMessage
				if json.Unmarshal(value["hooks"], &handlers) != nil {
					return errors.New("shared hook handlers are malformed")
				}
				delete(value, "hooks")
				scope, _ := json.Marshal(value)
				unique := []json.RawMessage{}
				for _, handler := range handlers {
					var fields map[string]json.RawMessage
					if json.Unmarshal(handler, &fields) != nil {
						return errors.New("shared hook handler is malformed")
					}
					canonical, _ := json.Marshal(fields)
					key := string(scope) + string(canonical)
					if !seen[key] {
						seen[key] = true
						unique = append(unique, handler)
					}
				}
				if len(unique) > 0 {
					value["hooks"], _ = json.Marshal(unique)
					encoded, _ := json.Marshal(value)
					kept = append(kept, encoded)
				}
			}
			settings.Hooks[event] = kept
		}
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
