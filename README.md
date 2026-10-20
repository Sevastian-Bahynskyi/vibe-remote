# Vibe Remote

Vibe Remote is a small macOS service for choosing which Claude subscription is available through Claude Remote Control and for carrying work context between Claude Code and Codex. It does not rotate accounts automatically, read usage quotas, switch Codex accounts, or depend on Omnigent/Omni Route.

## What it does

- Stores any number of independently authenticated Claude Code profiles, labeled by email.
- Runs as many managed interactive Claude Remote Control sessions as you start, in parallel, each pinned to one account and workspace and each either a new conversation or an earlier one resumed by ID.
- Remembers which conversation each session is talking in, so a restarted service resumes it instead of opening an empty one.
- Exposes a mobile dashboard only through Tailscale Serve at `/vibe-remote/`; the app itself listens only on `127.0.0.1:47173`.
- Captures provider-neutral session checkpoints from official Claude and Codex hooks without an LLM call.
- Continues a checkpoint on another Claude account in one click: creates a session with the source slot's name and workspace, attaches the latest eight captured turns plus live Git state, and starts continuation automatically. Codex destinations retain the 48-hour handoff and accept natural requests to continue.
- Keeps closed checkpoints for 7 days by default. Change the period under System (1–3650 days); saving applies cleanup immediately without restarting. The setting survives daemon restarts. Pinned checkpoints are retained until unpinned, and working sessions are protected. Cleanup removes Vibe Remote checkpoints, not project files or native Claude history.
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
- An Apple Development or Developer ID Application signing certificate in the macOS Keychain

Build and install:

```sh
go build -o ./build/vibe-remote ./cmd/vibe-remote
./build/vibe-remote install
```

The installer places the binary and SQLite database under `~/Library/Application Support/Vibe Remote`, installs `~/Library/LaunchAgents/com.seva.vibe-remote.plist`, merges checkpoint hooks, preserves an existing Codex notifier, and adds only `/vibe-remote/` to Tailscale Serve.

The installer signs the service with a certificate-backed `com.seva.vibe-remote` identity so macOS permissions can survive replacement builds. On first install it prefers a valid Developer ID Application certificate, then Apple Development, and saves that certificate's fingerprint in the private `signing-identity` file. Later installs reuse it. Set `VIBE_REMOTE_SIGNING_IDENTITY` to a certificate's 40-character fingerprint to choose explicitly. Signing keys stay in Keychain. An expired or unavailable saved certificate stops installation rather than silently reverting to ad hoc signing. After migrating an older installation, remove and re-add the installed service in macOS permissions once.

Open the URL shown by:

```sh
"$HOME/Library/Application Support/Vibe Remote/bin/vibe-remote" status
```

## First-time setup

1. Add each Claude account from the dashboard. An official login opens on the Mac; complete it once with the email shown in the dashboard.
2. Add each local workspace by absolute path.
3. Adding a workspace authorizes managed sessions to trust that folder and enable its configured MCP servers automatically, including sessions on newly added accounts.
4. In Codex, open `/hooks` once and trust the installed Vibe Remote hooks. The dashboard changes from `Installed · verify /hooks` to `Verified` after an actual hook event.
5. Press **New** under **Remote sessions**, choose the account, the workspace, and either **New conversation** or an earlier Claude checkpoint to resume. The dashboard reports success only after Claude registers an interactive Remote Control bridge.
6. On the phone, sign into the matching Claude account and tap **Open live session** on that card in the dashboard. On the Mac itself the same card offers **Open in Claude Desktop**, which hands the session's Remote Control link to the Claude app instead of a browser tab; **Open in browser** stays next to it.

## Running and switching sessions

Start as many sessions as you need. Each one gets a distinct Remote Control name — by default `Vibe Remote · <email> · <workspace>`, made unique with a numeric suffix — so parallel sessions are told apart in the Claude app. Starting, stopping, restarting, or removing one session never disturbs the others. Managed sessions start in Claude’s `bypassPermissions` mode, as authorized by the owner. Vibe Remote saves the owner-authorized first-use acknowledgement in each session’s settings, including new profiles. Routine tool calls do not go through the auto-mode classifier or permission prompts. Explicit deny rules, project hooks, and server-side restrictions still apply. Restart a session to apply this launch setting; existing running sessions keep their current mode.

