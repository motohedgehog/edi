# Repository guidance

## Locate the change

- `cmd/edi`: flags, signals, constructor wiring.
- `internal/config`: configuration loading and validation.
- `internal/app`: lifecycle and manual dependency construction.
- `internal/monitor`: windows, comparisons, alert creation, delivery scheduling.
- `internal/store`: SQLite access and migrations.
- `internal/telegram`: TfL persona and Telebot adapter.
- `internal/tfl`: HTTP requests, response validation, normalization.
- `internal/message`: message formats and chunks; `internal/logging`: safe diagnostics.

Dependencies flow toward domain behavior; domain packages do not import Telebot or SQL drivers. Define small interfaces in consuming packages. Use manual dependency injection; constructors must not start goroutines.

Before changing a contract, trace its callers, implementations, tests, and serialized representations. Update affected uses together. Prefer available semantic navigation for symbols; search text for configuration keys, tags, and other references outside the call graph.

## Make the change

Reuse existing patterns and dependencies. Keep functions focused and names consistent with the domain. Prefer a small interface that hides complexity; introduce abstractions for actual variation. Keep refactoring separate from behavior changes and preserve validation and error handling when moving code.

Return expected failures as errors, preserve useful error chains, and inspect them with `errors.Is` or `errors.As`. Give each failure one reporting owner. Close resources on every path and preserve failures that affect durability. Initialize maps before writes, avoid typed-nil errors, and copy mutable collections when callers must not share their storage.

Pass caller contexts through I/O; release derived contexts. Bound external calls and background work. Every goroutine needs an owner, a termination path, and a way to wait for completion. Protect shared mutable state and make channel closure ownership explicit.

For consequential design choices, compare viable alternatives against the actual constraints. Record durable decisions with context, the choice, rejected alternatives, and consequences. Explain why in plain English; preserve facts, uncertainty, identifiers, and obligations when editing documentation.

## Test the behavior

Build test-driven vertical slices: one failing behavioral test, then enough implementation to pass it. Assert observable contracts with independently derived expectations. Use fakes at external boundaries, controlled clocks, and real temporary file-backed SQLite databases. Tests stay offline and independent of execution order. Exercise relevant invalid-input, failure, cancellation, and cleanup paths. Use explicit synchronization instead of timing guesses; parallelize tests only when their state is isolated.

## Protect invariants

Write succinct log messages in sentence case explaining what happened. Keep operation names, outcomes, counts, and permitted identifiers in structured fields. Use `logging.Error` for complete error descriptions. Configure credential redaction through the shared handler replacement; configuration errors must be safe before that handler exists.

Numeric private-chat and user IDs may be logged only at DEBUG; never attach them to shared loggers that emit higher levels. Runtime errors, paths, and request URLs may be logged with the active Telegram token and TfL key redacted, including their query-escaped and path-escaped forms. Avoid deliberate logging of incoming or outgoing message bodies or raw upstream payloads. Keep recovered panic diagnostics application-controlled. Prefer environment variables for secrets; operators protect configuration files and logs.

Use parameterized SQL and context-aware calls. Keep related writes atomic, close rows, and check iteration errors. Preserve the single-connection SQLite setup. Keep database, lock, and SQLite side files owner-only; preserve existing directory permissions and the one-process advisory lock. Released Goose numbered migrations are immutable: add a migration for schema changes. Goose records alone determine schema version. Test migrations against existing data as well as an empty database.

Keep configuration loads isolated. When adding a setting, update bindings, validation, precedence tests, and user documentation together. An explicitly requested configuration file must fail visibly if unreadable or invalid.

The only persona is `tfl`, with command root `/tfl` and callback prefix `tfl:`. Add a general extension framework only when a second persona exists.

## Verify and finish

Run `make check` for the offline lint gate and `make test` for Go changes. Add `make test-race` for concurrency changes and `make container` for container changes. Inspect results; distinguish passed checks from checks not run.

Use Conventional Commits: `type(scope): description`, with optional scope. Report the outcome, verification, and remaining limitations succinctly.

## Load additional guidance

- Reviewing a diff, branch, PR, or proposed design: read [review.md](docs/agents/review.md).
- Diagnosing a failure, flaky test, race, or performance problem: read [debugging.md](docs/agents/debugging.md).
- Changing dependencies, lint/build configuration, containers, CI, or releases: read [tooling.md](docs/agents/tooling.md).
