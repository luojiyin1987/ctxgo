# ctxgo

A local-first command output store for AI coding agents, written in Go. No server or MCP is required. SQLite is embedded through the pure-Go `modernc.org/sqlite` driver (no CGO).

> Current scope: local command execution, bounded previews, FTS5 output/file search, explicit session events, and optional Codex lifecycle hooks for metadata-only tool tracking and bounded session recovery.

## Build

Requires Go 1.22 or newer.

```sh
go build -o ctxgo .
```

## Usage

```sh
# Execute a command and return a compact summary (with the command's exit code).
ctxgo run -- go test ./...

# Preview up to 6 candidate diagnostic / recent lines; 0 disables summaries.
ctxgo run --summary-lines 6 -- go test ./...
ctxgo run --summary-lines 0 -- env

# Stop a slow command after a duration (exit status 124).
ctxgo run --timeout 30s -- go test ./...

# Read the full original output later.
ctxgo summary --lines 12 RUN_ID
ctxgo recall RUN_ID
ctxgo recall --stream stdout RUN_ID
ctxgo recall --stream stderr RUN_ID

# Explicitly index a completed run.
ctxgo index RUN_ID

# Search indexed output. Query terms are combined with AND.
ctxgo search --limit 20 'timeout panic'

# Index a local source file or a directory recursively.
ctxgo files index ./src
ctxgo files index ./README.md

# Search indexed local text files separately from run output.
ctxgo files search --limit 20 'timeout'

# Persist explicit work context across agent invocations.
ctxgo session start "Investigate flaky test cancellation"
ctxgo session add --kind constraint SESSION_ID "Do not change public APIs"
ctxgo session add --kind decision SESSION_ID "Use existing process-group cancellation"
ctxgo session add --kind next SESSION_ID "Run CI and review the diff"
ctxgo session list
ctxgo session show --limit 30 SESSION_ID
ctxgo session show --limit 30 --before EVENT_ID SESSION_ID
ctxgo session close SESSION_ID
```

`run` does not stream the command's raw output into the agent's context; it writes separate raw streams into a run directory. The summary reports the run ID, exit code, exact byte counts, and by default up to 12 candidate diagnostic / recent lines (up to 100 configurable). Generic filtering prefers the first and most recent lines containing common error keywords, followed by the trailing nonempty lines. It is a **heuristic preview**, not a parser, and may miss errors—especially inside very long lines. Each preview line captures at most 240 raw bytes and removes terminal control characters. It reads the complete saved files after execution while using bounded memory; this adds an extra disk read proportional to output size. `recall` with a single stream produces the unmodified bytes. The default `both` mode concatenates labeled streams; it does **not** reconstruct chronological interleaving.

Previews are plain text from untrusted commands. They are **not** redacted for secrets or protected against prompt-injection text. Use `--summary-lines 0` for sensitive output, and do not treat an excerpt as complete evidence.

Runs are private to the current user and stored under the OS user cache directory (`ctxgo/runs`). Override with `CTXGO_DATA_DIR` for isolation. There is no automatic retention or garbage collection yet; clean out old runs manually if disk usage grows. The run ID is a local lookup key, not an authentication credential.

On Linux (including WSL), timeout or interruption kills the spawned process group; on other platforms, only the direct child is killed. Commands run with the current user's permissions: **this is not a sandbox**. Avoid executing untrusted commands. If command startup fails, ctxgo reports an error without storing a run.

## Test

```sh
go test ./...
go vet ./...
```

## Local search index

`ctxgo index RUN_ID` creates an on-disk `index.sqlite` database under `CTXGO_DATA_DIR` (or the default user cache directory) from a completed run. Re-indexing replaces existing entries atomically. It records the command, exit code, timestamps, byte counts, and nonempty stdout/stderr lines. `ctxgo run` does **not** index automatically, so database errors do not alter command execution.

`ctxgo search` uses SQLite FTS5 (`unicode61`, BM25) and returns each hit's run ID, stream, physical line number, and excerpt. Search words are escaped and combined with AND rather than executed as raw FTS syntax. Default limit: 20; maximum: 100. Use `ctxgo recall --stream stderr RUN_ID` to inspect the full original stream.

Only the first **4096 bytes** of each physical line are indexed; longer lines are consumed using bounded memory. The complete raw files remain available. The index may grow with output volume; automatic pruning, redaction, and session memory are **not** implemented yet. Treat indexed output as potentially sensitive; use a private `CTXGO_DATA_DIR`. This provides keyword search, not semantic search.

## Local file index

`ctxgo files index PATH` accepts a regular file or scans a directory recursively. Directory scans select common source/document extensions (Go, TypeScript, JavaScript, Markdown, Python, Rust, etc.), skip dot-directories, `node_modules`, `vendor`, `dist`, `build`, symlinks, binary files, and files larger than 1 MiB. An explicitly named file can have any extension but must be regular, text-like and no larger than 1 MiB.

