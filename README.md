# Robot Downloader

Robot Downloader is a split download system with two parts working together:

- `extension/` — a Firefox WebExtension that lives in the browser
- `helper-go/` — a native Go helper that does the real download work

The extension is intentionally lightweight. It handles browser UX, user actions, and request collection. The Go helper is the download engine: it receives jobs, downloads files in chunks, persists state on disk, resumes interrupted work, and manages files outside the browser sandbox.

## Why the project is split

A browser extension is great for:

- context menus
- popup UI
- detecting media and page context
- collecting cookies and request headers from the browser
- reacting to user interactions
- showing notifications and job state

But a browser extension is not a good place for a serious downloader engine.

The native helper is better suited for:

- long-running background work
- resumable downloads
- parallel chunk workers
- filesystem access
- persistent job state
- merging chunk files
- stronger retry and recovery logic

That split is the core design of Robot Downloader:

- **extension = control plane / browser UX**
- **helper = data plane / downloader engine**

## How extension and helper collaborate

Robot Downloader uses **Firefox Native Messaging** so the extension can talk to the local helper over stdin/stdout with JSON messages.

### End-to-end workflow

1. **The user starts a download in Firefox**
   - from the extension popup
   - from the context menu
   - from the floating video button injected by the content script
   - or via download interception when enabled

2. **The extension gathers browser-side context**
   The extension can collect things the native helper cannot easily see by itself:
   - target URL
   - filename hint
   - page URL
   - cookies for the target domain
   - `User-Agent`
   - `Referer`
   - `Origin`
   - `Accept-Language`
   - user settings like max connections, chunk size, and retry count

3. **The extension sends a JSON request to the helper**
   In `extension/background.js`, the extension opens a native messaging port with:
   - helper name: `robot.downloader`

   Then it sends commands like:
   - `enqueue`
   - `list`
   - `open-location`
   - `resume-job`
   - `cancel-job`
   - `remove-job`

4. **The helper receives the message and executes it**
   In `helper-go/cmd/downloader/main.go`, the helper:
   - reads native messages from stdin
   - decodes JSON payloads
   - dispatches by `type`
   - runs the downloader service
   - writes a JSON response back to stdout

5. **The helper manages the actual download lifecycle**
   The Go service then:
   - probes the URL
   - checks range support
   - creates a chunk plan
   - downloads pieces concurrently
   - saves job metadata and partial chunk progress
   - resumes unfinished jobs after restart
   - merges finished chunks into the final file

6. **The extension shows the result to the user**
   The popup and notifications layer can then:
   - show job state
   - refresh the job list
   - let the user open the output location
   - resume or cancel failed/active jobs
   - remove finished or broken jobs

## Architecture overview

```text
Firefox page / media
        │
        ▼
content-script.js
  (detects media, shows action button)
        │
        ▼
background.js
  (builds request context, talks to helper)
        │
        ▼
Firefox Native Messaging
        │
        ▼
helper-go/cmd/downloader/main.go
        │
        ▼
internal/downloader + storage + jobstate
        │
        ├── ~/.robot-downloader/jobs/
        ├── ~/.robot-downloader/chunks/
        └── ~/.robot-downloader/downloads/
```

## Repository structure

```text
robot-downloader/
├── README.md
├── extension/
│   ├── manifest.json
│   ├── background.js
│   ├── content-script.js
│   ├── popup.html
│   ├── popup.css
│   ├── popup.js
│   └── assets/
│       └── icon.svg
└── helper-go/
    ├── README.md
    ├── go.mod
    ├── cmd/
    │   └── downloader/
    │       └── main.go
    ├── internal/
    │   ├── api/
    │   │   └── messages.go
    │   ├── config/
    │   │   └── config.go
    │   ├── downloader/
    │   │   └── downloader.go
    │   ├── jobstate/
    │   │   └── job.go
    │   └── storage/
    │       └── jobs.go
    ├── packaging/
    │   └── native-host.manifest.template.json
    └── scripts/
        └── install-native-host.sh
```

## Components

## Extension side

### `extension/manifest.json`
Defines the Firefox extension:
- permissions
- background script
- content script
- popup
- native messaging permission
- Gecko extension ID

Current extension ID:
- `robot-downloader@pain.local`

### `extension/background.js`
This is the browser-side coordinator.

Current responsibilities include:
- loading and saving settings
- connecting to the native helper
- serializing helper requests
- collecting cookies and browser headers
- building enqueue payloads
- creating context menu actions
- listing jobs
- opening output location
- resuming/canceling/removing jobs
- showing notifications when the helper is unavailable

This file should stay orchestration-focused. It should not become a downloader engine.

### `extension/content-script.js`
This script detects video elements in web pages and shows a floating:
- **Download with Robot Downloader**

button on hovered videos.

When clicked, it sends an `enqueue-job` message back to the background script with:
- direct media URL
- guessed filename
- page/frame context

### `extension/popup.js`
The popup is the main management UI inside Firefox.

Current responsibilities include:
- rendering job summaries
- filtering/searching jobs
- showing progress, speed, ETA, and status
- saving settings
- opening file locations
- resuming failed jobs
- canceling active jobs
- removing jobs
- toggling theme

