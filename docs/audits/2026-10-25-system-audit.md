# Vibe Remote system audit — 25 October 2026

The highest priorities are protecting running sessions during account setup, making model selection deterministic, and fixing dashboard polling. A fresh account is currently authenticated successfully without necessarily receiving the same working environment as an established account.

## Scope and evidence

- Audited repository and installed service revision: `51d61d7f38e135daadd96e506f3cdc40f8ad9ca1`. The installed binary reports this revision with no local modifications. Served `app.js` and `console.js` match the repository.
- Inspected the live dashboard through the browser, including its session screen at a 390 × 844 viewport. Inspected SQLite schema and selected metadata through a read-only connection. No credentials or full conversation bodies were printed.
- Live snapshot: three accounts, seven slots, five running Claude workers, and 97 checkpoints. Of the 50 Claude checkpoints, 18 have no recorded model. One running slot also has no recorded model.
- The existing `go test ./...`, `go test -race ./...`, and `go vet ./...` checks pass.
- Temporary Go overlays outside the repository reproduced seven failing cases across six test functions. A separate Node harness reproduced the refresh scheduling defect. These were diagnostic tests of the current implementation, not changes to production behavior.
- No real account login/logout, worker restart, deployment, or profile configuration change was performed. Worker termination was reproduced using a disposable process and a temporary application root.

“Reproduced” below means an isolated fixture demonstrated the behavior. “Code verified” means the implementation establishes the failure path, without exercising the corresponding live operation.

## Findings

### F01 · P1 · Adding or reauthenticating an account can terminate other live workers

**Evidence: reproduced.** `accountAdd` and `accountLogin` construct `claude.New` against the running application's data directory (`cmd/vibe-remote/main.go:155`, `:184`). The constructor always calls `cleanupOrphan` (`internal/claude/manager.go:191`). Cleanup walks every worker PID record and sends TERM, followed by KILL if necessary, when the process command contains `claude` and `remote-control` (`:1069`, `:1089`). It does not establish that the owning daemon is dead.

Constructing a second manager terminated the audit's still-running disposable worker. The daemon may subsequently restart its workers, but an in-progress tool call or turn has already been interrupted.

**Recommendation:** separate account authentication from worker ownership. Run orphan recovery only during exclusive daemon startup, after acquiring a single-instance ownership lock. Store enough process identity to avoid treating an unrelated reused PID as a worker. Account helpers must have no authority to clean up the daemon's children.

**Acceptance:** add and reauthenticate an account while two disposable workers are busy; neither PID, run identity, pending activity, nor conversation changes. Separately verify recovery after a genuine daemon crash.

### F02 · P1 · Model continuity has several incomplete paths

**Evidence: reproduced, with live missing metadata.**

1. Cross-account continuation reads only `session_agents` (`internal/server/continuation.go:90`). When that row is absent or has an empty model, it does not recover the model already stored on the matching source slot. The audit fixture had an Opus source slot and produced a destination with an empty model.
2. `updateSlotOp` changes the resumed conversation while retaining the previous slot's `Model` and `Effort` (`internal/server/ops.go:181`). Selecting a known Opus conversation from a Sonnet slot still produced a Sonnet launch configuration. At launch this becomes an explicit `--model` argument, overriding the selected conversation's usual restoration behavior.
3. If the launch model is empty, `Manager.start` omits `--model` (`internal/claude/manager.go:668`). The destination's settings and provider defaults decide the model. The live profiles already differ: one stores `model: sonnet`, while the other two have no model setting.
4. `ValidModelName` rejects the documented `[1m]` suffix (`internal/model/model.go:171`). A `SessionStart` fixture containing `claude-opus-4-6[1m]` silently lost its model during parsing.

These are verified ways to get the reported Opus-to-Sonnet behavior. The available metadata does not identify which path caused the specific earlier incident, and it does not establish why all 18 historical model values are missing.

**Recommendation:** use one resolution policy for handoff, resume selection, new conversations, and supervisor restart. Preserve the selected conversation's known settings; use an explicitly matched source slot as a fallback; use a documented owner default for genuinely new work. Treat an unknown source model as unknown and surface the selected fallback before launch. Never transfer a Codex model identifier to Claude.

