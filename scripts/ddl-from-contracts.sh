#!/usr/bin/env bash
# Print the contracts document's SQL blocks, in order, as the goose migration
# body. The migration file is generated, never hand-edited; the store test
# fails when the two drift.
set -euo pipefail
doc="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/docs/specs/2026-09-04-autophage-contracts.md"
echo "-- +goose Up"
echo "-- The migration set is up-only by policy: no Down block anywhere in it."
echo "-- autophage rolls forward. Every table here is an append-only record of"
echo "-- what happened, so a Down that dropped one would delete the history the"
echo "-- aggregates are rebuilt from, and a schema change that needs the old"
echo "-- shape back gets a new numbered migration that puts it back."
echo
awk '/^```sql$/{f=1; next} /^```$/{if(f){print ""}; f=0; next} f' "$doc"
