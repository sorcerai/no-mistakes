# No-Mistakes MCP Gateway Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a local stdio MCP server that exposes No-Mistakes AXI as a safe mutation gateway for external agent surfaces.

**Architecture:** Keep MCP transport and schema handling in a new isolated package. Reuse or extract typed AXI application/service boundaries rather than reimplementing pipeline logic or permanently reparsing CLI output. GitHub publication remains owned by No-Mistakes.

**Tech Stack:** Go 1.25, Cobra, existing No-Mistakes AXI/pipeline internals, MCP stdio transport.

**Spec:** `docs/superpowers/specs/2026-09-14-mcp-gateway-design.md`

## Global constraints

- No direct GitHub write implementation in the MCP package.
- No merge MCP tool in v1.
- No direct default-branch push through MCP.
- `ask-user` findings require explicit external human decision.
- Repository paths must be canonicalized and constrained to configured roots.
- stdout is reserved for MCP protocol messages; diagnostics/logging use stderr.
- Existing AXI, TUI, and agent-skill behavior must remain backward compatible.
- Tests must prove executable/public behavior, not grep source text.

---

### Task 1: Establish the typed AXI service boundary

**Files:**
- Inspect/modify: `internal/cli/axi.go`
- Inspect/modify: `internal/cli/axi_query.go`
- Inspect/modify: `internal/cli/axi_drive.go`
- Create as needed: `internal/axiapi/service.go`
- Create: `internal/axiapi/service_test.go`

**Produces:** A narrow internal interface that returns typed AXI state/results for status, run/respond, logs, sync, and doctor without depending on terminal rendering.

- [ ] Write tests around the new service boundary using existing AXI fixtures/helpers.
- [ ] Verify the tests fail before extraction.
- [ ] Extract the smallest typed service API needed by MCP while keeping current CLI output unchanged.
- [ ] Run focused AXI/service tests.
- [ ] Run existing AXI CLI tests to prove no regression.
- [ ] Commit the extraction separately before MCP protocol work.

### Task 2: Add repository authorization

**Files:**
- Create: `internal/mcp/repository_policy.go`
- Create: `internal/mcp/repository_policy_test.go`
- Modify the existing config model only where needed to expose MCP allowed roots.

**Produces:** `RepositoryPolicy` that canonicalizes a repository path, resolves symlinks, checks configured roots, and rejects traversal/escape/non-git targets before mutation tools reach AXI.

Required behavioral cases:
- allowed repository under configured root passes;
- sibling outside root fails;
- `..` traversal cannot escape;
- symlink pointing outside root fails;
- non-git directory fails;
- policy error returns a stable machine code and remediation text.

- [ ] Write failing behavioral tests for each case.
- [ ] Implement minimal policy.
- [ ] Run focused tests.
- [ ] Commit.

### Task 3: Implement normalized MCP result contracts

**Files:**
- Create: `internal/mcp/types.go`
- Create: `internal/mcp/types_test.go`

**Produces:** Stable request/response types for status, run, respond, logs, sync, doctor, findings, next actions, terminal receipts, and typed errors.

Required states include: `idle`, `running`, `awaiting_decision`, `checks-passed`, `passed`, `passed-with-skips`, `failed`, `cancelled`.

- [ ] Write serialization/contract tests for success, gate, skipped, and error envelopes.
- [ ] Ensure full SHA preservation and explicit `automatic_skips`.
- [ ] Ensure `checks-passed` is distinguishable from `passed` and `passed-with-skips`.
- [ ] Commit.

### Task 4: Implement the MCP service layer

**Files:**
- Create: `internal/mcp/service.go`
- Create: `internal/mcp/service_test.go`

**Consumes:** typed AXI service from Task 1 and repository policy from Task 2.

**Produces:** Methods backing `nomistakes_status`, `nomistakes_run`, `nomistakes_respond`, `nomistakes_logs`, `nomistakes_sync`, and `nomistakes_doctor`.

Critical policy behavior:
- `ask-user` response returns `requires_user_decision: true` and does not advance automatically;
- sync executes only when AXI's current `next_action` authorizes sync/recovery;
- nested-gate context is returned as a typed non-mutating error;
- service never maps `passed-with-skips` to CI-ready.

- [ ] Write failing tests using a fake AXI service.
- [ ] Implement status and doctor first.
- [ ] Implement run and respond with gate preservation.
- [ ] Implement logs with bounded output.
- [ ] Implement guarded sync.
- [ ] Run focused tests.
- [ ] Commit.

### Task 5: Add stdio MCP transport and tool registration

**Files:**
- Create: `internal/mcp/server.go`
- Create: `internal/mcp/server_test.go`
- Create/modify: `internal/cli/mcp.go`
- Modify the root Cobra command registration file used by the project.
- Modify `go.mod` / `go.sum` only if an MCP protocol dependency is required.

**Produces:** `no-mistakes mcp serve --stdio` and discoverable v1 tools.

- [ ] Select the maintained Go MCP implementation appropriate at execution time and pin it explicitly, or use the project's existing JSON-RPC infrastructure if one exists by then.
- [ ] Write an initialize/list-tools protocol test before implementation.
- [ ] Register exactly the v1 tools from the spec; do not add merge or abort.
- [ ] Ensure protocol bytes use stdout and all logs use stderr.
- [ ] Run protocol tests.
- [ ] Commit.

### Task 6: End-to-end MCP -> AXI fixture journey

**Files:**
- Create: `internal/mcp/integration_test.go` or the repository's established integration-test location.

**Produces:** Deterministic proof that an MCP client can inspect status, start/reattach a run, receive a gate, respond, and observe a terminal outcome without a live LLM or real GitHub mutation.

- [ ] Add an initialized fixture repository and fake/local pipeline dependencies using existing test helpers.
- [ ] Exercise status -> run -> gate -> respond -> terminal receipt.
- [ ] Add separate `ask-user` non-progression assertion.
- [ ] Add shutdown assertion proving MCP server exit does not abort the underlying run.
- [ ] Run integration test.
- [ ] Commit.

### Task 7: Document client wiring and operational safety

**Files:**
- Create: `docs/src/content/docs/guides/mcp.md` or the closest existing guide location.
- Modify docs navigation/config as required by the current documentation structure.
- Update README only if the project normally surfaces major entry points there.

**Produces:** Setup instructions for local stdio usage and an agent loop showing ChatGPT/Hermes treating No-Mistakes as the mutation gateway.

Documentation must include:
- server startup command;
- allowed-root configuration;
- tool list and read/write classification;
- gate handling policy;
- explicit no-merge behavior;
- final receipt semantics (`checks-passed` vs skipped/unverified outcomes).

- [ ] Write docs.
- [ ] Run docs build.
- [ ] Commit.

### Task 8: Full verification and No-Mistakes self-hosting pass

- [ ] Run `make test`.
- [ ] Run `make lint`.
- [ ] Run `make docs`.
- [ ] Run any MCP-specific integration target added by the implementation.
- [ ] Confirm existing `/no-mistakes` skill generation/drift checks remain green.
- [ ] Review the diff for accidental changes to existing AXI semantics.
- [ ] Gate the implementation itself through No-Mistakes before declaring the PR ready.

## Definition of done

The PR is ready when `no-mistakes mcp serve --stdio` is discoverable by an MCP client, the six v1 tools work through typed AXI state, repository authorization blocks path escape, ask-user gates cannot progress without an explicit decision, existing workflows remain green, and the final successful journey returns the No-Mistakes-created PR URL plus full head SHA without the MCP package directly calling GitHub writes.