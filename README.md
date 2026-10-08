# ctxgo

A local-first command output store for AI coding agents, written in Go. No server, MCP, or external dependencies required.

> Initial scope: run commands, preserve raw stdout/stderr, and recall by ID. Filtering, indexing and session memory will come in separate changes.

## Build

Requires Go 1.22 or newer.

```sh
go build -o ctxgo .
```

## Usage

```sh
# Execute a command and return a compact summary (with the command's exit code).
ctxgo run -- go test ./...

# Stop a slow command after a duration (exit status 124).
ctxgo run --timeout 30s -- go test ./...

# Read the full original output later.
ctxgo recall RUN_ID
ctxgo recall --stream stdout RUN_ID
ctxgo recall --stream stderr RUN_ID
```

`run` does not stream the command's raw output into the agent's context; it writes separate raw streams into a run directory. The summary reports the run ID, exit code, and exact byte counts. `recall` with a single stream produces the unmodified bytes. The default `both` mode concatenates labeled streams; it does **not** reconstruct chronological interleaving.

Runs are private to the current user and stored under the OS user cache directory (`ctxgo/runs`). Override with `CTXGO_DATA_DIR` for isolation. There is no automatic retention or garbage collection yet; clean out old runs manually if disk usage grows. The run ID is a local lookup key, not an authentication credential.

On Linux (including WSL), timeout or interruption kills the spawned process group; on other platforms, only the direct child is killed. Commands run with the current user's permissions: **this is not a sandbox**. Avoid executing untrusted commands. If command startup fails, ctxgo reports an error without storing a run.

## Test

```sh
go test ./...
go vet ./...
```

## Planned follow-ups

- Semantics-aware bounded summaries and redaction policy
- Search/index using SQLite FTS5
- Session event tracking and agent hooks
