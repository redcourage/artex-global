# Codex subscription quota recovery

Authorized: implement and verify in the existing modified branch. Preserve earlier compaction fixes; do not commit or restart live services.

1. Preserve `usageLimitExceeded` as a dedicated HTTP 423 error. Add authenticated quota reads. Only explicit `ordinaryUsageAllowed: true` confirms recovery; percentages and reset timestamps do not. Persist the bridge's blocked latch, without prompts or credentials.
2. Persist task-wide quota waits in PostgreSQL, keep interrupted worker intents recoverable, and prevent planner/worker admissions while waiting. Do not use API failover. Poll the original provider configuration, compare revisions, and notify the engine after confirmed recovery. User pause/stop and task deadlines retain priority.
3. Preserve transcript resume, reject uncertain tool results, and prevent automatic continuation from discarding the completed-tool replay ledger. Surface waiting in task status.
4. Verify offline protocol tests, isolated PostgreSQL persistence/CAS and scheduler tests, tool-result resume, regression suites, and a fresh code review.

Limits: no automatic replay of a standalone chat response; task planner/workers resume. External tool execution is not transactional with transcript writes, so unanswered/interrupted tool calls require human review. Older Codex versions without explicit ordinary-usage permission remain waiting.

Progress: bridge classification/latch, durable task waiting, recovery scheduler,
worker transcript safety and status display implemented. Targeted tests passed:
DB reopen + revision CAS; router no retry/failover; exact endpoint/config binding;
user pause; real Engine/Worker/Norma HTTP with one tool execution across restart;
bridge restart with duplicate tool request internally replayed.

Final verification: 89 Python tests passed; full Go agent/llmpool/db/server
packages passed using disposable PostgreSQL databases (server isolated from the
other packages); real Norma micro/full compaction HTTP fixture passed; web tsc
passed; diff whitespace check clean. No production DB or live process changed.

Independent review identified one P1: novel reads cleared the replay ledger and
could expose a previously completed write. Fixed with a recovery-wide completed
action guard and a RED→GREEN batched-read/duplicate-write regression. Ambiguous
actions now pause the intent for review rather than entering model retries.
Also verified actual fallback-member attribution, raw unanswered-tool detection,
and exactly one external fixture write across Worker/Manager restart.

The existing core lifecycle test inherited a synthetic active LLM profile from
prior tests and could wait on an external API past its three-second assertion.
Made that test explicitly exercise the raw-goal fallback and cancel its server
context; the full server suite then passed.

Ruling: user task deadlines keep their original wall-clock semantics; waiting
does not extend them. Terminal tasks are never revived.
Ruling: failed tool results require manual review during automatic worker resume,
because an interrupted write may report an error after performing its side effect.
