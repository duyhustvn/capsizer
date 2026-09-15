#!/usr/bin/env python3
"""Runner wrapper for ramps/python/ramp.py."""

from __future__ import annotations

import sys
from pathlib import Path

# Đảm bảo đường dẫn gốc và ramps/python có trong sys.path
_root = Path(__file__).resolve().parent
_py_pkg = _root / "ramps" / "python"

for p in (str(_root), str(_py_pkg)):
    if p not in sys.path:
        sys.path.insert(0, p)

from ramps.python import ramp as _ramp

main = _ramp.main
main_async = _ramp.main_async
build_scenario = _ramp.build_scenario
Scenario = _ramp.Scenario
ChatSSEScenario = _ramp.ChatSSEScenario
RestScenario = _ramp.RestScenario

__all__ = [
    "main",
    "main_async",
    "build_scenario",
    "Scenario",
    "ChatSSEScenario",
    "RestScenario",
]

if __name__ == "__main__":
    raise SystemExit(main())
