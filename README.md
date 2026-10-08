# ctxgo

A local-first command output store for AI coding agents, written in Go. No server or MCP is required. SQLite is embedded through the pure-Go `modernc.org/sqlite` driver (no CGO).

> Current scope: run commands, preserve raw stdout/stderr, show bounded previews, recall by ID, and explicitly index both completed runs and local text files for FTS5 search. Session memory will come in separate changes.

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

## Planned follow-ups

- Semantics-aware parsers, explicit redaction policy and output retention
- Optional auto-indexing, retention, and disk caps for SQLite FTS5
- Session event tracking and agent hooks
