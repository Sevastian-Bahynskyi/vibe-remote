package claude

import (
	"context"
	"encoding/json"
	"os/exec"
	"strings"
	"time"
)

// direnvTimeout bounds how long a worker start waits on direnv before giving
// up. direnv only ever reads a local .envrc and does no network I/O itself, so
// a real evaluation finishes in milliseconds; the bound exists to survive a
// misbehaving or hung "direnv" on PATH without blocking a slot's launch.
const direnvTimeout = 5 * time.Second

// direnvEnv reports the environment variables a workspace's .envrc computes,
// as KEY=VALUE pairs ready to merge into a Command's Env.
//
// Claude is started by exec, not by an interactive shell that "cd"s into the
// workspace, so direnv's shell hook never runs. An MCP server configured with
// a direnv-exported reference (e.g. Supabase's "${SUPABASE_TOKEN_ACCOUNT2}")
// would otherwise see it empty. Running "direnv export json" with the
// workspace as its working directory reproduces exactly what the hook would
// have injected on "cd".
//
// A missing direnv binary, a workspace with no .envrc, an unauthorized
// .envrc, or any other failure yields no variables rather than an error: most
// workspaces have no .envrc at all, and a worker should still start using
// whatever the host process already has.
func direnvEnv(workspace string) []string {
	binary, err := exec.LookPath("direnv")
	if err != nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), direnvTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "export", "json")
	cmd.Dir = workspace
	output, err := cmd.Output()
	if err != nil {
		return nil
	}
	trimmed := strings.TrimSpace(string(output))
	if trimmed == "" {
		return nil
	}
	var exported map[string]json.RawMessage
	if err := json.Unmarshal([]byte(trimmed), &exported); err != nil {
		return nil
	}
	result := make([]string, 0, len(exported))
	for key, raw := range exported {
		// direnv's own bookkeeping (DIRENV_DIFF, DIRENV_DIR, ...) is meaningful
		// only to a shell running its hook; carrying it into Claude's process
		// would claim a load that never happened through that mechanism.
		if strings.HasPrefix(key, "DIRENV_") {
			continue
		}
		var value string
		if err := json.Unmarshal(raw, &value); err != nil {
			continue
		}
		result = append(result, key+"="+value)
	}
	return result
}

// mergeEnv appends overrides onto base, dropping any base entry an override
// replaces so the result carries one entry per variable and the override
// always wins.
func mergeEnv(base []string, overrides []string) []string {
	if len(overrides) == 0 {
		return base
	}
	replaced := make(map[string]struct{}, len(overrides))
	for _, item := range overrides {
		key, _, _ := strings.Cut(item, "=")
		replaced[key] = struct{}{}
	}
	result := make([]string, 0, len(base)+len(overrides))
	for _, item := range base {
		key, _, _ := strings.Cut(item, "=")
		if _, skip := replaced[key]; skip {
			continue
		}
		result = append(result, item)
	}
	return append(result, overrides...)
}
