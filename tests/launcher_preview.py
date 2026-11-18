#!/usr/bin/env python3
"""Check the thin launcher's directory, stdio, environment, and exit behavior."""
import os
from pathlib import Path
import subprocess
import tempfile

ROOT = Path(__file__).resolve().parents[1]


def main():
    with tempfile.TemporaryDirectory(prefix='forge-launcher-preview-') as directory:
        directory = Path(directory)
        launcher = directory / 'main.ts'
        launcher.write_bytes((ROOT / 'main.ts').read_bytes())
        (directory / 'package.json').write_text('{"type":"module"}')
        outside = directory / 'outside'
        outside.mkdir()
        missing = subprocess.run(['node', str(launcher)], cwd=outside, env=dict(os.environ, NODE_NO_WARNINGS='1'), capture_output=True, timeout=5)
        assert missing.returncode != 0
        binary = directory / 'forge'
        binary.write_text('#!/bin/sh\nprintf "%s" "$FORGE_LAUNCHER_TEST"\nprintf "child stderr" >&2\nsleep 0.1\nexit 37\n')
        binary.chmod(0o755)
        result = subprocess.run(['node', str(launcher)], cwd=outside, env=dict(os.environ, FORGE_LAUNCHER_TEST='child stdout', NODE_NO_WARNINGS='1'), capture_output=True, timeout=5)
        assert result.returncode == 37, result
        assert result.stdout == b'child stdout' and result.stderr == b'child stderr', result
        print('Launcher: 4 checks passed (relative binary, inherited environment/stdio, child exit, spawn failure)')


if __name__ == '__main__':
    main()
