#!/bin/sh
set -eu

TEST_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
NODE_ROOT=$(CDPATH='' cd -- "$TEST_DIR/.." && pwd)

for script in "$NODE_ROOT"/scripts/*.sh "$NODE_ROOT"/tests/*.sh; do sh -n "$script"; done
jq empty "$NODE_ROOT"/*.schema.json "$NODE_ROOT"/templates/*.json
python3 "$TEST_DIR/test-schemas.py"
python3 "$TEST_DIR/test-template-semantics.py"
printf 'Node infrastructure contract tests passed\n'