## Helper side

### `helper-go/cmd/downloader/main.go`
The native helper entrypoint.

Responsibilities:
- load helper config
- ensure storage directories exist
- resume incomplete jobs on startup
- read native messages from stdin
- handle supported command types
- write JSON responses to stdout

### `helper-go/internal/api/messages.go`
Defines the message contract between extension and helper.

That keeps the protocol explicit and stable.

Current request/response shapes cover:
- enqueueing downloads
- listing jobs
- job actions using `jobId`
- job summary metadata returned to the popup

### `helper-go/internal/config/config.go`
Defines where helper state is stored.

Current root:
- `~/.robot-downloader/`

Subdirectories:
- `jobs/` — persisted job metadata
- `chunks/` — partial chunk files
- `downloads/` — final output files

### `helper-go/internal/jobstate/job.go`
Defines the persisted job model used for resumability.

This includes fields such as:
- job ID
- URL
- filename
- status
- output path
- total bytes
- downloaded bytes
- chunk layout
- range support
- last error
- timestamps

### `helper-go/internal/storage/jobs.go`
Reads and writes job metadata to disk as JSON.

That persistence layer is what allows recovery after helper restarts.

### `helper-go/internal/downloader/downloader.go`
The real download engine.

Current responsibilities include:
- URL probing
- `HEAD` / range fallback behavior
- chunk planning
- concurrent chunk download workers
- retry behavior
- progress persistence
- resume support
- final chunk merge
- open-location / cancel / cleanup behavior

## What works now

- Firefox extension connects to the helper through Native Messaging
- page context, cookies, and browser headers can be forwarded to the helper
- downloads can be enqueued from browser-side actions
- helper persists jobs to disk
- helper downloads in chunks concurrently
- helper resumes unfinished jobs on startup
- helper merges completed chunks into final output
- popup UI can list and manage jobs
- content script can surface direct video downloads from page media elements

## Current message flow example

Example enqueue request sent by the extension:

```json
{
  "type": "enqueue",
  "url": "https://example.com/video.mp4",
  "filename": "video.mp4",
  "options": {
    "maxConnections": 8,
    "chunkSizeBytes": 8388608,
    "retryCount": 3
  },
  "context": {
    "headers": {
      "User-Agent": "...",
      "Cookie": "session=...",
      "Referer": "https://example.com/page"
    },
    "pageUrl": "https://example.com/page"
  }
}
```

Typical helper response:

```json
{
  "ok": true,
  "type": "enqueue",
  "jobId": "job_123",
  "status": "queued",
  "message": "queued video.mp4"
}
```

## Installation

## Requirements

- **Firefox** for the extension
- **Go 1.23+** for building the helper
- a Linux/macOS environment for the provided native-host install path/script
- permission to install a Firefox native messaging manifest in your home directory

## 1) Build and install the native helper

From the project root:

```bash
cd helper-go
./scripts/install-native-host.sh
```

What this does:
- builds the helper binary at `helper-go/bin/robot-downloader-helper`
- installs a Firefox native messaging manifest at:
  - `~/.mozilla/native-messaging-hosts/robot.downloader.json`

## 2) Load the Firefox extension

Temporary development install:

1. Open Firefox
2. Go to `about:debugging`
3. Open **This Firefox**
4. Click **Load Temporary Add-on**
5. Select `extension/manifest.json`

## 3) Test the workflow

Suggested test flow:
- load the extension
- confirm the helper is installed
- open the popup
- queue a test download
- verify jobs appear in the popup
- verify output files are written under `~/.robot-downloader/downloads/`

## Supported platforms

### Currently targeted

- **Firefox** extension frontend
- **Linux** helper install flow
- **Go native helper** using Firefox Native Messaging

### Likely portable with adjustments

- macOS
- other Unix-like systems

### Not currently targeted in this repo

- Chromium/Chrome extension packaging
- Windows native host installation

## Development notes

### Why Go for the helper

Go is a strong fit here because the helper needs:
- concurrent workers
- reliable filesystem IO
- long-running background execution
- a simple distributable binary
- low-overhead resume/retry logic

### Why keep the extension thin

The browser extension should remain the UX layer.

That keeps responsibilities clean:
- browser integration in JavaScript
- download engine in Go
- protocol in a small JSON contract

This makes the project easier to maintain as features grow.

## Security notes

The extension can collect cookies and request headers to help reproduce browser-authenticated downloads. That is powerful and should be treated carefully.

Recommended precautions:
- keep Native Messaging host access limited to your extension ID
- do not expose the helper as a network service unless you add authentication
- be careful when logging request headers
- avoid storing sensitive headers longer than needed

## Roadmap ideas

- live progress events from helper to popup
- pause/resume/cancel protocol expansion
- integrity verification / checksums
- better filename detection
- packaged Firefox install flow
- Windows native-host installer
- download history cleanup policy
- standalone desktop UI later, if needed

## License

Add a license before publishing publicly.

If you want a simple default, MIT is a good choice.
