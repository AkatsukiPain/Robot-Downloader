# helper-go

Native Go downloader engine for Robot Downloader.

## What it does

- receives download jobs from the Firefox extension over Native Messaging
- probes URLs
- plans chunks
- downloads files concurrently
- persists chunk/job state on disk
- resumes incomplete downloads on startup
- merges finished chunks into final files

## Install Native Messaging host (Firefox)

From this directory:

```bash
./scripts/install-native-host.sh
```

What it does:
- builds the helper binary into `helper-go/bin/robot-downloader-helper`
- installs the Firefox native messaging manifest into:
  - `~/.mozilla/native-messaging-hosts/robot.downloader.json`

## Storage paths

The helper stores data under:

- `~/.robot-downloader/jobs/`
- `~/.robot-downloader/chunks/`
- `~/.robot-downloader/downloads/`

## Current protocol

Incoming messages:
- `enqueue`
- `list`

Planned next protocol actions:
- `pause`
- `resume`
- `cancel`
- `status`

## Build manually

```bash
go build ./...
```
