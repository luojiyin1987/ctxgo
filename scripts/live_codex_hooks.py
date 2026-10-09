#!/usr/bin/env python3
"""Run an explicit Codex hook check in an isolated WSL project."""

import argparse
import json
import os
from pathlib import Path
import re
import shlex
import shutil
import sqlite3
import subprocess
import sys
import tempfile


REPO = Path(__file__).resolve().parents[1]
DECISION = "Keep the public API unchanged."
CONSTRAINT = "Check current files before acting."
MODEL_NAME = re.compile(r"^[A-Za-z0-9._-]+$")


def read_jsonl(text):
    return [json.loads(line) for line in text.splitlines() if line.startswith("{")]


def require(condition, message):
    if not condition:
        raise RuntimeError(message)


def write_wrapper(root):
    target = root / "bin" / "ctxgo"
    target.write_text(
        """#!/usr/bin/env python3
import json
import os
from pathlib import Path
import subprocess
import sys
import time

root = Path(os.environ["CTXGO_E2E_ROOT"])
payload = sys.stdin.buffer.read()
start = time.perf_counter_ns()
result = subprocess.run([str(root / "bin" / "ctxgo-real"), "codex", "hook"],
                        input=payload, capture_output=True)
elapsed_ms = round((time.perf_counter_ns() - start) / 1_000_000, 3)
try:
    event = json.loads(payload)
except json.JSONDecodeError:
    event = {}
record = {"event": event.get("hook_event_name"), "source": event.get("source"),
          "tool": event.get("tool_name"), "tool_use_id": event.get("tool_use_id"),
          "session_id": event.get("session_id"), "elapsed_ms": elapsed_ms,
          "exit_code": result.returncode, "stderr": result.stderr.decode(errors="replace")[:500]}
if record["event"] == "SessionStart" and result.stdout:
    record["context"] = json.loads(result.stdout)["hookSpecificOutput"]["additionalContext"]
with (root / "hook-calls.jsonl").open("a") as log:
    log.write(json.dumps(record) + "\\n")
sys.stdout.buffer.write(result.stdout)
sys.stderr.buffer.write(result.stderr)
sys.exit(result.returncode)
"""
    )
    target.chmod(0o700)


def prepare(root, model):
    project = root / "project"
    home = root / "codex-home"
    data = root / "data"
    for path in (root / "bin", project / ".codex", home, data):
        path.mkdir(parents=True)
    subprocess.run(["git", "init", "-q", str(project)], check=True)
    shutil.copyfile(REPO / "examples/codex/hooks.json", project / ".codex" / "hooks.json")
    real_home = Path.home() / ".codex"
    require((real_home / "auth.json").is_file(), "Codex auth.json is missing")
    (home / "auth.json").symlink_to(real_home / "auth.json")
    if (real_home / "models_cache.json").is_file():
        (home / "models_cache.json").symlink_to(real_home / "models_cache.json")
    home.joinpath("config.toml").write_text(
        f"model = {json.dumps(model)}\n"
        f"[projects.{json.dumps(str(project))}]\ntrust_level = \"trusted\"\n"
        "[features]\nhooks = true\nplugins = false\napps = false\n"
    )
    write_wrapper(root)
    subprocess.run(["go", "build", "-o", str(root / "bin" / "ctxgo-real"), "."],
                   cwd=REPO, check=True, timeout=120)
    return project, home, data


def run_codex(root, project, home, data_dir, label, args, timeout):
    command = "cd " + shlex.quote(str(project)) + " && codex-fast " + shlex.join(args)
    env = os.environ.copy()
    env.update({"CODEX_HOME": str(home), "CTXGO_DATA_DIR": str(data_dir),
                "CTXGO_E2E_ROOT": str(root),
                "PATH": str(root / "bin") + os.pathsep + env.get("PATH", "")})
    result = subprocess.run(["zsh", "-lic", command], env=env,
                            input="", text=True, capture_output=True, timeout=timeout)
    (root / f"{label}.stdout.jsonl").write_text(result.stdout)
    (root / f"{label}.stderr.log").write_text(result.stderr)
    events = read_jsonl(result.stdout)
    require(result.returncode == 0, f"{label} failed; see {label}.stderr.log")
    require(any(item.get("type") == "turn.completed" for item in events),
            f"{label} did not complete a turn")
    usage = next(item["usage"] for item in reversed(events)
                 if item.get("type") == "turn.completed")
    tokens = usage.get("input_tokens", 0) + usage.get("output_tokens", 0)
    return events, tokens


def tool_items(events, kind):
    return [item["item"] for item in events if item.get("type") == "item.completed"
            and item.get("item", {}).get("type") == kind]


def calls_for(root, codex_id):
    log = root / "hook-calls.jsonl"
    return [item for item in read_jsonl(log.read_text())
            if item.get("session_id") == codex_id]


def assert_hooks(root, codex_id, source, tools):
    calls = calls_for(root, codex_id)
    require(sum(call.get("source") == source for call in calls) == 1,
            f"expected one {source} SessionStart hook")
    for name in tools:
        require(sum(call.get("tool") == name for call in calls) == 1,
                f"expected one {name} PostToolUse hook")
    return calls


