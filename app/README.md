# Ollama for macOS and Windows

## Download

- [macOS](https://github.com/ollama/app/releases/download/latest/Ollama.dmg)
- [Windows](https://github.com/ollama/app/releases/download/latest/OllamaSetup.exe)

## Browser pane

A third pane on the right shows a live view of a tab in your own Chromium
browser, and hands tasks to the [WebBrain](https://webbrain.one) extension
running there. It is hidden until you open it with the toolbar button, and
opening it is what makes the app attach to a browser.

The pane streams frames over the Chrome DevTools Protocol and forwards clicks,
scrolling, and typing back. It does not embed a browser of its own: the pages
you see are running in your real, already-signed-in session.

### Setup

1. Quit Chrome completely, then relaunch it with remote debugging:

   ```sh
   open -a "Google Chrome" --args --remote-debugging-port=9222
   ```

   Any Chromium browser works — Edge, Brave, Vivaldi. Firefox does not: it has
   neither CDP nor the offscreen document WebBrain's bridge needs.

2. Open the pane and press **Connect**. Set a non-default port under
   `BrowserDebugPort` in settings if 9222 is taken.

3. For the WebBrain half, install the extension and point its bridge at the
   app: **WebBrain → Settings → General → Advanced → Cloud bridge**, set the URL
   to `ws://127.0.0.1:17374/extension` and enable it.

The pane works as a viewer without step 3; only the Ask/Act bar needs it.

### Notes

- **The bridge is exclusive.** The extension holds one outbound bridge socket at
  a time, so while it is pointed here it is not pointed at WebBrain Cloud
  (`17373`) or the LM Studio plugin (`17375`).
- **Ask vs Act.** Ask is read-only. Act can click, type, and submit, and is
  gated by WebBrain's own in-browser approval. Anything other than an explicit
  Act runs as Ask.
- **First connect is slow.** The app launches the bridge with
  `npx -y @webbrain/mcp-server`, which downloads the package on first use.

## Development

### Desktop App

```bash
go generate ./... &&
go run ./cmd/app
```

### Browser pane

`app/browser` is a standalone package: a minimal RFC 6455 client (`ws.go`), a
CDP client (`cdp.go`), a stdio MCP client for webbrain-mcp (`mcp.go`), and the
`Manager` that owns both connections. Its tests run against in-process fakes,
plus an opt-in test against a real browser:

```bash
open -a "Google Chrome" --args --remote-debugging-port=9333
OLLAMA_TEST_CHROME_PORT=9333 go test ./app/browser/ -run TestRealChrome -v
```

Run that after touching the protocol code. Fakes share the client's own
constants and will happily agree with a wrong one; a real browser will not.

To develop against an unreleased bridge, point `Manager.MCPCommand` at a
checkout (`node path/to/mcp-server/dist/index.js`).

### UI Development

#### Setup

Install required tools:

```bash
go install github.com/tkrajina/typescriptify-golang-structs/tscriptify@latest
```

#### Develop UI (Development Mode)

1. Start the React development server (with hot-reload):

```bash
cd ui/app
npm install
npm run dev
```

2. In a separate terminal, run the Ollama app with the `-dev` flag:

```bash
go generate ./... &&
OLLAMA_DEBUG=1 go run ./cmd/app -dev
```

The `-dev` flag enables:

- Loading the UI from the Vite dev server at http://localhost:5173
- Fixed UI server port at http://127.0.0.1:3001 for API requests
- CORS headers for cross-origin requests
- Hot-reload support for UI development

## Build


### Windows

- https://jrsoftware.org/isinfo.php


**Dependencies** - either build a local copy of ollama, or use a github release
```powershell
# Local dependencies
.\scripts\deps_local.ps1

# Release dependencies
.\scripts\deps_release.ps1 0.6.8
```

**Build**
```powershell
.\scripts\build_windows.ps1
```

### macOS

CI builds with Xcode 14.1 for OS compatibility prior to v13.  If you want to manually build v11+ support, you can download the older Xcode [here](https://developer.apple.com/services-account/download?path=/Developer_Tools/Xcode_14.1/Xcode_14.1.xip), extract, then `mv ./Xcode.app /Applications/Xcode_14.1.0.app` then activate with:

```
export CGO_CFLAGS="-O3 -mmacosx-version-min=12.0"
export CGO_CXXFLAGS="-O3 -mmacosx-version-min=12.0"
export CGO_LDFLAGS="-mmacosx-version-min=12.0"
export SDKROOT=/Applications/Xcode_14.1.0.app/Contents/Developer/Platforms/MacOSX.platform/Developer/SDKs/MacOSX.sdk
export DEVELOPER_DIR=/Applications/Xcode_14.1.0.app/Contents/Developer
```

**Dependencies** - either build a local copy of Ollama, or use a GitHub release:
```sh
# Local dependencies
./scripts/deps_local.sh

# Release dependencies
./scripts/deps_release.sh 0.6.8
```

**Build**
```sh
./scripts/build_darwin.sh
```
