# Codex Handoff — Vibe Remote

Use this document to resume development in a fresh Codex session. Read `README.md` for the product contract, then verify the live state before changing code.

## Mission

Vibe Remote is a small macOS service that lets the user manually select which authenticated Claude account runs one local Claude Remote Control worker. A Tailscale-only phone dashboard controls the selection. Provider-neutral checkpoints let Claude and Codex continue each other's work by typing exactly `continue` in the same workspace.

Keep these boundaries intact:

- Claude account switching is manual. There is no usage monitoring or automatic rotation.
- Codex account switching is outside this product. Codex only participates in checkpoint capture and handoff consumption.
- Each Claude account has an isolated `CLAUDE_CONFIG_DIR` and authenticates through the official Claude login flow.
- Vibe Remote never reads, copies, exports, or stores OAuth tokens.
- Any number of managed interactive Claude Remote Control workers may run at once, one per remote session slot.
- The dashboard listens on localhost and is exposed only through Tailscale Serve. Tailscale Funnel stays off.
- Checkpoint capture and consumption are deterministic hook/database operations with no model call.
- Handoffs are destination-scoped, one-use, and expire after 48 hours.

## Repository state

- Repository: `git@github.com:Sevastian-Bahynskyi/vibe-remote.git`
- Working directory: `/Users/seva/Developer/personal/vibe-remote`
- Active branch: `feature/remote-account-switcher`
- Handoff base commit: `0c62d21` (`design: rebrand dashboard as terminal console`)
- `main` still points to the earlier product and has not been replaced.
- Remote branches currently include `main`, `feature/remote-account-switcher`, and `native-harness-direction`.

The user previously chose this completion policy: after explicit acceptance of the finished system, replace `main` with this implementation and remove every other branch except `main` and `feature/remote-account-switcher`. That branch operation has not been performed.

## Live installation at handoff

Verified on 2026-09-06:

- Dashboard: `https://sevastians-macbook-pro.tailfeb3fa.ts.net/vibe-remote/`
- Local service: `127.0.0.1:47173`
- LaunchAgent: `com.seva.vibe-remote`, running
- Tailscale Serve: `/vibe-remote` proxies to the local service; tailnet-only; Funnel off
- Claude hooks and Codex hooks: installed
- Codex hook fingerprint: verified
- Selected workspace: `/Users/seva/Developer/personal/vibe-remote`
- Active account: `couplegoai.main@gmail.com`, authenticated
- Second account: `support@couplegoai.com`, authenticated and available to switch
- Managed worker: running for the active account and selected workspace
- Captured sessions: zero after cleanup/verification
- Power state: on battery, so the dashboard correctly warns that the Mac may become unreachable

The installed binary and state live under `~/Library/Application Support/Vibe Remote`. Account credentials remain in Claude-managed macOS Keychain entries tied to each isolated profile directory.

## Implemented behavior

- Add, reauthenticate, activate, and remove Claude accounts from the dashboard.
- Add, select, and remove absolute-path workspaces.
- Start any number of PTY-backed interactive Claude Remote Control processes, one per remote session slot, each pinned to an account, a workspace, and optionally an earlier conversation to resume; restore every slot that should be running after a service restart.
- Start, stop, restart, move, and remove one session without disturbing the others.
- Wait for Claude to report successful Remote Control registration before reporting activation success.
- Gracefully stop an existing worker; require confirmation before forced interruption.
- Capture Claude and Codex prompt/response lifecycle events through official hooks.
- Store the latest eight captured turns plus live Git context in a checkpoint payload capped at 12,000 characters.
- Create a Claude-account or Codex destination handoff and inject it when the destination sends exactly `continue` from the same workspace.
- Claim handoffs with a short SQLite lease to prevent concurrent double consumption.
- Pin checkpoints, retain closed unpinned checkpoints for 30 days, and copy isolated Claude resume commands.
- Keep the Mac awake while connected to AC power.
- Serve a responsive Matrix-style terminal dashboard tested at 390 px without horizontal overflow.

## Code map

