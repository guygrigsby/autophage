#!/usr/bin/env bash
# Print the contracts document's SQL blocks, in order, as the goose migration
# body. The migration file is generated, never hand-edited; the store test
# fails when the two drift.
set -euo pipefail
doc="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/docs/specs/2026-09-04-autophage-contracts.md"
echo "-- +goose Up"
awk '/^```sql$/{f=1; next} /^```$/{if(f){print ""}; f=0; next} f' "$doc"
