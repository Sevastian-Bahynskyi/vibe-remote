# Vibe Remote

Vibe Remote is a small macOS service for choosing which Claude subscription is available through Claude Remote Control and for carrying work context between Claude Code and Codex. It does not rotate accounts automatically, read usage quotas, switch Codex accounts, or depend on Omnigent/Omni Route.

## What it does

- Stores any number of independently authenticated Claude Code profiles, labeled by email.
- Runs exactly one managed `claude remote-control` server for the selected account and workspace.
- Exposes a mobile dashboard only through Tailscale Serve at `/vibe-remote/`; the app itself listens only on `127.0.0.1:47173`.
- Captures provider-neutral session checkpoints from official Claude and Codex hooks without an LLM call.
- Creates destination-scoped, 48-hour, one-use handoffs. Type exactly `continue` in the selected destination to inject the latest eight captured turns plus live Git state.
- Keeps closed checkpoints for 30 days. Pinned checkpoints are retained until unpinned.
- Keeps the Mac awake while it is on AC power. Closing the lid can still make it unavailable, by design.

## Account isolation and credentials

Vibe Remote never reads, copies, exports, or stores OAuth tokens. Each account uses its own `CLAUDE_CONFIG_DIR`; current Claude Code versions key the macOS Keychain credential to that directory. Authentication and revocation run through `claude auth login` and `claude auth logout`.

The long-lived token from `claude setup-token` is deliberately not used because Anthropic restricts it to inference and it cannot establish Remote Control sessions. A full official Claude.ai login is required for every account.

## Install

Requirements:

- macOS with the lid open while remote access is desired
- Tailscale signed in on the Mac and phone
- Claude Code 2.1.261 or newer
- Codex CLI/Desktop with hooks support
- Go 1.25 or newer to build from source

Build and install:

```sh
go build -o ./build/vibe-remote ./cmd/vibe-remote
./build/vibe-remote install
```

The installer places the binary and SQLite database under `~/Library/Application Support/Vibe Remote`, installs `~/Library/LaunchAgents/com.seva.vibe-remote.plist`, merges checkpoint hooks, preserves an existing Codex notifier, and adds only `/vibe-remote/` to Tailscale Serve.

Open the URL shown by:

```sh
"$HOME/Library/Application Support/Vibe Remote/bin/vibe-remote" status
```

## First-time setup

1. Add each Claude account from the dashboard. An official login opens on the Mac; complete it once with the email shown in the dashboard.
2. Add each local workspace by absolute path.
3. Before using a workspace remotely for the first time, run Claude locally in it with the desired profile and accept Claude's workspace trust prompt. Vibe Remote reports this explicitly if it is missing.
4. In Codex, open `/hooks` once and trust the installed Vibe Remote hooks. The dashboard changes from `Installed · verify /hooks` to `Verified` after an actual hook event.
5. Activate an account and workspace. The dashboard reports success only after Claude prints a registered Remote Control URL.
6. On the phone, sign into the matching Claude account, open **Code**, and choose the session named `Vibe Remote · email@example.com`.

## Switching accounts

Choose a workspace, then press **Activate** (or **Restart / move**) on the desired Claude account. Vibe Remote first requests a graceful stop. If active work does not stop, the dashboard asks before forcing it; every open session known to that worker is marked interrupted with a fresh Git snapshot before termination.

Then switch the Claude mobile app to the same email and open its Vibe Remote session. Codex remains under Codex's own account/session management.

## Handoff workflow

1. Pick the exact source session in **Session checkpoints**.
2. Choose a Claude email or Codex as the destination.
3. Open the destination in the same workspace and type exactly `continue`.

The checkpoint contains visible captured prompts/responses, branch, HEAD, status, and changed paths, capped at 12,000 characters. It does not parse private transcript files. Delivery uses a short SQLite claim lease so concurrent `continue` prompts cannot consume the same ticket. Hook protocols provide no post-injection acknowledgment, so the final boundary is best-effort after the context has been written successfully to the provider hook pipe.

For a native Claude session, **Copy resume command** restores the correct isolated account. After resuming locally, type `/desktop` to move that CLI conversation into Claude Desktop. Claude Desktop and CLI keep separate history until that explicit transfer.

## Browser and computer use

Remote Control keeps the full local Claude environment, including local MCP servers and project configuration. Browser work is available after Claude in Chrome is configured. Native computer control is available on supported Pro/Max accounts after enabling the built-in `computer-use` MCP server with `/mcp` for that project and granting macOS Accessibility and Screen Recording permissions. Those one-time macOS approvals cannot safely be bypassed by Vibe Remote.

## Security model

- No public listener and no Tailscale Funnel.
- Tailscale access control is the network authentication boundary.
- Dashboard mutations require a same-origin custom header and responses use CSP, frame denial, no-referrer, and no-store headers.
- Credentials stay in Claude-managed Keychain entries; Vibe Remote's database contains labels, profile paths, session metadata, and checkpoint text only.
- Session URLs and OAuth output are discarded, never written to service logs.
- Paths are validated before profile removal or workspace execution.
- Existing Tailscale Serve routes are preserved; Vibe Remote owns only `/vibe-remote/`.

## Operations

```sh
# Service and account/checkpoint status
"$HOME/Library/Application Support/Vibe Remote/bin/vibe-remote" status

# Reinstall/repair hooks
"$HOME/Library/Application Support/Vibe Remote/bin/vibe-remote" hooks-install

# Tests
go test ./...
go test -race ./...
go vet ./...
```

If launchd is killed unexpectedly, Vibe Remote records its child PID and terminates only a matching stale `claude remote-control` process before restoring the selected worker.
