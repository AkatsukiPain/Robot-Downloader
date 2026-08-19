# helper-go

Native Go downloader engine for Robot Downloader.

## What it does

- receives download jobs from the Firefox extension over Native Messaging
- probes URLs
- extracts supported page-based media when needed (currently YouTube via `yt-dlp`)
- plans chunks
- downloads files concurrently
- persists chunk/job state on disk
- resumes incomplete downloads on startup
- merges finished chunks into final files

## Requirements

- Go 1.23+
- `yt-dlp` for supported page-based extractors such as YouTube
- `ffmpeg` for manifest/stream downloads and some extractor-backed flows

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
- `open-location`
- `resume-job`
- `cancel-job`
- `remove-job`

The helper can now return clearer failures for unsupported page types and for extractor failures such as missing `yt-dlp`.

## Build manually

```bash
go build ./...
```
