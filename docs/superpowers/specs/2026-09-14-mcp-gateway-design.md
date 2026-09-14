# No-Mistakes MCP Gateway Design

## Goal

Expose No-Mistakes as a small, machine-stable MCP gateway so ChatGPT, Hermes, Muse, Codex, Claude, or another agent surface can safely hand off repository mutation work without depending on direct GitHub write primitives.

The MCP server is an orchestration adapter. It must not reimplement No-Mistakes, git publishing, PR creation, CI handling, or merge logic. No-Mistakes remains the mutation and validation authority.

## Why

Agent surfaces can usually inspect repositories reliably, while write actions may be unavailable, permission-gated, or flaky. No-Mistakes already owns the desired delivery pipeline:

`intent -> rebase -> review -> test -> document -> lint -> push -> PR -> CI`

It also exposes the headless `no-mistakes axi` interface with structured machine output and explicit approval/fix gates. The missing piece is a stable MCP boundary around that interface.

## Architecture

```text
Agent surface
   |
   v
No-Mistakes MCP server
   |
   v
No-Mistakes AXI
   |
   +--> disposable worktree
   +--> review / test / docs / lint
   +--> safe fixes + ask-user gates
   +--> push / PR / CI
   |
   v
GitHub
```

The MCP layer should stay thin and deterministic. Each tool maps to one No-Mistakes control operation, normalizes AXI state to JSON, and preserves No-Mistakes ownership of state transitions.

## V1 tool surface

### `nomistakes_status`
Read-only. Return current AXI home/status for a repository and optional run ID.

### `nomistakes_run`
Write-capable orchestration action. Start or reattach to a run.

Required input:

```json
{
  "repo_path": "/absolute/path/to/repo",
  "intent": "Exact user goal, constraints, exclusions, acceptance criteria, and material decisions.",
  "skip": [],
  "wait_seconds": 480
}
```

Rules:
- `intent` is mandatory and must not be lossily summarized.
- If AXI returns a gate, return that gate and stop.
- If AXI returns a terminal state, return the final receipt.

### `nomistakes_respond`
Write-capable action for an active gate.

```json
{
  "repo_path": "/absolute/path/to/repo",
  "run_id": "01...",
  "action": "approve|fix|skip",
  "finding_ids": ["finding-1"],
  "instructions": "optional guidance",
  "wait_seconds": 480
}
```

Policy:
- `auto-fix`: agent may request fix when mechanical and within approved intent.
- `no-op`: agent may approve.
- `ask-user`: must return `requires_user_decision: true`; never auto-approve or auto-fix without an explicit user decision.
- `skip`: always surfaced as consequential in receipts.

### `nomistakes_logs`
Read-only. Bounded run/step logs.

### `nomistakes_sync`
Write-capable. Only allowed when AXI reports the matching structured sync/recovery next action. Reject speculative sync calls.

### `nomistakes_doctor`
Read-only diagnostic wrapper.

### Deferred
`nomistakes_abort` is intentionally excluded from v1 unless implementation proves a real need. Abort is easy to misuse mid-run and No-Mistakes treats it as a between-runs action.

## Normalized response contract

```json
{
  "ok": true,
  "operation": "run",
  "repo_path": "/absolute/path/to/repo",
  "run_id": "01...",
  "branch": "feat/example",
  "head_sha": "full-sha",
  "state": "awaiting_decision",
  "step": "review",
  "pr_url": null,
  "ci": null,
  "requires_user_decision": true,
  "findings": [
    {
      "id": "r1",
      "severity": "blocking",
      "action": "ask-user",
      "file": "path/file.go",
      "description": "..."
    }
  ],
  "next_action": {
    "code": "respond",
    "allowed": ["approve", "fix", "skip"]
  },
  "automatic_skips": [],
  "warnings": []
}
```

Errors should be typed and actionable:

```json
{
  "ok": false,
  "error": {
    "code": "repo_not_allowed",
    "message": "Repository path is outside configured roots.",
    "remediation": "Add the canonical repository root to mcp.allowed_repo_roots."
  }
}
```

## Repository authorization

The server must not accept arbitrary filesystem paths by default.

Requirements:
- canonicalize `repo_path` including symlink resolution;
- require it to live under a configured allowed root;
- reject traversal and symlink escapes;
- reject non-git repositories;
- reject mutation calls for repositories not initialized for No-Mistakes, while preserving No-Mistakes remediation guidance;
- never accept shell fragments, command strings, or arbitrary environment injection as MCP input.

