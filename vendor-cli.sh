#!/usr/bin/env bash
# vendor-cli.sh — cross-compile the clinical-trials CLI to a linux/amd64 binary
# at bin/clinical-trials-pp-cli-linux, which the Dockerfile copies into the image.
#
# USAGE (from WEB_DIR, in Git Bash), monorepo on the feat/clinical-trials branch:
#   ./vendor-cli.sh
#   ./vendor-cli.sh "/c/Users/LACI/Desktop/printing-press-library/library/health/clinical-trials"
set -euo pipefail
CLI_SRC="${1:-/c/Users/LACI/Desktop/printing-press-library/library/health/clinical-trials}"
OUT="bin/clinical-trials-pp-cli-linux"
if [ ! -f "$CLI_SRC/go.mod" ] || [ ! -d "$CLI_SRC/cmd" ]; then
  echo "ERROR: CLI source not found at: $CLI_SRC (check out the feat/clinical-trials branch)" >&2
  exit 1
fi
echo "Vendoring from: $CLI_SRC"
rm -rf cli-src && mkdir -p cli-src
cp "$CLI_SRC/go.mod" "$CLI_SRC/go.sum" cli-src/
cp -r "$CLI_SRC/cmd" "$CLI_SRC/internal" cli-src/
echo "Cross-compiling -> $OUT"
mkdir -p bin
( cd cli-src && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o "../$OUT" ./cmd/clinical-trials-pp-cli )
file "$OUT"; ls -la "$OUT"
