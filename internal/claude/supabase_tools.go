package claude

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

type supabaseServer struct {
	Type    string            `json:"type"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers"`
}

type workspaceSupabaseSettings struct {
	Workspaces   map[string]map[string]supabaseServer `json:"workspaces"`
	Repositories map[string]string                    `json:"repositories,omitempty"`
}

var supabaseName = regexp.MustCompile(`^supabase(?:-[a-z0-9-]+)?$`)
var supabaseProject = regexp.MustCompile(`^[a-z0-9]{20}$`)
var supabaseAuthorization = regexp.MustCompile(`^Bearer \$\{([A-Z][A-Z0-9_]*)\}$`)

func loadWorkspaceSupabase(path, workspace string) (map[string]supabaseServer, error) {
	servers, _, err := resolveWorkspaceSupabase(path, workspace)
	return servers, err
}

func resolveWorkspaceSupabase(path, workspace string) (map[string]supabaseServer, string, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, "", nil
	}
	if err != nil {
		return nil, "", errors.New("could not read workspace Supabase settings")
	}
	var settings workspaceSupabaseSettings
	if json.Unmarshal(data, &settings) != nil || settings.Workspaces == nil {
		return nil, "", errors.New("workspace Supabase settings are malformed")
	}
	credentialWorkspace := ""
	lookup := func(path string) map[string]supabaseServer {
		if servers := settings.Workspaces[path]; servers != nil {
			credentialWorkspace = path
			return servers
		}
		canonical, err := filepath.EvalSymlinks(path)
		if err != nil {
			return nil
		}
		for configured, servers := range settings.Workspaces {
			if resolved, err := filepath.EvalSymlinks(configured); err == nil && resolved == canonical {
				credentialWorkspace = configured
				return servers
			}
		}
		return nil
	}
	servers := lookup(workspace)
	if servers == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		command := exec.CommandContext(ctx, "git", "rev-parse", "--path-format=absolute", "--git-common-dir")
		command.Dir = workspace
		if output, err := command.Output(); err == nil {
			common := strings.TrimSpace(string(output))
			if filepath.Base(common) == ".git" {
				servers = lookup(filepath.Dir(common))
			}
		}
	}
	if servers == nil && len(settings.Repositories) > 0 {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		command := exec.CommandContext(ctx, "git", "config", "--get-regexp", `^remote\..*\.url$`)
		command.Dir = workspace
		if output, err := command.Output(); err == nil {
			matched := ""
			for _, line := range strings.Split(string(output), "\n") {
				_, remote, ok := strings.Cut(line, " ")
				if !ok {
					continue
				}
				configured := settings.Repositories[supabaseRepositoryIdentity(remote)]
				if configured == "" {
					continue
				}
				if matched != "" && matched != configured {
					return nil, "", errors.New("workspace matches conflicting shared Supabase repositories")
				}
				matched = configured
			}
			if matched != "" {
				servers = lookup(matched)
				if servers == nil {
					return nil, "", errors.New("shared Supabase repository references an unconfigured workspace")
				}
			}
		}
	}
	for name, server := range servers {
		parsed, err := url.Parse(server.URL)
		if !supabaseName.MatchString(name) || server.Type != "http" || err != nil || parsed.Scheme != "https" || parsed.Host != "mcp.supabase.com" || parsed.Path != "/mcp" || parsed.User != nil || parsed.Fragment != "" || !supabaseProject.MatchString(parsed.Query().Get("project_ref")) {
			return nil, "", errors.New("workspace Supabase connection must use a named, project-scoped official endpoint")
		}
		for key, values := range parsed.Query() {
			if len(values) != 1 {
				return nil, "", errors.New("workspace Supabase endpoint contains duplicate parameters")
			}
			if key != "project_ref" && key != "features" && key != "read_only" && key != "skip_elicitations" {
				return nil, "", errors.New("workspace Supabase endpoint contains unsupported parameters")
			}
		}
		if len(server.Headers) != 1 || !supabaseAuthorization.MatchString(server.Headers["Authorization"]) {
			return nil, "", errors.New("workspace Supabase credentials must reference an environment variable")
		}
		if strings.Contains(name, "production") && parsed.Query().Get("read_only") != "true" {
			return nil, "", errors.New("production Supabase connections must be read-only")
		}
	}
	return servers, credentialWorkspace, nil
}

