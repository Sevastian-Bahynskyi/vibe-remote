# Vibe Remote

Vibe Remote is a small macOS service for choosing which Claude subscription is available through Claude Remote Control and for carrying work context between Claude Code and Codex. It does not rotate accounts automatically, read usage quotas, switch Codex accounts, or depend on Omnigent/Omni Route.

## What it does

- Stores any number of independently authenticated Claude Code profiles, labeled by email.
- Runs as many managed interactive Claude Remote Control sessions as you start, in parallel, each pinned to one account and workspace and each either a new conversation or an earlier one resumed by ID.
- Remembers which conversation each session is talking in, so a restarted service resumes it instead of opening an empty one.
- Exposes a mobile dashboard only through Tailscale Serve at `/vibe-remote/`; the app itself listens only on `127.0.0.1:47173`.
- Captures provider-neutral session checkpoints from official Claude and Codex hooks without an LLM call.
- Continues a checkpoint on another Claude account in one click: creates a session with the source slot's name and workspace, attaches the latest eight captured turns plus live Git state, and starts continuation automatically. Codex destinations retain the 48-hour handoff and manual `continue` trigger.
- Keeps closed checkpoints for 30 days. Pinned checkpoints are retained until unpinned.
- Blocks idle sleep on AC power only while slots are actually serving Remote Control, and tracks the battery cost of doing so. Closing the lid can still make it unavailable, by design.

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
5. Press **New** under **Remote sessions**, choose the account, the workspace, and either **New conversation** or an earlier Claude checkpoint to resume. The dashboard reports success only after Claude registers an interactive Remote Control bridge.
6. On the phone, sign into the matching Claude account and tap **Open live session** on that card in the dashboard. On the Mac itself the same card offers **Open in Claude Desktop**, which hands the session's Remote Control link to the Claude app instead of a browser tab; **Open in browser** stays next to it.

## Running and switching sessions

Start as many sessions as you need. Each one gets a distinct Remote Control name — by default `Vibe Remote · <email> · <workspace>`, made unique with a numeric suffix — so parallel sessions are told apart in the Claude app. Starting, stopping, restarting, or removing one session never disturbs the others.

**Move / edit** on a session card changes its account, workspace, conversation, or name and restarts it so the change takes effect. **Restart** reapplies the current settings in place. Both first request a graceful stop; if active work does not stop, the dashboard asks before forcing it, and every open checkpoint known to that worker is marked interrupted with a fresh Git snapshot before termination.

Removing a session stops it and forgets the slot; its captured checkpoints are kept. Sessions that were running are restored automatically when the service restarts.

A session started as **New conversation** adopts the first conversation it actually talks in, and keeps it from then on: a restarted service, or a worker the supervisor restarts after a crash, resumes that conversation rather than opening an empty one. The binding comes from the slot identity carried in the worker's environment and reported by its checkpoint hook, so two sessions sharing one account and workspace never claim each other's conversation. **Move / edit** shows the adopted conversation as the default and leaves it alone unless you pick another one or choose **Start a new conversation**, which is the explicit way to reset a slot. Moving a session to another account or workspace drops the link, because a conversation belongs where it started.

Workspaces and session checkpoints live under **Settings**, collapsed by default.

The dashboard is the same page on the Mac and on the phone. Opened on the Mac it lays out for a laptop window — sessions and accounts side by side, each card's controls on the row with the name they act on — and the desktop-only actions appear, decided by the server: Tailscale Serve proxies to the same loopback listener, so a request is treated as local only when it carries no forwarding headers.

## Handoff workflow

1. Pick the exact source session in **Session checkpoints**.
2. Choose a Claude email or Codex as the destination.
3. For another Claude account, open the automatically created destination session to follow its progress; no message is required. A matching source slot is stopped first. Retrying reuses the destination conversation, leaving unrelated slots alone. For Codex, open the destination in the same workspace and type exactly `continue`.

