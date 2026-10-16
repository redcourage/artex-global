# Codex subscription task integration probe

> Execution: implement and run the user-authorized local probe in this session.

**Goal:** Complete one real ARTEX task using Codex subscription inference for goals, planner and workers, with the existing Go engine, PostgreSQL stores, tool execution and guard intact.

**Architecture:** A local experimental Chat Completions bridge passes ARTEX prompts and tools to `codex app-server`. Codex tool requests return to ARTEX as function calls; only ARTEX executes those tools. Subsequent requests deliver tool results back into the pending Codex turn. Session headers separate roles/intents. This is a compatibility probe, not a production provider or a long-duration reliability claim.

**Tech stack:** Python standard library, installed Codex CLI, existing Go server, disposable PostgreSQL 16 container.

**Spec:** The approved in-chat flow: create task → Codex Planner creates intents → existing engine dispatches Codex Workers → real tools persist facts → planner verifies completion. Exercise multiple turns and different role models.

## Constraints

- Keep the existing API implementation unchanged.
- Run only against local generated fixtures and a disposable database.
- Use existing ChatGPT authentication without exporting its tokens.
- Bind all probe listeners to loopback; require a local bridge token.
- Do not push, merge, or modify existing Docker services.
- Label the bridge experimental; support non-streaming requests only initially.
- Report failures and unsupported recovery/compaction behavior explicitly.

## Review focus

- Tool-result IDs must match pending Codex requests, including multiple calls.
- Session IDs must not mix distinct agents or jobs.
- A reset planner history must create a fresh Codex context.
- Unknown or changed tool catalogs must not silently execute unauthorized tools.
- An interrupted HTTP request must not leave inference running indefinitely.

## Tasks

1. [x] Add bridge protocol tests; observe the missing implementation fail. Implement message/tool conversion, request correlation, session boundaries, authentication and bounded cancellation. Run Python tests.
2. [x] Build the unmodified ARTEX binary; start an isolated database, fixture server, bridge and ARTEX. Configure session headers and separate planner/worker models through the real API. Submit one bounded local task.
3. [x] Verify the actual database/API contains planner-created intents, worker tool activity, facts derived from unpredictable fixture markers, multiple planner rounds and terminal goal completion. Save redacted evidence, stop probe processes/container, and review changes.

## Limitations to retain in the result

This does not certify streaming, arbitrary payloads, automatic API fallback, crash-mid-tool replay, natural context exhaustion, or hours-long operation. The bridge must reject unsupported inputs rather than claim compatibility.

## Execution record

- Branch: `feature/codex-cli-backend`. Original Go implementation unchanged.
- Task 1: complete. Initial protocol tests failed without the implementation, then passed.
- Task 2: complete. Built the current Go server and ran a task through its actual HTTP API and PostgreSQL stores.
- Final independent review found three lifecycle issues: cancellation before turn-start acknowledgement, completed-turn ID resurrection, and runner SIGTERM/orphan cleanup.
- Final: fixed cancellation-before-ack — delayed start-ack regression RED→GREEN.
- Final: fixed completion ordering — second-user-turn regression RED→GREEN.
- Final: fixed SIGTERM and orphan cleanup — real subprocess regressions RED→GREEN.
- Python suite: 14 tests passed after the fixes. Live artifacts remain local, outside tracked source.
- Task 3: complete. Actual integration passed all eight checks before and after the review fixes. Examined activity records also confirm distinct fetch workers and a follow-up worker reading both source facts. Owned services were stopped. Latest evidence is preserved under ignored `test-results/codex-probe/2026-10-16/`.

## Subsequent authorized metering work (2026-10-16)

The user subsequently requested token tracking and model-specific API-equivalent
costs including cache savings. This expands the original probe scope: Go transport,
ledger, API aggregation and the task overview UI now include metering changes.
Recovery, retention and long-duration findings are investigated in
[the review report](../reviews/2026-10-16-codex-bridge.md); they remain separate from
the implemented accounting feature.

- Added per-inference Codex usage IDs, retry deduplication, API usage detail capture,
  cost snapshots, exact-model pricing configuration, and missing-usage coverage.
- First metered live run completed the task and matched token totals but passed
  only 11/12 checks: tool-call notifications preceded usage notifications, creating
  false missing-usage rows. Accounting now waits for the terminal turn response.
- Review regressions also cover missing usage on later turns, partial API counters,
  stream read-ahead, and preserved reported usage on failed responses.
- Final live run: **12/12 checks passed in 78.2 seconds**; three intents, two fixture
  requests, goal met, zero activity errors, 24 priced inferences, zero missing or
  unpriced ledger rows. Codex final totals exactly match the database counters.
- Model totals: `gpt-6.1-sol` input 245831 / output 1406 / cache read 147200;
  `gpt-6-luna` input 213231 / output 824 / cache read 161664.
- Standard API-equivalent estimate **$0.23322734**; without-cache baseline
  **$0.52745710**; cache read savings **$0.29422976**; reported cache write premium
  **$0**. These are estimates, not subscription charges.
- Validation: full `go test ./...`, focused transport race tests, disposable
  PostgreSQL ledger tests (not skipped), Python 20 tests, Vitest 15 tests, Node
  calculation tests 6, TypeScript, i18n consistency, and changed-file Biome error
  checks passed. Existing lint warnings remain outside the new cost components.
- Final evidence is local and ignored under
  `test-results/codex-probe/2026-10-16-metering/`. Runner stopped its services and
  disposable database. No commit or push performed.