func supabaseRepositoryIdentity(remote string) string {
	remote = strings.TrimSpace(remote)
	if strings.HasPrefix(remote, "git@") && !strings.Contains(remote, "://") {
		host, path, ok := strings.Cut(strings.TrimPrefix(remote, "git@"), ":")
		if !ok {
			return ""
		}
		remote = "ssh://git@" + host + "/" + path
	}
	parsed, err := url.Parse(remote)
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "ssh") || parsed.Host == "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return ""
	}
	path := strings.TrimSuffix(strings.Trim(parsed.Path, "/"), ".git")
	if len(strings.Split(path, "/")) != 2 {
		return ""
	}
	return strings.ToLower(parsed.Host + "/" + path)
}

func inheritSupabaseCredentials(servers map[string]supabaseServer, environment, canonical []string) []string {
	values := map[string]string{}
	for _, entry := range environment {
		name, value, _ := strings.Cut(entry, "=")
		values[name] = value
	}
	needed := map[string]bool{}
	for _, server := range servers {
		match := supabaseAuthorization.FindStringSubmatch(server.Headers["Authorization"])
		if len(match) == 2 && strings.TrimSpace(values[match[1]]) == "" {
			needed[match[1]] = true
		}
	}
	fallback := []string{}
	for _, entry := range canonical {
		name, _, _ := strings.Cut(entry, "=")
		if needed[name] {
			fallback = append(fallback, entry)
		}
	}
	return mergeEnv(environment, fallback)
}

func provisionWorkspaceSupabase(profileDir, workspace string, servers map[string]supabaseServer) error {
	if len(servers) == 0 {
		return nil
	}
	path := filepath.Join(profileDir, ".claude.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return errors.New("could not read Claude workspace state")
	}
	var state map[string]json.RawMessage
	if json.Unmarshal(data, &state) != nil || state == nil {
		return errors.New("Claude workspace state is malformed")
	}
	projects := map[string]map[string]json.RawMessage{}
	if raw := state["projects"]; raw != nil {
		if json.Unmarshal(raw, &projects) != nil || projects == nil {
			return errors.New("Claude projects state is malformed")
		}
	}
	project := projects[workspace]
	if project == nil {
		project = map[string]json.RawMessage{}
	}
	configured := map[string]json.RawMessage{}
	if raw := project["mcpServers"]; raw != nil {
		if json.Unmarshal(raw, &configured) != nil || configured == nil {
			return errors.New("Claude MCP server state is malformed")
		}
	}
	for name, server := range servers {
		configured[name], err = json.Marshal(server)
		if err != nil {
			return errors.New("could not encode Supabase connection")
		}
	}
	project["mcpServers"], err = json.Marshal(configured)
	if err != nil {
		return err
	}
	projects[workspace] = project
	state["projects"], err = json.Marshal(projects)
	if err != nil {
		return err
	}
	data, err = json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(profileDir, ".supabase-state-*")
	if err != nil {
		return errors.New("could not prepare Claude Supabase state")
	}
	defer os.Remove(temporary.Name())
	if _, err := temporary.Write(append(data, '\n')); err != nil {
		_ = temporary.Close()
		return errors.New("could not write Claude Supabase state")
	}
	if err := temporary.Close(); err != nil {
		return errors.New("could not close Claude Supabase state")
	}
	if err := os.Rename(temporary.Name(), path); err != nil {
		return errors.New("could not activate Claude Supabase state")
	}
	return nil
}

func supabasePermissionRules(servers map[string]supabaseServer) []string {
	rules := make([]string, 0, len(servers))
	for name := range servers {
		rules = append(rules, "mcp__"+name+"__*")
	}
	sort.Strings(rules)
	return rules
}

func checkWorkspaceSupabase(ctx context.Context, servers map[string]supabaseServer, environment []string) error {
	values := map[string]string{}
	for _, entry := range environment {
		key, value, _ := strings.Cut(entry, "=")
		values[key] = value
	}
	checked := map[string]bool{}
	for _, rule := range supabasePermissionRules(servers) {
		name := strings.TrimSuffix(strings.TrimPrefix(rule, "mcp__"), "__*")
		server := servers[name]
		match := supabaseAuthorization.FindStringSubmatch(server.Headers["Authorization"])
		if len(match) != 2 || strings.TrimSpace(values[match[1]]) == "" {
			return fmt.Errorf("Supabase connection %s: credential is missing from the workspace and shared loader; repair the configured direnv credential source", name)
		}
		if strings.ContainsAny(values[match[1]], "\r\n") {
			return fmt.Errorf("Supabase connection %s: credential environment variable is invalid", name)
		}
		key := server.URL + "\x00" + match[1]
		if checked[key] {
			continue
		}
		if err := checkSupabaseEndpoint(ctx, server.URL, "Bearer "+values[match[1]]); err != nil {
			return fmt.Errorf("Supabase connection %s: %w", name, err)
		}
		checked[key] = true
	}
	return nil
}

