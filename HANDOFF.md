# Vibe Remote development handoff

Read `README.md`, the current owner instructions, and `docs/audits/2026-10-26-implementation-progress.md` before changing code. The October 25 audit preserves the original evidence; it is not a description of the corrected runtime.

## Current work

Working tree: `/Users/seva/Developer/personal/vibe-remote`. The reliability work was developed on `bugfix/vibe-remote-reliability`, based on `51d61d7f38e135daadd96e506f3cdc40f8ad9ca1`. The owner subsequently authorized committing all pending changes, pushing them, and integrating them into `main`. Verify the current branch and remote state with Git. Preserve existing branches and do not apply older branch-replacement instructions. Never put `codex` in branch names or commit messages.

The owner cancelled overnight scheduling and authorized implementation and repeated service restarts on October 26. The `overnight-vibe-remote-fixes` heartbeat was deleted. Its earlier idle/pending-question gate is superseded. Do not recreate it or ask for restart authorization again.

## Product boundaries

Any number of independent Claude Remote Control slots may run, each pinned to an account, workspace, and native conversation. Account switching is manual. OAuth remains in Claude-managed isolated Keychain entries. No quota polling, automatic rotation, token copying, or Codex account switching.

The daemon listens on `127.0.0.1:47173`; the existing tailnet-only route is `https://sevastians-macbook-pro.tailfeb3fa.ts.net/vibe-remote/`. Preserve CSP, mutation/console origin guards, private filesystem modes, and unrelated Serve routes. No Funnel.

Unknown/new Claude selections use `opus` / `high`; continuations retain source requested/observed settings per field. Deliberate changes update intent; fallback updates observation. Managed callbacks require current run/account/workspace/conversation and top-level scope. Never transfer GPT model IDs to Claude.

Normal `~/.claude` supplies only the approved baseline categories and safe preferences. Explicit shared hooks/environment remain in `settings.shared.json`. Required user plugins and registries use official Claude tooling. Fingerprinted profile reconciliation preserves local extras, backs up conflicts, and revokes only owned entries. Do not copy whole credential, history, or plugin-state directories.

Managed project memory uses canonical Git common-directory identity or canonical workspace path. Content/provenance imports preserve originals and differing versions. Normal Claude CLI memory is unchanged.

## Code map

- `cmd/vibe-remote/`: service composition, account-only authentication helpers, durable setup completion/cancellation.
- `internal/claude/`: daemon ownership and verified recovery, environment boundaries, safe profile/plugin reconciliation, project memory, PTY lifecycle, bounded buffers and adaptive computer-use approval.
- `internal/model/`, `internal/store/`: typed requested/observed selection, additive SQLite migrations 9/10, durable setup jobs, transactional callback scope, activity and retention counters.
- `internal/checkpoint/`: hook intent/observation parsing and deterministic continuation context.
- `internal/server/`: JSON/HTML setup/model/status/diagnostics interfaces, coherent refresh fragments and controls.
- `internal/server/static/`: external poll registry, stale/visibility guards, run-scoped console disposal and history privacy.
- `scripts/`: fake-clock dashboard regressions and disposable 30-minute browser/resource soak.

## Verification and installation

Run `go test ./...`, `go test -race ./...`, `go vet ./...`, `node --test scripts/test-dashboard.mjs`, and `git diff --check` after material changes. Read the dated progress/resource reports for actual measured evidence and unresolved acceptance limits; do not assume a historical browser spike is fixed.

Before live migration/install, make a consistent private SQLite backup and preserve the installed binary and configuration/import sources. Supported commands:

```sh
go build -o ./build/vibe-remote ./cmd/vibe-remote
./build/vibe-remote install
```

Stop legacy workers gracefully before the first ownership-record upgrade. Unverifiable live PID records must not be killed or discarded. New daemons require the exclusive installation lock before recovery/restoration. Authentication helpers do not control workers.

Verify original account/workspace/conversation bindings after restart and verify the private dashboard. Use disposable acceptance workspaces for model/handoff checks. Do not delete or reauthenticate an owner's account to substitute for a genuinely new human OAuth sign-in. That final sign-in requires the owner if no newly authenticated profile is available. Retention defaults to seven days (configurable); protected live/current conversations and pinned checkpoints remain.
