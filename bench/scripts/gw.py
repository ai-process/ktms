#!/usr/bin/env python3
"""Reads gateways.json for the shell scripts: `gw.py names` or `gw.py NAME FIELD`.

Strings print raw, everything else as JSON; missing fields print the documented default.
"""
import json
import sys
from pathlib import Path

DEFAULTS = {"ready": "/health", "model": "gpt-4o-mini", "headers": {}, "prepare": "", "deps": [],
            "path": "/v1/chat/completions", "auth": True}

gateways = json.loads((Path(__file__).resolve().parent.parent / "gateways.json").read_text())
if sys.argv[1:] == ["names"]:
    print(" ".join(gateways))
    sys.exit(0)
name, field = sys.argv[1], sys.argv[2]
if name not in gateways:
    sys.exit(f"unknown gateway {name}; known: {' '.join(gateways)}")
v = gateways[name].get(field, DEFAULTS.get(field))
if v is None:
    sys.exit(f"gateway {name} has no field {field}")
print(" ".join(v) if field == "deps" else v if isinstance(v, (str, int)) else json.dumps(v, separators=(",", ":")))