Track the requested model separately from the latest observed model and its observation time. Display both, including “unknown” and any mismatch, on the active session screen. Today model/effort appear in checkpoint details but not active session details. Validate supported model syntax without dropping legitimate identifiers silently.

Claude documents model precedence and native resume behavior in its [model configuration reference](https://code.claude.com/docs/en/model-config#setting-your-model).

**Acceptance:** test Opus and Sonnet in both account directions; absent agent metadata with a known source slot; changing the selected native conversation; restart after a model change; extended-context identifiers; and an unavailable model. Verify the observed destination model, not just the launch argument.

### F03 · P1 · Delayed hooks can overwrite the current slot's model

**Evidence: reproduced.** Activity writes validate worker run identity, but settings writes do not. After `RecordWorkerActivity` ignores a callback from a replaced worker, `HandleStdin` still calls `RecordAgentSettings` and `SetRemoteSessionAgent` (`internal/checkpoint/service.go:135`, `internal/store/store.go:556`). The slot update is constrained only by slot ID.

A fixture from an old run replaced the current slot's Opus model with Sonnet. A synthetic `PostModelSwitch` carrying a child `agent_id` also changed the parent slot, because that event bypasses the agent filtering in `observeActivity`. Whether Claude emits that child-event shape in the relevant live flow remains unverified; the stale-worker failure does not depend on it.

**Recommendation:** validate account, workspace, worker run, conversation, and agent scope before accepting any slot metadata. Make the validation and write atomic. Preserve historical conversation metadata separately where appropriate. Do not let a rejected callback update the slot indirectly.

**Acceptance:** old-run, old-conversation, wrong-account, and child-agent fixtures leave the current slot's model, effort, and conversation unchanged. An authorized model switch still updates them.

### F04 · P1 · Fresh accounts have no complete, repeatable setup contract

**Evidence: code verified and live differences observed.** `Manager.Add` provisions an output style; the CLI helper installs checkpoint hooks after login. Launch separately provisions permissions, MCP definitions, selected workspace settings, and shared `env`/hooks. There is no versioned baseline covering the rest of the personal working environment.

`provisionPermissions` accepts only permission rules from ordinary global settings and only `env`/hooks from `settings.shared.json` (`internal/claude/permissions.go:30`). It does not propagate model/effort defaults, plugin configuration, status-line settings, or other preferences. Current isolated profile directories lack the `CLAUDE.md`, `commands`, and `agents` entries present in the normal Claude directory. All three currently contain skills and plugins, but this repository's account-creation path does not provision those collections, so their current presence does not prove a future account will receive them.

Authentication status also does not distinguish “signed in” from “the expected tools and instructions are ready.”

**Recommendation:** define an explicit, versioned manifest of shared instructions, skills/commands, agent definitions, plugin registrations, safe settings, and required MCP capabilities. Reconcile it idempotently for new and existing profiles while preserving account-specific preferences. Keep credentials, OAuth state, native history, and account caches isolated. Check the effective instruction/tool inventory before labeling a new profile ready. Do not copy the entire Claude configuration directory.

Keep cross-account project memory as an explicit product decision. A checkpoint transfers eight captured turns and bounded Git context; it does not transfer all native history or all profile-local memory. Explain that boundary in the handoff preview.

**Acceptance:** an empty temporary profile receives the required baseline, repeated setup makes no further changes, an upgrade repairs drift, and account-specific settings survive. A missing required integration produces a precise readiness failure.

### F05 · P1 · Workspace environment can override the selected account isolation

**Evidence: reproduced.** `profileEnv` filters credential/provider overrides and assigns `CLAUDE_CONFIG_DIR`. `workerEnv` then merges the complete direnv export over that environment (`internal/claude/manager.go:948`). It restores the Vibe Remote identity variables afterward, but does not restore the selected config directory or repeat the credential filtering.

A fake direnv export successfully replaced `CLAUDE_CONFIG_DIR` and reintroduced `ANTHROPIC_API_KEY`, despite the earlier host-environment filter. The dashboard can therefore label one account while the worker uses a different profile or authentication route. No real workspace was shown to be doing this during the audit.

**Recommendation:** enforce reserved identity, config-directory, authentication, and provider variables after all environment layers, including shared session settings. Continue allowing project credential variables needed by MCP integrations. Define any supported provider override as an explicit mode rather than an incidental environment merge.

**Acceptance:** conflicting direnv/shared-settings fixtures cannot replace the selected profile or authentication mode; required project MCP credentials still resolve. Never log variable values.

### F06 · P1 · Dashboard polling ignores the configured interval after each swap

**Evidence: reproduced and live DOM consistent with it.** `refresh` writes `data-refreshed-at` onto the old element, then replaces that element with server-rendered `outerHTML` (`internal/server/static/app.js:38`). The replacement has no timestamp. The next one-second timer tick sees zero and immediately refreshes it again.

The Node harness made five requests in five ticks for a region configured for 20 seconds; without replacing the element it made one. Live refreshed regions had no retained timestamp. Home's 60-second alerts region has the same defect, and its endpoint performs the heavier snapshot with checkpoint reads (`internal/server/ui.go:311`).

This creates avoidable network traffic, database reads, HTML allocation, and DOM churn. It is a credible contributor to the reported resource problem, but it does not prove the earlier browser-memory spike was a persistent heap leak. Requests also lack a per-region in-flight guard, allowing overlap on slow connections.

**Recommendation:** keep scheduling state independently of replaced DOM nodes, prevent overlapping requests for a region, abort obsolete requests on navigation, and retain the existing hidden-tab pause. Skip unchanged swaps where practical. Fix this before considering a dashboard framework rewrite.

**Acceptance:** a 20-second region remains at that cadence across many swaps; the 60-second alerts region does likewise; a slow response never creates parallel requests; hidden tabs stop polling; navigation cannot apply a stale response to a later screen.

### F07 · P1 · A newly connected session can show Running without gaining its live link

**Evidence: code verified.** Cold startup is asynchronous. The initial session detail can render while `Running` is false and has no live URL. Periodic refresh updates only `session-activity`, plus the separate handoff section when necessary (`internal/server/templates/partials/session.html`, `internal/server/ui.go:327`). It does not update the live-session link, approval-console availability, action buttons, error text, process facts, or conversation label.

Consequently the heading can reach Running while the page still lacks “Open in Claude.” A supervised restart can also leave the old remote URL on the page. The top-bar Refresh follows the same partial refresh behavior.

**Recommendation:** refresh a coherent connection/action region whenever readiness, run ID, URL, or failure state changes. Preserve an open console and pending confirmation deliberately rather than replacing them blindly. Report “Starting” until bridge registration is confirmed, and keep connection readiness separate from model/tool readiness.

**Acceptance:** remain on a fresh account's session page through slow registration, failure, retry, and supervised restart. Links and controls become correct without leaving the page, and stale links disappear.

### F08 · P2 · Browser history can persist material intended to be transient

**Evidence: code verified.** The bundled htmx version defaults to ten history entries in `localStorage`. Navigation uses `hx-push-url`; the layout neither disables history snapshots nor excludes console pages. The bundled history implementation clones the page DOM before storing it.

This persists session metadata independently of HTTP `Cache-Control: no-store`. When the terminal is open, its rendered DOM, including its screen-reader representation, can also be included. This weakens the stated in-memory-only console behavior and adds serialization/allocation on navigation. It is bounded history caching, not evidence of unbounded growth.

**Recommendation:** disable persistent HTML snapshots for this private operational dashboard, or explicitly exclude sensitive screens and remove terminal-generated DOM before snapshots. Handle history restoration and terminal disposal explicitly. Preserve ordinary Back navigation through server fetches. htmx documents [disabling history snapshots](https://htmx.org/docs/#history) for this purpose.

**Acceptance:** navigate away from a fake terminal containing a unique fixture marker; no persistent browser entry contains the marker. Back restores a working page with exactly one active terminal and poller.

### F09 · P2 · Idle workers repeatedly query SQLite, while finished activity accumulates

**Evidence: code verified and live counts observed.** Every managed worker starts a computer-use approver that ticks every 200 ms (`internal/claude/computer_use.go:85`). Each tick queries activity even when no computer-use request exists. Five live workers therefore imply roughly 25 activity reads per second before dashboard traffic. At inspection, all 2,148 stored activity items were finished.

Finished rows survive until a worker run is replaced or a conversation changes. The activity lookup has no partial index for unfinished items. This adds persistent idle work and makes long-running conversations progressively more expensive; it is not by itself proof of a large RAM leak.

**Recommendation:** wake approval handling from pending computer-use activity, or use an inexpensive cached pending flag with idle backoff. Keep bounded completion tombstones where needed for late/out-of-order events, rather than deleting them indiscriminately. Add a pending-item index if query measurements justify it.

**Acceptance:** idle sessions do not poll SQLite at five reads per second each; active approval remains responsive; duplicates and late events cannot resurrect completed activity; a long session has bounded retained activity state.

### F10 · P2 · Account setup feedback and Refresh can remain stale

**Evidence: code verified.** Add-account opens a separate asynchronous login helper, then immediately rerenders the current account list (`internal/server/ui.go:504`). The list has no refresh region. The notice promises that the account appears when login completes, but nothing updates the list at that point. The global Refresh button only visits `[data-refresh]` regions, so it does nothing on Accounts, Workspaces, Conversations, and System when those pages have none.

**Recommendation:** expose a pending setup job and its terminal result, refresh it through completion, and give Refresh an actual route-aware action on every screen. Allow retry/repair without deleting the account. Distinguish a completed login from a ready profile.

**Acceptance:** complete and cancel a delayed disposable sign-in while staying on Accounts. The page reflects either result, and Refresh works on every addressable screen.

### F11 · P1 · Mobile zoom restrictions and form labeling need an accessibility pass

**Evidence: code verified; mobile layout inspected.** The viewport disables user scaling (`internal/server/templates/layout.html:5`), and `app.js` cancels coarse-pointer gesture events. This is intentional existing behavior, but it creates an accessibility tradeoff for users needing magnification; it should not be treated as an accidental regression. The workspace-path input also has a placeholder without a dedicated associated label (`templates/screens/screens.html:54`).

**Recommendation:** permit magnification, give the path input an explicit label, and verify text resizing and keyboard focus after fragment updates. Use a targeted solution for accidental control gestures rather than a page-wide zoom restriction. Relevant accessibility requirements include WCAG 1.4.4 and 1.3.1; this audit is not a full conformance assessment.

**Acceptance:** text resizing/magnification remains usable, the path field has an accessible name, and keyboard focus survives refresh/navigation. The inspected 390 px session page had no horizontal document overflow; physical-device gestures and assistive technology were not tested.

## RAM assessment

The current evidence does not reproduce the historical large dashboard-memory spike.

- One daemon sample measured a **105.1 MB physical footprint**, with a **109.6 MB process-lifetime peak** reported by `vmmap`.
- Resident memory varied from about **42–69 MiB** across samples. This is a different measure from physical footprint.
- The daemon plus descendants had approximately **647 MiB summed RSS** in one sample, including five Claude workers and integration processes. Summed RSS double-counts shared pages and is not the browser dashboard's memory.
- Browser retained heap and the affected browser's physical footprint were not measured. An idle page at one viewport cannot establish long-duration behavior.

Keep the existing bounds: the backend terminal buffer is capped at 1 MiB per process, terminal reads at 64 KiB, frontend scrollback at 300 lines, and console setup includes disposal. These are useful safeguards and should survive the fixes.

After correcting F06, measure browser heap, detached DOM nodes, active terminal instances/timers, daemon footprint, and worker/integration footprint separately. Run a 30-minute idle test, repeated navigation/Back cycles, repeated fake-console open/close cycles, and a high-output synthetic terminal test. Compare retained memory after equivalent idle/collection points. Proposed success criteria are a stable retained-memory plateau, no accumulating terminal/timer instances, correct request rates, and no steady growth tied to navigation count. Establish numerical budgets from these measurements rather than guessing from RSS alone.

## Implementation plan

| Order | Work package | Concrete result | Verification |
| --- | --- | --- | --- |
| 1 | Protect worker/account ownership (F01, F05) | Exclusive daemon recovery; account-only authentication helpers; reserved environment boundaries enforced last | Busy disposable workers survive add/login; conflicting environment cannot reroute authentication |
| 2 | Repair model continuity (F02, F03) | One settings-resolution policy; selected-conversation settings honored; source-slot fallback; atomic run-scoped hook writes; requested/observed model visible | Cross-account matrix with known, missing, changed, and unsupported settings; delayed callback tests |
| 3 | Correct dashboard scheduling and lifecycle (F06, F07, F10) | Stable per-region timers; no overlapping/obsolete requests; working Refresh; connection controls and setup jobs update through completion | Fake clock/slow network; cold start and crash/reconnect while staying on the same page |
| 4 | Make profile setup repeatable (F04) | Versioned shared baseline, idempotent reconciliation, readiness report, repair action | Fresh empty profile and drifted profile both obtain the required tool/instruction inventory |
| 5 | Bound resource and browser persistence costs (F08, F09) | No sensitive persistent HTML cache; bounded activity state; idle approval backoff/events; lightweight private diagnostics | Fake-marker cache check; out-of-order activity cases; browser/process soak tests |
| 6 | Finish usability and operating documentation (F11) | Magnification and labels corrected; accurate setup guidance and current handoff documentation | Phone-sized layout, text resizing, keyboard pass, documentation checked against actual behavior |

Keep these as small reviewable changes rather than a system rewrite. Work packages 1–3 address the most direct failure modes first. A global default of Opus is not assumed: preserve an explicit source choice, and make the default for new/unknown work owner-controlled.

The release acceptance flow should use a disposable newly authenticated Claude profile alongside two established profiles: prepare → verify tools/instructions → continue an Opus session → observe Opus → switch model → restart → hand back. Test failures and cancellation too. Do not use the unsafe current login path against busy real workers to reproduce F01.

## Dashboard audit score

This is a provisional code/UI audit score, not a formal accessibility or performance certification.

| Dimension | Score | Main evidence |
| --- | --- | --- |
| Accessibility | 2/4 | Zoom restriction and unlabeled path field; semantic forms and visible focus styles already exist |
| Performance | 2/4 | Refresh cadence resets on swaps; lazy console creation and bounded buffers already exist |
| Responsive design | 3/4 | Inspected session fits 390 px; physical touch and enlarged-text coverage remains open |
| Theming | 3/4 | Central light/dark tokens; console deliberately uses a fixed dark palette |
| Implementation integrity | 2/4 | Readiness/control state and asynchronous account feedback can disagree |
| Total | **12/20** | Significant behavioral work remains |

**Integrity verdict:** the server-rendered screens form a coherent product-specific UI, but asynchronous state propagation needs repair. The Impeccable detector reported no findings. It could not resolve template-relative stylesheet URLs, so that output does not establish color/contrast correctness.

For the frontend work, the relevant follow-up workflows are `/impeccable optimize` for polling/resource costs, `/impeccable harden` for lifecycle/history/setup state, `/impeccable adapt` for magnification and labels, then `/impeccable polish` after the behavior is verified.

## Lower-priority maintenance

- `session_agents` currently has 216 rows with no matching checkpoint. Some can be legitimate lifecycle-only observations, so cleanup must protect live slots and allow a grace period; the table currently has no complete retention policy.
- Content-addressed Claude runtime copies have no retirement policy. Retain binaries still referenced by live workers and remove unused old versions through explicit maintenance. This concerns disk space, not an established RAM leak.
- The in-app first-use guide says workspace trust requires a local prompt, while managed launch now writes the authorized trust/onboarding state. `HANDOFF.md` also describes obsolete branch, account, and retention state. Update these after the behavior fixes so they do not misdirect new-account recovery.

## Security and validation boundaries

Preserve isolated official authentication, private filesystem permissions, the loopback listener, Tailscale exposure boundary, CSP, custom mutation header, and console run-ID checks. The account/profile reconciliation feature must share an approved configuration manifest without copying credential stores or native conversation histories. Diagnostics should contain counters, timing, version/configuration fingerprints, and model state—not credentials, environment values, prompts, SQL, or tool bodies.

Remaining live validation is deliberately limited: there was no reproduction of the user's exact historical model incident, no fresh real account sign-in, no long browser-memory soak, and no physical phone/assistive-technology pass. Those are acceptance tasks for the fixes, not evidence already obtained by this audit.


## Implementation follow-up — 26 October

The approved reliability work is implemented on `bugfix/vibe-remote-reliability`; the original findings above remain historical evidence. See [implementation progress](2026-10-26-implementation-progress.md) and [the measured resource soak](2026-10-26-resource-soak.json) for exact completion, installation, test, and acceptance status. A fresh human OAuth sign-in is still an external acceptance step. The synthetic soak does not establish that the owner's exact historical browser-memory spike has been eliminated.