The checkpoint contains visible captured prompts/responses, branch, HEAD, status, and changed paths, capped at 12,000 characters. It does not parse private transcript files. Automatic Claude continuation uses a private, slot-specific file, removed after delivery through the prompt hook; checkpoint text never appears in process arguments. Failed starts retain it for retry. Manual handoffs use a short SQLite claim lease so concurrent `continue` prompts cannot consume the same ticket. Hook protocols provide no post-injection acknowledgment, so the final boundary is best-effort after the context has been written successfully to the provider hook pipe. This transfers checkpoint context, not the complete native conversation history.

For a native Claude session, **Copy resume command** restores the correct isolated account. After resuming locally, type `/desktop` to move that CLI conversation into Claude Desktop. Claude Desktop and CLI keep separate history until that explicit transfer.

## Browser and computer use

Remote Control keeps the full local Claude environment, including local MCP servers and project configuration. Browser work is available after Claude in Chrome is configured. Native computer control is available on supported Pro/Max accounts after enabling the built-in `computer-use` MCP server with `/mcp` for that project and granting macOS Accessibility and Screen Recording permissions. Those one-time macOS approvals cannot safely be bypassed by Vibe Remote.

## Security model

- No public listener and no Tailscale Funnel.
- Tailscale access control is the network authentication boundary.
- Dashboard mutations require a same-origin custom header and responses use CSP, frame denial, no-referrer, and no-store headers.
- Credentials stay in Claude-managed Keychain entries; Vibe Remote's database contains labels, profile paths, session metadata, and checkpoint text only.
- The active Remote Control URL is held only in service memory and returned only through the tailnet-only dashboard API. OAuth output is discarded and never written to service logs.
- Paths are validated before profile removal or workspace execution.
- Existing Tailscale Serve routes are preserved; Vibe Remote owns only `/vibe-remote/`.

## Keeping the Mac awake

Remote Control reaches a slot only while that slot's process holds its connection open, and a sleeping Mac drops those connections. Availability therefore costs wakefulness. Vibe Remote takes an IOKit `NetworkClientActive` power assertion — narrower than `caffeinate -s`, so a deliberate sleep or a closed lid still works — and holds it only when it must:

| `VIBE_REMOTE_AWAKE` | Behavior |
| --- | --- |
| `auto` (default) | Hold only while at least one slot is serving. Reachability is unchanged: with no slots running there is nothing to reach. |
| `always` | Hold for the daemon's whole lifetime. |
| `off` | Never hold. Slots drop off Remote Control whenever the Mac sleeps. |

No policy holds the assertion on battery power; on battery, sleeping is what protects the charge.

## Battery tracking

`vibe-remote battery` prints the gauge and the trend across recorded samples, then records one itself. The daemon writes on start, on stop, whenever it takes or drops the assertion, and once a day — the daily sample exists because under `auto` a busy Mac never changes keep-awake state, so event-driven rows alone would leave a month of uptime holding a single sample. History is a JSONL file at `~/Library/Application Support/Vibe Remote/battery-history.jsonl`.

Two separate lines report wakefulness, and the distinction matters:

- `Keep-awake (ours)` / `Keep-awake duty` — whether *this daemon* is holding the Mac awake.
- `Sleep blocked` — whether *anything* is, and what. Claude Code takes its own `caffeinate -i` whenever a session is working, and Electron apps hold `NoIdleSleepAssertion`, so the Mac can be pinned awake with this project's assertion released. Reading only our own assertion would have flattered the policy and misreported an idle Mac.

Note that macOS smooths the health percentage it displays: it matched no raw IOKit ratio on the machine this was built against (93.9% nominal-to-design read as 96%), so `Gauge capacity` in mAh is the honest trend signal and the percentage will sit still for weeks and then step.

## Operations

```sh
# Service and account/checkpoint status
"$HOME/Library/Application Support/Vibe Remote/bin/vibe-remote" status

# Battery wear and keep-awake duty cycle
"$HOME/Library/Application Support/Vibe Remote/bin/vibe-remote" battery
"$HOME/Library/Application Support/Vibe Remote/bin/vibe-remote" battery -json

# Reinstall/repair hooks
"$HOME/Library/Application Support/Vibe Remote/bin/vibe-remote" hooks-install

# Tests
go test ./...
go test -race ./...
go vet ./...
```

If launchd is killed unexpectedly, Vibe Remote records its child PID and terminates only a matching stale Claude Remote Control process before restoring the selected worker.