**Move / edit** on a session card changes its account, workspace, conversation, or name and restarts it so the change takes effect. **Restart** reapplies the current settings in place. Both first request a graceful stop; if active work does not stop, the dashboard asks before forcing it, and every open checkpoint known to that worker is marked interrupted with a fresh Git snapshot before termination.

At every launch, managed sessions load the `allow`, `ask`, and `deny` permission rules from `~/.claude/settings.json` through a private settings file. This shares your saved approvals across accounts without copying credentials, environment variables, hooks, or permission modes. Vibe Remote also allows `Bash(gh issue edit *)`; matching ask and deny rules still take precedence. Changes to the shared rules apply at the next session launch. Account-specific settings and project rules remain in effect. Managed sessions also approve project `.mcp.json` servers at launch, so a new profile does not stop at the MCP setup dialog; explicitly disabled servers remain disabled.

Removing a session stops it and forgets the slot; its captured checkpoints are kept. Sessions that were running are restored automatically when the service restarts.

Session cards and details show pending tool names and elapsed time separately from the Remote Control connection. When an MCP server asks for input, the session shows **Waiting for confirmation**, the server name, and guidance for responding in Claude. Vibe Remote observes these requests without accepting or changing them. Calls pending for two minutes show **Taking longer** with a reminder to verify database state before retrying a write. Restarting or stopping a session clears its activity; callbacks from its old worker cannot restore it. Activity contains names and timing only, with no tool arguments, SQL, server messages, or response bodies. When a server omits its confirmation request ID, requests are grouped by server and the uniquely identifiable pending tool; concurrent requests without IDs cannot always be distinguished.

A session started as **New conversation** adopts the first conversation it actually talks in, and keeps it from then on: a restarted service, or a worker the supervisor restarts after a crash, resumes that conversation rather than opening an empty one. The binding comes from the slot identity carried in the worker's environment and reported by its checkpoint hook, so two sessions sharing one account and workspace never claim each other's conversation. **Move / edit** shows the adopted conversation as the default and leaves it alone unless you pick another one or choose **Start a new conversation**, which is the explicit way to reset a slot. Moving a session to another account or workspace drops the link, because a conversation belongs where it started.

Workspaces and session checkpoints live under **Settings**, collapsed by default.

The dashboard is the same page on the Mac and on the phone. Opened on the Mac it lays out for a laptop window — sessions and accounts side by side, each card's controls on the row with the name they act on — and the desktop-only actions appear, decided by the server: Tailscale Serve proxies to the same loopback listener, so a request is treated as local only when it carries no forwarding headers.

## Handoff workflow

1. Pick the exact source session in **Session checkpoints**.
2. Choose a Claude email or Codex as the destination.
3. For another Claude account, open the automatically created destination session to follow its progress; no message is required. A matching source slot is stopped first. Retrying reuses the destination conversation, leaving unrelated slots alone. For Codex, open the destination in the same workspace and ask it to continue.

The checkpoint contains visible captured prompts/responses, branch, HEAD, status, and changed paths, capped at 12,000 characters. It does not parse private transcript files. Automatic Claude continuation uses a private, slot-specific file, removed after delivery through the prompt hook; checkpoint text never appears in process arguments. Failed starts retain it for retry. Manual handoffs use a short SQLite claim lease so concurrent continuation prompts cannot consume the same ticket. Hook protocols provide no post-injection acknowledgment, so the final boundary is best-effort after the context has been written successfully to the provider hook pipe. This transfers checkpoint context, not the complete native conversation history.

For a native Claude session, **Copy resume command** restores the correct isolated account. After resuming locally, type `/desktop` to move that CLI conversation into Claude Desktop. Claude Desktop and CLI keep separate history until that explicit transfer.

## Browser and computer use

Remote Control keeps the full local Claude environment, including local MCP servers and project configuration. Browser work is available after Claude in Chrome is configured. Native computer control is available on supported Pro/Max accounts with the built-in `computer-use` MCP server enabled for the project. Managed sessions enable it at launch. Grant macOS Accessibility (called Device Control and Data Access on newer macOS versions) and Screen Recording to the installed `~/Library/Application Support/Vibe Remote/bin/vibe-remote` service: macOS attributes these sessions' requests to their launching service. Claude Desktop's grants do not cover the service. Restart the service after granting permissions.

Managed native Claude workers run from private, content-addressed copies under `~/Library/Application Support/Vibe Remote/claude-runtime`. Each copy retains Anthropic’s embedded signature and stays unchanged while sessions use it, so an npm or native CLI update cannot unlink a running worker’s executable and break macOS permission checks. New sessions use the currently installed version. Updates are disabled inside these managed copies; update the normal Claude installation to supply a newer version. Script-based launchers keep their original location and dependencies. macOS grants remain attached to the signed Vibe Remote service across account profiles.

