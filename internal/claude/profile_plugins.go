package claude

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

type marketplace struct {
	Name   string `json:"name"`
	Source string `json:"source"`
	Repo   string `json:"repo,omitempty"`
	URL    string `json:"url,omitempty"`
	Path   string `json:"path,omitempty"`
}

type installedPlugin struct {
	ID      string `json:"id"`
	Enabled bool   `json:"enabled"`
	Scope   string `json:"scope"`
}

var pluginIDPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*@[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)

func pluginCommand(ctx context.Context, runner CommandRunner, profile string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	data, err := runner.Output(ctx, Command{Args: args, Env: mergeEnv(filteredEnvironment(os.Environ()), []string{"CLAUDE_CONFIG_DIR=" + profile, "DISABLE_AUTOUPDATER=1"})})
	if err != nil {
		return nil, errors.New("required plugin registration failed; repair profile setup")
	}
	return data, nil
}

func listMarketplaces(ctx context.Context, runner CommandRunner, profile string) ([]marketplace, error) {
	data, err := pluginCommand(ctx, runner, profile, "plugin", "marketplace", "list", "--json")
	if err != nil {
		return nil, err
	}
	var entries []marketplace
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, errors.New("plugin marketplace inventory is invalid")
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	return entries, nil
}

func marketplaceSource(entry marketplace) ([]string, error) {
	if entry.Name == "" || strings.HasPrefix(entry.Name, "-") || strings.ContainsAny(entry.Name, "/\\\x00\r\n") {
		return nil, errors.New("marketplace name requires repair")
	}
	switch entry.Source {
	case "github":
		if !regexp.MustCompile(`^[a-zA-Z0-9_.-]+/[a-zA-Z0-9_.-]+$`).MatchString(entry.Repo) {
			return nil, errors.New("marketplace repository requires repair")
		}
		return []string{entry.Repo}, nil
	case "git", "url":
		parsed, err := url.Parse(entry.URL)
		if err != nil || parsed.User != nil || (parsed.Scheme != "https" && parsed.Scheme != "ssh") {
			return nil, errors.New("marketplace URL requires repair")
		}
		return []string{entry.URL}, nil
	case "directory", "file":
		if !filepath.IsAbs(entry.Path) {
			return nil, errors.New("marketplace path requires repair")
		}
		return []string{entry.Path}, nil
	case "claudeai":
		return []string{"--claudeai", entry.Name}, nil
	default:
		return nil, errors.New("marketplace source needs supported registration before this profile can be ready")
	}
}

func reconcilePlugins(ctx context.Context, source, profile string, desired map[string]bool, manifest *baselineManifest, runner CommandRunner) ([]marketplace, error) {
	if runner == nil {
		return nil, errors.New("plugin tooling is unavailable")
	}
	wanted, err := listMarketplaces(ctx, runner, source)
	if err != nil {
		return nil, err
	}
	current, err := listMarketplaces(ctx, runner, profile)
	if err != nil {
		return nil, err
	}
	byName := map[string]marketplace{}
	for _, entry := range current {
		byName[entry.Name] = entry
	}
	names := map[string]bool{}
	for _, entry := range wanted {
		names[entry.Name] = true
		args, err := marketplaceSource(entry)
		if err != nil {
			return nil, err
		}
		if existing, ok := byName[entry.Name]; ok {
			if existing == entry {
				continue
			}
			if err := writePrivateJSON(filepath.Join(profile, ".vibe-baseline", "backups", "marketplace-"+contentHash([]byte(entry.Name))[:16]+".json"), existing); err != nil {
				return nil, err
			}
			if _, err := pluginCommand(ctx, runner, profile, "plugin", "marketplace", "remove", entry.Name, "--scope", "user", "--json"); err != nil {
				return nil, err
			}
		}
		command := append([]string{"plugin", "marketplace", "add"}, args...)
		command = append(command, "--scope", "user", "--json")
		if _, err := pluginCommand(ctx, runner, profile, command...); err != nil {
			return nil, err
		}
		manifest.Marketplaces[entry.Name] = true
		if err := writePrivateJSON(filepath.Join(profile, ".vibe-baseline", "manifest.json"), manifest); err != nil {
			return nil, err
		}
	}
	data, err := pluginCommand(ctx, runner, profile, "plugin", "list", "--json")
	if err != nil {
		return nil, err
	}
	var installed []installedPlugin
	if err := json.Unmarshal(data, &installed); err != nil {
		return nil, errors.New("plugin inventory is invalid")
	}
	plugins := map[string]installedPlugin{}
	for _, entry := range installed {
		if entry.Scope == "user" {
			plugins[entry.ID] = entry
		}
	}
	ids := []string{}
	for id, enabled := range desired {
		if !pluginIDPattern.MatchString(id) {
			return nil, errors.New("plugin identifier needs repair")
		}
		if enabled {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		entry, exists := plugins[id]
		if !exists {
			if _, err := pluginCommand(ctx, runner, profile, "plugin", "install", id, "--scope", "user", "--json"); err != nil {
				return nil, err
			}
			manifest.Plugins[id] = true
		} else if !entry.Enabled {
			if _, err := pluginCommand(ctx, runner, profile, "plugin", "enable", id, "--scope", "user", "--json"); err != nil {
				return nil, err
			}
			manifest.Plugins[id] = true
		}
		if err := writePrivateJSON(filepath.Join(profile, ".vibe-baseline", "manifest.json"), manifest); err != nil {
			return nil, err
		}
	}
	for id := range manifest.Plugins {
		if desired[id] {
			continue
		}
		if _, exists := plugins[id]; exists {
			if _, err := pluginCommand(ctx, runner, profile, "plugin", "uninstall", id, "--scope", "user", "--json"); err != nil {
				return nil, err
			}
		}
		delete(manifest.Plugins, id)
		delete(plugins, id)
	}
	for name := range manifest.Marketplaces {
		if names[name] {
			continue
		}
		used := false
		for id := range plugins {
			if strings.HasSuffix(id, "@"+name) {
				used = true
				break
			}
		}
		if used {
			continue
		}
		if _, err := pluginCommand(ctx, runner, profile, "plugin", "marketplace", "remove", name, "--scope", "user", "--json"); err != nil {
			return nil, err
		}
		delete(manifest.Marketplaces, name)
	}
	verifiedData, err := pluginCommand(ctx, runner, profile, "plugin", "list", "--json")
	if err != nil {
		return nil, err
	}
	var verified []installedPlugin
	if err := json.Unmarshal(verifiedData, &verified); err != nil {
		return nil, errors.New("plugin readiness inventory is invalid")
	}
	ready := map[string]bool{}
	for _, entry := range verified {
		if entry.Scope == "user" && entry.Enabled {
			ready[entry.ID] = true
		}
	}
	for id, required := range desired {
		if required && !ready[id] {
			return nil, errors.New("a required plugin is unavailable; repair setup before starting")
		}
	}

	return wanted, nil
}
