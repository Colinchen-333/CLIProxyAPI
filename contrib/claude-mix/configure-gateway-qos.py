#!/usr/bin/env python3
"""Classify the existing user-facing HTTP gateway as interactive on macOS.

This preserves all other plist fields. Reload the service through launchd after
running this helper; it deliberately does not signal or restart any process.
"""
import os
from pathlib import Path
import plistlib
import tempfile


def configure(path: Path) -> bool:
    original = path.read_bytes()
    data = plistlib.loads(original)
    if data.get("Label") != "ai.lumirain.claudex-cliproxy":
        raise ValueError("refusing to modify a different launchd service")
    if data.get("ProcessType") == "Interactive":
        return False
    data["ProcessType"] = "Interactive"
    fd, temporary = tempfile.mkstemp(prefix=path.name + ".", dir=path.parent)
    try:
        with os.fdopen(fd, "wb") as output:
            output.write(plistlib.dumps(data))
        os.chmod(temporary, path.stat().st_mode & 0o777)
        os.replace(temporary, path)
    finally:
        if os.path.exists(temporary):
            os.unlink(temporary)
    return True


if __name__ == "__main__":
    path = Path.home() / "Library/LaunchAgents/ai.lumirain.claudex-cliproxy.plist"
    print("updated" if configure(path) else "already interactive")
