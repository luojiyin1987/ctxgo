#!/usr/bin/env python3
"""Check the live harness gate without starting Codex."""

from pathlib import Path
import json
import sqlite3
import subprocess
import sys
import tempfile

sys.path.insert(0, str(Path(__file__).resolve().parent))
import live_codex_hooks as harness


SCRIPT = Path(__file__).with_name("live_codex_hooks.py")


def main():
    help_result = subprocess.run([sys.executable, str(SCRIPT), "--help"],
                                 capture_output=True, text=True)
    if help_result.returncode != 0 or "--run-live" not in help_result.stdout:
        raise SystemExit("help output is missing the live gate")
    gated = subprocess.run([sys.executable, str(SCRIPT), "--model", "test-model"],
                          capture_output=True, text=True)
    if gated.returncode == 0 or "pass --run-live" not in gated.stderr:
        raise SystemExit("the harness ran without --run-live")
    with tempfile.TemporaryDirectory() as temporary:
        root = Path(temporary)
        data = root / "data"
        data.mkdir()
        calls = [{"event": "SessionStart", "source": "startup", "session_id": "thread"}]
        for name, tool_id in (("Bash", "call_1"), ("Bash", "call_2"),
                              ("apply_patch", "call_3")):
            calls.append({"event": "PostToolUse", "tool": name,
                          "tool_use_id": tool_id, "session_id": "thread",
                          "exit_code": 0, "stderr": ""})
        calls.append(calls[1].copy())
        (root / "hook-calls.jsonl").write_text(
            "".join(json.dumps(item) + "\n" for item in calls))
        db = sqlite3.connect(data / "index.sqlite")
        db.executescript("""CREATE TABLE codex_bindings (codex_id TEXT, session_id TEXT);
            CREATE TABLE codex_tool_uses (codex_id TEXT, tool_use_id TEXT);
            CREATE TABLE session_events (session_id TEXT, kind TEXT, text TEXT);
            INSERT INTO codex_bindings VALUES ('thread', 'local');""")
        for name, tool_id in (("Bash", "call_1"), ("Bash", "call_2"),
                              ("apply_patch", "call_3")):
            db.execute("INSERT INTO codex_tool_uses VALUES (?,?)", ("thread", tool_id))
            db.execute("INSERT INTO session_events VALUES (?,?,?)", (
                "local", "progress",
                f"Codex PostToolUse tool={name} tool_use_id={tool_id} turn=turn_1"))
        db.commit()
        harness.assert_hooks(root, "thread", "startup", ["Bash", "apply_patch"])
        harness.assert_recorded_tool_calls(root, data, "thread")
        db.execute("INSERT INTO session_events VALUES (?,?,?)", (
            "local", "progress",
            "Codex PostToolUse tool=Bash tool_use_id=call_1 turn=turn_1"))
        db.commit()
        db.close()
        try:
            harness.assert_recorded_tool_calls(root, data, "thread")
        except RuntimeError:
            pass
        else:
            raise SystemExit("the harness accepted a duplicate SQLite event")
    print("live harness gate OK")


if __name__ == "__main__":
    main()