The owner authorizes computer use for every managed profile, including future accounts. When you ask a session to use a Mac app, Vibe Remote answers Claude's separate app-access dialog automatically; opening the approval console is unnecessary. Approval follows the current computer-use tool and selected option, so terminal animations cannot stall it. A truncated app list expands automatically before approval. Requested clipboard and system-key flags are included. Unknown dialogs and missing macOS grants still need attention; Claude's app tiers, policy blocks, and single-session lock remain in effect. Vibe Remote does not modify the Claude executable.

Each launch also loads the Mac-wide MCP server definitions from `~/.claude.json` through a private per-session `--mcp-config` file. Local integrations such as Rive are available to existing and future profiles without registering them for every account. Only server definitions are shared; Claude login state, project history, and unrelated account preferences are excluded. Environment-variable references remain references. Updated or removed definitions refresh at the next launch; independent registrations already saved in a profile remain its own configuration.

The Claude mobile app may show an access tool as running without approval buttons. Expand **Approval console** on its Vibe Remote session as a fallback. Use Up/Down or Tab to select an option, Space to select apps, Enter to confirm, and Escape to dismiss. The console cannot submit typed commands and refuses input for restarted or stopping workers; close and reopen it after a restart. Closing the console disconnects polling without stopping Claude.

Each session offers direct handoff buttons for Codex and other signed-in Claude profiles. A handoff stops the source, reads its latest checkpoint, and keeps the workspace. Claude destinations start automatically; Codex uses the existing 48-hour continuation ticket. New sessions default to a new conversation, with previous conversations under a collapsed picker.

Console output is a bounded, in-memory byte stream for the current worker, never saved in logs or checkpoints. Reading and sending keys require the dashboard's same-origin header and reject cross-origin requests. Like other dashboard features, it is available only through the loopback listener or the existing private Tailscale Serve route. Terminal rendering uses bundled xterm.js 6.0.0 (MIT), with its three generated style elements adapted to carry the dashboard's CSP nonce; no external scripts or unrestricted inline styles are needed.

Vibe Remote can provision shared Supabase connections into every Claude profile when a workspace session starts. Configure `~/Library/Application Support/Vibe Remote/workspace-supabase.json` with a `workspaces` object keyed by absolute repository path; each value maps connection names (`supabase`, optional compatibility aliases, and `supabase-production`) to Claude HTTP MCP definitions (`type`, `url`, `headers`). Use the official `https://mcp.supabase.com/mcp` endpoint with an explicit `project_ref`, and `Authorization: Bearer ${YOUR_TOKEN_ENV_VAR}`. Tokens remain in the workspace environment. Production connections must include `read_only=true`. Linked Git worktrees inherit the main repository's configuration. An optional `repositories` object maps normalized Git remote identities such as `github.com/couplegoai/couplegoai` to a configured workspace path, allowing fresh checkouts to inherit the same connections. SSH and HTTPS remote forms match the same identity; conflicting mappings stop launch. Only missing credential variables referenced by those connections are inherited from the configured workspace's direnv environment; unrelated environment settings are not copied.

Before launching a configured session, Vibe Remote checks that credential variables exist and that the authenticated MCP endpoint exposes `execute_sql`. Missing credentials, revoked access, or network failures stop that session's launch with a diagnostic in the dashboard. The check performs no SQL queries. Connection names generate matching allow rules while retaining explicit ask/deny rules and project hooks; bypass permission mode stays enabled. Updating this file takes effect on the next session launch, including launches using newly added profiles.

## Shared Claude session settings

Vibe Remote loads optional `~/.claude/settings.shared.json` whenever it starts a Claude workspace session. Its `env` and `hooks` fields are explicitly shared across every account profile, checkout, and worktree; ordinary global settings still contribute only permission rules. Use absolute hook script paths outside repositories, and let shared hooks check repository identity when a policy applies to one project. Keep secrets in the existing secure environment setup. Missing shared settings preserve the previous behavior; malformed settings stop the session launch. Changes take effect on the next session launch.

For direct Claude sessions, register the same hooks in `~/.claude/settings.json`; for Codex, use `~/.codex/hooks.json` and review new hooks through `/hooks`. These machine-local files survive repository replacement and worktree creation.

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