Suggested config:

```yaml
mcp:
  enabled: true
  transport: stdio
  allowed_repo_roots:
    - /Users/aria/src
    - /srv/repos
```

## Transport

V1 should be stdio MCP:

```bash
no-mistakes mcp serve --stdio
```

Keep protocol handling isolated from AXI invocation so streamable HTTP can be added later without changing tool semantics.

## Internal boundary

Preferred layering:

```text
MCP tool handler
  -> internal MCP service
    -> shared AXI application/service API
      -> pipeline state / daemon
```

Do not permanently shell out to `no-mistakes axi` and scrape human-readable output if typed internal AXI results are available. A subprocess adapter is acceptable only as a spike or compatibility fallback.

## State and safety invariants

1. No-Mistakes owns pipeline state.
2. AXI is the source of truth.
3. While a run is active, callers do not directly edit the pipeline worktree.
4. No MCP tool directly performs GitHub mutation in v1; publishing stays inside No-Mistakes.
5. No merge tool in v1.
6. Never push directly to the default branch through MCP.
7. Never auto-resolve an `ask-user` finding.
8. Never use abort as a recovery shortcut for an active gate.
9. Never execute caller-provided shell text.
10. Never mutate under nested-gate context.
11. Never hide skipped validation steps.
12. Never report `checks-passed` unless AXI reports it.
13. Never claim CI is green from `passed`, `passed-with-skips`, or another unverified state.
14. Preserve full commit SHAs in machine-facing receipts.

## Agent loop

```text
1. status
2. run(intent)
3. if gate:
   - auto-fix -> agent may request fix
   - no-op -> agent may approve
   - ask-user -> return decision to human
4. repeat until terminal outcome
5. return PR URL + full head SHA + CI/outcome
```

The protocol should favor explicit state over autonomous guessing.

## Testing

Tests must assert behavior through public interfaces, not grep implementation source.

Required coverage:
- MCP initialize/list-tools succeeds;
- schemas reject missing required fields;
- status works against idle repository;
- run starts against an initialized fixture and returns run identity;
- gate responses preserve `auto-fix`, `no-op`, and `ask-user` action classifications;
- `ask-user` cannot silently progress through policy code;
- sync refuses unless AXI advertises an allowed sync/recovery transition;
- repository allowlist rejects traversal and symlink escape;
- nested-gate context becomes a typed non-mutating error;
- `checks-passed` carries PR URL and exact head SHA when available;
- `passed-with-skips` exposes automatic skips and cannot masquerade as CI-ready;
- stopping the MCP process does not abort active No-Mistakes runs.

Use a fake/in-memory AXI service for protocol tests plus a small integration fixture for MCP -> AXI behavior. Deterministic CI must not require a live LLM or real GitHub write.

## Observability

For stdio, logs go to stderr only. stdout remains reserved for MCP protocol messages.

Structured fields should include tool name, safe repo identifier, run ID, step, duration, and outcome/error code. Never log secrets, arbitrary environment values, or repository contents.

## Upstream compatibility

`sorcerai/no-mistakes` is a fork. Keep this isolated enough to absorb upstream changes:
- prefer new `internal/mcp` package(s);
- narrow CLI registration changes;
- extract shared typed AXI service boundaries only where necessary;
- avoid broad pipeline rewrites.

## Acceptance criteria

1. `no-mistakes mcp serve --stdio` starts a standards-compliant MCP server.
2. MCP clients can discover the v1 tools.
3. A client can inspect status, start/reattach a run, respond to gates, fetch logs, execute authorized sync, and run doctor.
4. `ask-user` findings cannot progress without explicit external decision.
5. Repository authorization prevents writes outside configured roots.
6. No MCP tool directly writes GitHub or merges PRs.
7. A real fixture journey can reach a No-Mistakes-created PR and return its URL plus full head SHA.
8. Existing AXI/TUI/agent workflows remain backward-compatible.
9. Unit and integration tests pass under normal repository test commands.
10. Documentation shows ChatGPT/Hermes using MCP as the mutation gateway.

## Future extensions

After v1 only:
- streamable HTTP transport with explicit auth;
- webhook/event subscriptions;
- richer multi-repository orchestration in Hermes;
- trusted policy profiles for safe auto-fix;
- carefully designed non-destructive cancellation if No-Mistakes semantics support it.
