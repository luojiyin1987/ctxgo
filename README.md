# ctxgo

A local-first command output store for AI coding agents, written in Go. No server, MCP, or external dependencies required.

> Current scope: run commands, preserve raw stdout/stderr, show bounded filtered previews, and recall by ID. Indexing and session memory will come in separate changes.

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

## Planned follow-ups

- Semantics-aware parsers, explicit redaction policy and output retention
- Search/index using SQLite FTS5
- Session event tracking and agent hooks