- `cmd/vibe-remote/main.go`: CLI commands and service composition.
- `internal/claude/manager.go`: isolated profiles, authentication, trust checks, worker lifecycle, restoration, and stale-child cleanup.
- `internal/claude/runner.go`: Claude process execution and output handling.
- `internal/checkpoint/`: hook parsing, Git snapshots, handoff creation, and `continue` injection.
- `internal/store/`: SQLite migrations and all durable state/claim operations.
- `internal/server/server.go`: localhost HTTP API, mutation guard, security headers, and embedded dashboard.
- `internal/server/static/`: dashboard HTML, JavaScript, and terminal styling.
- `internal/install/install.go`: binary/LaunchAgent installation, hook merging, and Tailscale Serve configuration.
- `internal/system/health.go`: Tailscale, hook, installation, and power health reporting.
- `internal/paths/paths.go`: authoritative runtime paths and listener constants.

## Resume sequence

1. Confirm the branch and working tree with `git status --short --branch` and compare `HEAD` to the handoff base commit above. Completion criterion: every newer change is understood before editing.
2. Read `README.md`, then inspect the relevant implementation and tests from the code map. Completion criterion: the requested change preserves every mission boundary.
3. Inspect the live service with `curl -fsS http://127.0.0.1:47173/api/state | jq`. Completion criterion: distinguish current runtime state from implementation assumptions.
4. Run `go test ./...`, `go test -race ./...`, and `go vet ./...` before declaring implementation work complete.
5. Rebuild and reinstall only when code or embedded UI changes need live verification:

   ```sh
   go build -o ./build/vibe-remote ./cmd/vibe-remote
   ./build/vibe-remote install
   ```

6. Verify the phone-sized page through the live Tailscale URL, not only localhost. Completion criterion: account state is current, mutations work, and the page has no horizontal overflow.

## Acceptance work still open

The implementation and automated tests are complete, but final user acceptance still needs these real workflows:

1. Activate `support@couplegoai.com` with the selected workspace from the dashboard. If Claude reports missing workspace trust, open that isolated profile locally in the exact workspace and let the user approve Claude's trust prompt once. Confirm the worker registers Remote Control successfully.
2. From the phone, switch the Claude mobile account to `support@couplegoai.com`, open **Code**, and connect to `Vibe Remote · support@couplegoai.com`.
3. Switch back to `couplegoai.main@gmail.com` from the dashboard and repeat the phone connection with the matching mobile account.
4. Generate a real checkpoint in one provider, create a destination-scoped handoff in the dashboard, type exactly `continue` in the other provider from the same workspace, and verify the injected context is sufficient to continue.
5. Repeat the handoff in the opposite provider direction.
6. Obtain explicit user acceptance before performing the branch replacement and cleanup described under **Repository state**.

## Important implementation facts

- First use of a workspace by an isolated Claude profile may require Claude's local trust prompt; the service detects and reports this instead of bypassing it.
- The active Claude Remote Control URL is held only in service memory and exposed through the tailnet-only dashboard API. OAuth output must not be added to logs or API responses.
- Dashboard mutations require `X-Vibe-Remote: 1`; keep the same-origin and response security headers intact.
- Hook protocols do not acknowledge post-injection processing. Handoff finalization is best-effort after successful output to the provider hook pipe.
- Existing Codex notification configuration is forwarded by the installer rather than replaced.
- Existing Tailscale Serve routes are preserved; this service owns only `/vibe-remote/`.
- Closing the MacBook lid can make the Mac unreachable by design. The keep-awake process runs only on AC power.
- Native Claude CLI history and Claude Desktop history are separate until the user runs `/desktop` after resuming the isolated CLI session.

## Last verification

Immediately before this handoff:

- `go test ./...` passed.
- `go test -race ./...` passed.
- `go vet ./...` passed.
- `git diff --check` passed.
- The live service survived reinstall and restored its worker.
- Both configured Claude accounts reported `authenticated` with no account error.
- System health reported Tailscale online, tailnet-only, Funnel off, Serve ready, hooks installed, and hooks verified.
- The live Matrix dashboard rendered successfully at mobile and desktop sizes.
