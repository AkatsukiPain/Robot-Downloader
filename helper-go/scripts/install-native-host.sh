#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BIN_DIR="$ROOT_DIR/bin"
OUT_BIN="$BIN_DIR/robot-downloader-helper"
MANIFEST_DIR="$HOME/.mozilla/native-messaging-hosts"
MANIFEST_PATH="$MANIFEST_DIR/robot.downloader.json"

mkdir -p "$BIN_DIR"
mkdir -p "$MANIFEST_DIR"

pushd "$ROOT_DIR" >/dev/null
GO111MODULE=on go build -o "$OUT_BIN" ./cmd/downloader
popd >/dev/null

cat > "$MANIFEST_PATH" <<JSON
{
  "name": "robot.downloader",
  "description": "Robot Downloader native messaging host",
  "path": "$OUT_BIN",
  "type": "stdio",
  "allowed_extensions": [
    "robot-downloader@pain.local"
  ]
}
JSON

echo "Installed native host manifest to: $MANIFEST_PATH"
echo "Built helper binary at: $OUT_BIN"