def run_live(root, args):
    home = root / "codex-home"
    try:
        return run_checks(root, args)
    finally:
        (home / "auth.json").unlink(missing_ok=True)


def run_checks(root, args):
    project, home, data = prepare(root, args.model)
    budget = args.token_budget
    count = 0

    def model_call(label, data_dir, command):
        nonlocal budget, count
        require(count < args.max_model_calls, "model call limit reached")
        events, tokens = run_codex(root, project, home, data_dir, label, command,
                                   args.timeout_seconds)
        count += 1
        budget -= tokens
        require(budget >= 0, "token budget exceeded after a model call")
        return events

    start = model_call("start", data, ["exec", "--json", "--dangerously-bypass-hook-trust",
            "Run pwd with Bash. Then use apply_patch to create probe.txt with one line: integration probe. Reply OK."])
    codex_id = next(item["thread_id"] for item in start if item.get("type") == "thread.started")
    require(any(item.get("exit_code") == 0 and str(project) in item.get("aggregated_output", "")
                for item in tool_items(start, "command_execution")), "Bash did not run")
    require(any(item.get("status") == "completed" for item in tool_items(start, "file_change")),
            "apply_patch did not run")
    require((project / "probe.txt").read_text() == "integration probe\n",
            "apply_patch wrote unexpected content")
    assert_hooks(root, codex_id, "startup", ["Bash", "apply_patch"])

    db = sqlite3.connect(data / "index.sqlite")
    local_id = db.execute("SELECT session_id FROM codex_bindings WHERE codex_id=?",
                          (codex_id,)).fetchone()[0]
    db.close()
    for kind, note in (("decision", DECISION), ("decision", DECISION),
                       ("constraint", CONSTRAINT)):
        subprocess.run([str(root / "bin" / "ctxgo-real"), "session", "add",
                        "--kind", kind, local_id, note],
                       env={**os.environ, "CTXGO_DATA_DIR": str(data)},
                       check=True, capture_output=True, text=True)

    for label in ("resume-one", "resume-two"):
        resumed = model_call(label, data, ["exec", "resume", "--json",
            "--dangerously-bypass-hook-trust", codex_id,
            "State the recovered decision exactly. Do not call tools."])
        require(any(DECISION in item.get("text", "")
                    for item in tool_items(resumed, "agent_message")),
                f"{label} did not use the recovered decision")
        calls = calls_for(root, codex_id)
        context = [item["context"] for item in calls
                   if item.get("source") == "resume"][-1]
        require(context.count(DECISION) == 1 and CONSTRAINT in context,
                f"{label} lost or duplicated a note")
        require("decision #" in context and " UTC" in context and
                "Previous notes may be stale" in context,
                f"{label} omitted note provenance")

    db = sqlite3.connect(data / "index.sqlite")
    require(db.execute("SELECT count(*) FROM codex_bindings WHERE codex_id=?",
                       (codex_id,)).fetchone()[0] == 1, "resume created another binding")
    db.close()

    bad_data = root / "bad-data-path"
    bad_data.write_text("not a directory")
    failed = model_call("database-failure", bad_data, ["exec", "--json",
        "--dangerously-bypass-hook-trust", "Run pwd with Bash. Reply with the directory path."])
    failed_id = next(item["thread_id"] for item in failed
                     if item.get("type") == "thread.started")
    require(any(item.get("exit_code") == 0 and str(project) in item.get("aggregated_output", "")
                for item in tool_items(failed, "command_execution")),
            "SQLite failure blocked Bash")
    failed_calls = assert_hooks(root, failed_id, "startup", ["Bash"])
    require(all("hook skipped" in item["stderr"] and item["exit_code"] == 0
                for item in failed_calls), "SQLite failure did not fail open")

    return {"status": "passed", "model_calls": count,
            "tokens_used": args.token_budget - budget,
            "codex_session_id": codex_id, "ctxgo_session_id": local_id,
            "hooks": read_jsonl((root / "hook-calls.jsonl").read_text())}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--run-live", action="store_true", help="Allow four real model calls")
    parser.add_argument("--model", help="Codex model to use for the live check")
    parser.add_argument("--max-model-calls", type=int, default=4)
    parser.add_argument("--timeout-seconds", type=int, default=120)
    parser.add_argument("--token-budget", type=int, default=150000)
    parser.add_argument("--output-dir", type=Path, help="New directory for test artifacts")
    args = parser.parse_args()
    if not args.run_live:
        parser.error("pass --run-live to allow real model calls")
    if not args.model or not MODEL_NAME.fullmatch(args.model):
        parser.error("pass a model name with --model")
    if args.max_model_calls < 4 or args.timeout_seconds < 1 or args.token_budget < 1:
        parser.error("four calls, a positive timeout, and a positive token budget are required")
    root = args.output_dir or Path(tempfile.mkdtemp(prefix="ctxgo-codex-e2e-"))
    if args.output_dir:
        root.mkdir(mode=0o700)
    root.chmod(0o700)
    try:
        result = run_live(root, args)
    except Exception as exc:
        result = {"status": "failed", "error": str(exc)}
    (root / "summary.json").write_text(json.dumps(result, indent=2) + "\n")
    print(root / "summary.json")
    return 0 if result["status"] == "passed" else 1


if __name__ == "__main__":
    sys.exit(main())