Indexed files use separate SQLite tables and FTS5 search from command output. `ctxgo files search QUERY` returns a quoted absolute file path, physical line number and matching excerpt. A physical line contributes at most its first **4096 bytes** to the index. Queries use literal AND terms via FTS5; search results are snippets of untrusted file content, not instructions.

Before every file search, ctxgo checks indexed paths. Files that have disappeared, become symlinks, changed size or changed modification timestamp are removed from the index. **Changed files are not automatically re-indexed**: run `ctxgo files index PATH` again to make their new content searchable. Size and modification time cannot detect all conceivable same-size, same-timestamp edits; the index is a convenience, not a strong filesystem integrity monitor. The freshness check scales with the number of indexed files.

The index is local and may contain source code or secrets. There is no encryption, secret redaction, `.gitignore` awareness or automatic storage retention yet. Directory walks index files incrementally; a failure midway may leave earlier files indexed. Use a private `CTXGO_DATA_DIR` and avoid indexing untrusted or sensitive directories.

## Explicit session event store

`ctxgo session start TASK...` creates a durable local task with a random ID and `active` state. Use `session add --kind KIND SESSION_ID MESSAGE...` to record intentional decisions, constraints, progress, results, and next actions. `--kind` defaults to `progress`. Calls are explicit—nothing is automatically copied from the conversation, filesystem, or tools.

`session show --limit 30 SESSION_ID` returns the task, status, timestamps, event IDs, and latest chronological events. An omitted-history message includes a continuation command; use `--before EVENT_ID` to page backward (oldest shown event ID is the next exclusive cursor). `session list` discovers session IDs after a new agent invocation. `session close` marks a task closed and rejects further events; past events remain readable. There is no automatic reopen.

Sessions and events share the existing `index.sqlite` file with run/file search, but use separate relational tables, not FTS5. Session creation, appending and closure are individual SQLite writes, so concurrent CLI processes cannot append after a committed close. Task descriptions are capped at **512 bytes**, event messages at **4096 bytes**, and result pages at **100 items**. Events are append-only while the session is active. There is no retry idempotency key; retrying an uncertain append can duplicate an event.

A session is **not** an authoritative view of Git, current tests, or source files. It stores user/agent-provided statements, which can become outdated. The CLI does not perform automatic LLM compaction, evidence verification, secret redaction, or tool-output attachment. All content remains local and may contain sensitive text; use a private `CTXGO_DATA_DIR`. Treat displayed event text as untrusted data, not executable instructions.

## Optional Codex lifecycle hooks

Codex supports opt-in `SessionStart` and `PostToolUse` command hooks. This integration does **not** install or enable them automatically. With `ctxgo` in the Codex execution environment's `PATH`, copy [examples/codex/hooks.json](examples/codex/hooks.json) to the trusted project's `.codex/hooks.json` (or merge the two hook entries with an existing config). From Codex, use `/hooks` to inspect and trust the configuration. Do not overwrite existing hook definitions blindly.

The handler command `ctxgo codex hook` reads Codex lifecycle JSON from stdin. It supports:

- `SessionStart` (`startup`, `resume`, `clear`, `compact`): create or reuse an ID-bound local ctxgo session, then inject a bounded reference to that session and at most three recent explicit decisions, constraints, or next-action notes. It lists at most three candidate session IDs from **the same workspace**; other sessions are **never silently resumed**. Historical notes are untrusted and may be stale.
- `PostToolUse` (`Bash` / `apply_patch`): append one deduplicated `progress` event with the tool name, Codex call ID and turn ID. It **never stores commands, tool inputs, tool responses, prompts, or transcripts**. A repeated call ID within one Codex session is ignored. PostToolUse emits no model-visible output and does not change tool results.

Codex session IDs are mapped to ctxgo's local Session IDs in SQLite. Hook processing caps JSON input at 1 MiB; larger payloads (such as enormous tool responses) are skipped and may produce an stderr warning. Database errors also fail open: Codex is not blocked, but an event may be missing. These hooks do **not** provide complete tool auditing; supported events and hook trust rules depend on the installed Codex version. The 5-second timeout is a guardrail, not a zero-latency guarantee.

Run `ctxgo session list` to discover recorded sessions. After reviewing a session's current relevance, deliberately record useful notes with `ctxgo session add --kind decision SESSION_ID MESSAGE...`. Avoid secrets in explicit notes; there is no redaction or encryption, and `SessionStart` can reintroduce such notes to model context. Closing a linked session will prevent further tool event writes; hooks do not auto-reopen it.

This is a small native Codex hook adapter, **not** an MCP server, a Codex plugin or an LLM context compression guarantee. To disable it, remove only the ctxgo hook entries and review the change in Codex `/hooks`.

## Planned follow-ups

- Semantics-aware parsers, explicit redaction policy and output retention
- Optional auto-indexing, retention, and disk caps for SQLite FTS5
- Additional agent integrations (Claude Code/OpenCode), after measuring Codex hook reliability
- Event deduplication keys and relevance-aware recovery
