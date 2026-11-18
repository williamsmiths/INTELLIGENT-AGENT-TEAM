#!/usr/bin/env bash
# Build a public copy of the committed tree for the official repository, without private data.
# Usage: deploy/export-public.sh <dest-dir> [commit]   (dest must not exist)
# Excluded: hidden oracles, scenario and retrieval-fixture data (product rules of real projects), eval results,
# working plans and reports. The scan fails the export on anything that looks like a secret.
set -euo pipefail
dest=${1:?dest dir}; rev=${2:-HEAD}
[ ! -e "$dest" ] || { echo "dest exists: $dest" >&2; exit 2; }
root=$(git rev-parse --show-toplevel)
mkdir -p "$dest"
git -C "$root" archive "$rev" | tar -x -C "$dest"
rm -rf "$dest/evals/abc/oracle" "$dest/evals/abc/scenarios.json" "$dest/evals/abc/setup_abc.py" "$dest/evals/team" "$dest/.work" "$dest/plans" "$dest/infra/.env" "$dest/deploy/team/"*.local.json "$dest/evals/retrieval/fixture.json" "$dest/evals/retrieval/report.json" "$dest/evals/retrieval/runs"
find "$dest" -name '__pycache__' -prune -exec rm -rf {} +
# Secret scan: real-looking keys, tokens, DSNs with passwords, private keys.
if grep -rIlE '(gh[pousr]_[A-Za-z0-9]{30,}|sk-[A-Za-z0-9_-]{30,}|AKIA[0-9A-Z]{16}|-----BEGIN [A-Z ]*PRIVATE KEY-----|postgres://[^:/ ]+:[^@<$ {]{6,}@)' "$dest" \
     | grep -vE '_test\.go$'; then
  echo "secret-like content found; export aborted" >&2; exit 1
fi
echo "exported $rev to $dest"
