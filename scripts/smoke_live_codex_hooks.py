#!/usr/bin/env python3
"""Check the live harness gate without starting Codex."""

from pathlib import Path
import subprocess
import sys


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
    print("live harness gate OK")


if __name__ == "__main__":
    main()