func checkSupabaseEndpoint(ctx context.Context, endpoint, authorization string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	client := &http.Client{CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	session := ""
	protocol := ""
	nextID := 0
	call := func(method string, params json.RawMessage) (json.RawMessage, error) {
		var id *int
		if !strings.HasPrefix(method, "notifications/") {
			nextID++
			currentID := nextID
			id = &currentID
		}
		payload, err := json.Marshal(struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      *int            `json:"id,omitempty"`
			Method  string          `json:"method"`
			Params  json.RawMessage `json:"params"`
		}{"2.0", id, method, params})
		if err != nil {
			return nil, errors.New("could not prepare availability check")
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
		if err != nil {
			return nil, errors.New("invalid endpoint")
		}
		request.Header.Set("Authorization", authorization)
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Accept", "application/json, text/event-stream")
		request.Header.Set("User-Agent", "Vibe-Remote/1.0")
		if protocol != "" {
			request.Header.Set("MCP-Protocol-Version", protocol)
		}
		if session != "" {
			request.Header.Set("Mcp-Session-Id", session)
		}
		response, err := client.Do(request)
		if err != nil {
			return nil, errors.New("availability check failed; check network access")
		}
		defer response.Body.Close()
		if id == nil && (response.StatusCode == http.StatusAccepted || response.StatusCode == http.StatusOK) {
			return nil, nil
		}
		if response.StatusCode != http.StatusOK {
			if response.StatusCode == http.StatusUnauthorized {
				return nil, errors.New("credential was rejected; refresh the configured shared Supabase token")
			}
			if response.StatusCode == http.StatusForbidden {
				return nil, errors.New("credential does not have project access; check the shared Supabase account permissions")
			}
			if response.StatusCode == http.StatusTooManyRequests {
				return nil, errors.New("Supabase rate limit reached; retry after the limit clears")
			}
			return nil, fmt.Errorf("availability check returned HTTP %d; check credentials and project access", response.StatusCode)
		}
		if id := response.Header.Get("Mcp-Session-Id"); id != "" {
			session = id
		}
		body, err := io.ReadAll(io.LimitReader(response.Body, 1024*1024))
		if err != nil {
			return nil, errors.New("could not read availability check")
		}
		if strings.HasPrefix(response.Header.Get("Content-Type"), "text/event-stream") {
			scanner := bufio.NewScanner(bytes.NewReader(body))
			scanner.Buffer(make([]byte, 4096), 1024*1024)
			body = nil
			for scanner.Scan() {
				if line := scanner.Text(); strings.HasPrefix(line, "data:") {
					body = []byte(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
					break
				}
			}
		}
		var result struct {
			Result json.RawMessage `json:"result"`
			Error  json.RawMessage `json:"error"`
		}
		if json.Unmarshal(body, &result) != nil || len(result.Result) == 0 || (len(result.Error) > 0 && string(result.Error) != "null") {
			return nil, errors.New("MCP availability check failed; check credentials and project access")
		}
		return result.Result, nil
	}
	initialized, err := call("initialize", json.RawMessage(`{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"vibe-remote","version":"1.0"}}`))
	if err != nil {
		return err
	}
	var initialization struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if json.Unmarshal(initialized, &initialization) != nil || initialization.ProtocolVersion != "2025-03-26" {
		return errors.New("MCP protocol negotiation failed")
	}
	protocol = initialization.ProtocolVersion
	if _, err := call("notifications/initialized", json.RawMessage(`{}`)); err != nil {
		return err
	}
	result, err := call("tools/list", json.RawMessage(`{}`))
	if err != nil {
		return err
	}
	var list struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if json.Unmarshal(result, &list) != nil {
		return errors.New("MCP tool list is malformed")
	}
	for _, tool := range list.Tools {
		if tool.Name == "execute_sql" {
			return nil
		}
	}
	return errors.New("SQL tool is unavailable; check database feature access")
}
