# ctxgo

A local-first command output store for AI coding agents, written in Go. No server or MCP is required. SQLite is embedded through the pure-Go `modernc.org/sqlite` driver (no CGO).

> Current scope: local command execution, bounded previews, FTS5 output/file search, explicit session events, and optional Codex lifecycle hooks for metadata-only tool tracking and bounded session recovery.

## Install

Building from source requires **Go 1.22 or newer**. The repository root is the installable Go command (no CGO or external SQLite installation is required).

Download a ready-to-run archive from [GitHub Releases](https://github.com/luojiyin1987/ctxgo/releases). Packages cover Linux, macOS, and Windows on amd64 and arm64. Each archive includes `ctxgo`, `README.md`, and `LICENSE`. Check the archive against `SHA256SUMS` before use.

**WSL / Linux / macOS** — install the executable from the Go module:

```sh
go install github.com/luojiyin1987/ctxgo@latest

# Go installs commands into GOBIN, or GOPATH/bin if GOBIN is unset.
go_bin="$(go env GOBIN)"
if [ -z "$go_bin" ]; then go_bin="$(go env GOPATH)/bin"; fi
export PATH="$go_bin:$PATH"

command -v ctxgo
ctxgo help
```

Put the same `export PATH=...` setting in your shell startup file (`~/.bashrc` or `~/.zshrc`) so future terminals can find `ctxgo`. If you use a non-default `GOBIN`, use that directory instead of assuming `~/go/bin`. To update an installation, rerun `go install ...@latest`.

**Build from a checkout** (useful when testing unreleased changes):

```sh
git clone https://github.com/luojiyin1987/ctxgo.git
cd ctxgo
go build -o ctxgo .
./ctxgo help
```

For **VSCode Remote WSL**, install and configure `ctxgo` **inside the same WSL distribution where the coding agent runs**. Installing a Windows executable does not automatically make it available to a Linux Codex process. If an IDE-launched agent cannot see your updated shell `PATH`, restart the remote extension/terminal or configure the Hook with the **absolute Linux path** from `command -v ctxgo`.

The installed binary does **not** include this repository's `examples/` files; download or copy the Hook example separately as described below.

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
bash scripts/smoke-package-release.sh
```

The release workflow builds packages when a `vX.Y.Z` tag reaches GitHub. It tests the tagged source before release. It also has a manual action for an existing tag. The manual action uses the workflow from the selected branch and builds the tagged source. To make packages locally, run `scripts/package-release.sh --help`.

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

## Codex: native lifecycle Hook integration

This is the **only built-in automatic agent integration** at present. Codex can invoke `ctxgo codex hook` on `SessionStart` and `PostToolUse`; there is no MCP server and no automatic installation.

**Step 1 — Install and check the executable in Codex's environment.** Follow [Install](#install) above, then run `command -v ctxgo` **in WSL** if Codex is running through WSL. The Hook subprocess must see that same executable and user data directory.

**Step 2 — Add the project Hook configuration.** From a project directory you trust:

```sh
cd /path/to/your/project
mkdir -p .codex

# Only download a new file when the project has no hooks.json yet.
# If one exists, merge its existing "hooks" entries with this example instead.
if [ -e .codex/hooks.json ]; then
  echo "Existing .codex/hooks.json: merge the ctxgo entries manually" >&2
else
  curl -fsSL \
    https://raw.githubusercontent.com/luojiyin1987/ctxgo/main/examples/codex/hooks.json \
    -o .codex/hooks.json
fi
```

The complete reference configuration is [examples/codex/hooks.json](examples/codex/hooks.json). Inspect its contents before trusting it. **Do not replace** an existing project's Hooks or add a duplicate JSON `hooks` key; merge the `SessionStart` and `PostToolUse` entries into the existing object.

**Step 3 — Trust and verify.** In Codex, open `/hooks` to inspect and approve the Hook definition, then **start a new session**. The configuration calls `ctxgo codex hook`, so the executable must be on the `PATH` visible to the Codex process. If your VSCode/WSL agent uses a different `PATH`, replace each `"command": "ctxgo codex hook"` with `"command": "/absolute/linux/path/to/ctxgo codex hook"` (substitute the actual result of `command -v ctxgo`), then review/trust the updated configuration.

**Step 4 — Check local persistence and record a useful note.** After a Codex session starts and uses a supported tool, run:

```sh
ctxgo session list
ctxgo session show SESSION_ID
ctxgo session add --kind decision SESSION_ID "Keep the public API unchanged"
```

Replace `SESSION_ID` with the ctxgo ID reported by `session list` or the SessionStart context, **not** the Codex thread ID. Resume that Codex session and inspect the injected historical note. Auto-captured tool events contain **only metadata**: you must explicitly add decisions, constraints, and next actions. The default storage is per OS user; if you set `CTXGO_DATA_DIR`, use the **same value** for Codex Hooks and your manual `ctxgo session` commands or they will see different databases.

The handler command `ctxgo codex hook` reads Codex lifecycle JSON from stdin. It supports:

- `SessionStart` (`startup`, `resume`, `clear`, `compact`): create or reuse an ID-bound local ctxgo session. It injects at most three distinct recent decisions, constraints, or next-action notes. Each note shows its event ID and UTC creation time. The handler checks at most 30 recent eligible events and keeps the newest copy of identical notes. It reserves one slot for the newest constraint in that set. Constraints beyond those 30 events can be omitted. It lists at most three candidate session IDs from **the same workspace**. Other sessions are **never silently resumed**. Historical notes are untrusted and may be stale. Each resume can receive the notes again.
- `PostToolUse` (`Bash` / `apply_patch`): append one deduplicated `progress` event with the tool name, Codex call ID and turn ID. It **never stores commands, tool inputs, tool responses, prompts, or transcripts**. A repeated call ID within one Codex session is ignored. PostToolUse emits no model-visible output and does not change tool results.

SessionStart skips historical notes with invalid timestamps. Valid notes remain eligible for recovery.

Codex session IDs are mapped to ctxgo's local Session IDs in SQLite. Hook processing caps JSON input at 1 MiB; larger payloads (such as enormous tool responses) are skipped and may produce an stderr warning. Database errors also fail open: Codex is not blocked, but an event may be missing. These hooks do **not** provide complete tool auditing; supported events and hook trust rules depend on the installed Codex version. The 5-second timeout is a guardrail, not a zero-latency guarantee.

Run `ctxgo session list` to discover recorded sessions. After reviewing a session's current relevance, deliberately record useful notes with `ctxgo session add --kind decision SESSION_ID MESSAGE...`. Avoid secrets in explicit notes; there is no redaction or encryption, and `SessionStart` can reintroduce such notes to model context. Closing a linked session will prevent further tool event writes; hooks do not auto-reopen it.

This is a small native Codex hook adapter, **not** an MCP server, a Codex plugin or an LLM context compression guarantee. To disable it, remove only the ctxgo hook entries and review the change in Codex `/hooks`.

### Live Codex check

The live check uses `codex-fast` from the WSL login shell. It makes four model calls in a private temporary project. It tests startup, two resumes, Bash, `apply_patch`, and a database failure. It writes `summary.json` and hook timing records in the output directory. The script removes its temporary auth link after the check.

Run it only when you want to spend model tokens:

```bash
python3 scripts/live_codex_hooks.py --run-live --model MODEL --max-model-calls 4 --timeout-seconds 120 --token-budget 150000
```

The token budget stops later calls when an earlier call uses too many tokens. A single call can exceed the remaining budget. CI runs `python3 scripts/smoke_live_codex_hooks.py` without model calls. CI does not run the live check.
The script stops the Codex process group when a model call reaches its timeout.

## Other coding agents: CLI integration (no native Hooks yet)

**Claude Code, OpenCode, and other agents are not automatically integrated by this project.** The [Codex Hooks JSON](examples/codex/hooks.json) uses Codex's event schema; do **not** copy it into another agent and expect its Hook callbacks or JSON to match. In particular, `ctxgo codex hook` is **not** a generic stdin protocol, MCP tool, or universal agent adapter.

Any coding agent that can **execute shell commands** can still use `ctxgo` immediately. It can invoke the CLI via its shell tool, or you can add the following guidance to that agent's project instructions (for example, an `AGENTS.md` if the agent supports it, or its own project rules):

> When a command is expected to produce large output, prefer `ctxgo run -- COMMAND ARG...`. This saves stdout/stderr locally and returns a bounded summary and run ID. The command's exit code remains significant; this is not a streaming terminal.
>
> Use `ctxgo summary --lines 12 RUN_ID` or `ctxgo recall --stream stderr RUN_ID` for more evidence, and `ctxgo index RUN_ID` followed by `ctxgo search QUERY` when searching saved command output. Use `ctxgo files index PATH` and `ctxgo files search QUERY` only when an explicit local file index is wanted.
>
> At the start of a task or resumed conversation, use `ctxgo session list` and `ctxgo session show SESSION_ID` to inspect relevant previous notes. To create a task, run `ctxgo session start "TASK"`. Record durable decisions/constraints/next steps with `ctxgo session add --kind KIND SESSION_ID "MESSAGE"`. Treat stored notes as historical, not verified current facts. Do not put secrets into stored notes.

**Minimal manual example** (run inside the same shell environment as the agent):

```sh
ctxgo run -- go test ./...
# Copy RUN_ID from the result if further diagnostics are needed.
ctxgo recall --stream stderr RUN_ID
ctxgo index RUN_ID
ctxgo search 'failed'

ctxgo session start "Investigate test failures"
# Copy SESSION_ID from the output. Use it again in the next invocation.
ctxgo session add --kind next SESSION_ID "Inspect the failure and rerun tests"
ctxgo session show SESSION_ID
```

Manual CLI integration **does not automatically capture another agent's tool calls or inject context on resume**. Native adapters for those agents require separate, agent-specific Hook/API implementations. Agents without a shell tool cannot use this CLI directly.

## Planned follow-ups

- Semantics-aware parsers, explicit redaction policy and output retention
- Optional auto-indexing, retention, and disk caps for SQLite FTS5
- Additional agent integrations (Claude Code/OpenCode), after measuring Codex hook reliability
- Event deduplication keys and relevance-aware recovery
