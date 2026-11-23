#!/usr/bin/env python3
"""The parts of a VM observation that mbt-observe.py derives from a state.json.

Usage: observe-state.py MBT_OBSERVE DIR...

For each DIR, a fixture with the driver's document state.json, print one
JSON line {"dir": DIR, "observed": OBS, "errors": [...]}. OBS has the
parts "state" and "poolClaims" of a VM observation as MBT_OBSERVE derives
them from DIR/state.json; "errors" are the errors of MBT_OBSERVE.

Python 3 standard library only.
"""

import importlib.util
import json
import os
import sys

sys.dont_write_bytecode = True


def load(path):
    """The module of the observer in path."""
    spec = importlib.util.spec_from_file_location("mbt_observe", path)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


def observed(obs, d, errors):
    """The parts as the observer obs derives them from d/state.json."""
    obs.STATE_FILE = os.path.join(d, "state.json")
    doc, doc_read = obs.state_document(errors)
    return {
        "state": obs.driver_state(doc, errors),
        "poolClaims": obs.pool_claims(doc) if doc_read else None,
    }


def main():
    obs = load(sys.argv[1])
    for d in sys.argv[2:]:
        errors = []
        line = {"dir": d, "observed": observed(obs, d, errors), "errors": errors}
        print(json.dumps(line, sort_keys=True))


if __name__ == "__main__":
    main()
