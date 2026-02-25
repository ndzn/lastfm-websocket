# LastFM to WebSocket

> A headless service that streams a user's [Last.fm](https://last.fm) currently-playing (or most recently scrobbled) track over a WebSocket connection. Designed to run on a server without human intervention — no UI, just an API.

## Features

- **Shared polling** – multiple WebSocket clients watching the same Last.fm username share a single API poller, keeping API usage proportional to unique usernames rather than total connections.
- **Ping/pong keep-alive** – dead connections are detected and cleaned up automatically.
- **Graceful shutdown** – the server finishes in-flight connections before exiting on `SIGINT`/`SIGTERM`.
- **Origin checking** – configurable `ALLOWED_ORIGINS` to prevent cross-site WebSocket hijacking.
- **Poller limits** – configurable `MAX_POLLERS` to cap the number of unique usernames being monitored.
- **Input validation** – usernames are validated against Last.fm's format before any API calls are made.

## Quick Start

### Environment Variables

| Variable | Required | Default | Description |
|---|---|---|---|
| `LASTFM_API_KEY` | **yes** | – | Your [Last.fm API key](https://www.last.fm/api/account/create) |
| `PORT` | no | `3621` | HTTP listen port |
| `ALLOWED_ORIGINS` | no | *(allow all)* | Comma-separated list of allowed WebSocket origins (e.g. `https://example.com,https://other.com`). Leave empty to allow all origins. |
| `MAX_POLLERS` | no | `1000` | Maximum number of unique usernames that can be monitored simultaneously |

### Run Locally

```bash
export LASTFM_API_KEY=your_key_here
go run main.go
```

Connect a WebSocket client to:

```
ws://localhost:3621/fm/USERNAME
```

### Docker

```bash
docker build -t lastfm-websocket .
docker run -p 3621:3621 -e LASTFM_API_KEY=your_key_here lastfm-websocket
```

Or use Docker Compose – see `docker-compose.yml.example`.

## WebSocket API

### Connecting

```
ws://your-server:3621/fm/USERNAME
```

Replace `USERNAME` with any Last.fm username (alphanumeric, hyphens, underscores, max 15 characters). The server sends a JSON message each time the track changes.

### Example JSON Response

```json
{
  "artist": "Grant",
  "track": "Wishes",
  "image_url": "https://lastfm.freetls.fastly.net/i/u/174s/5bb0cf2d5c308fb4df40e6aab7514d4d.jpg",
  "track_url": "https://www.last.fm/music/Grant/_/Wishes",
  "is_now_playing": true
}
```

`is_now_playing` is `true` when the user is actively listening; `false` means this is the most recently scrobbled track.

### Embedding in Your Own Page

```html
<div id="now-playing"></div>
<script>
  var ws = new WebSocket("ws://your-server:3621/fm/YOUR_USERNAME");
  ws.onmessage = function (event) {
    var track = JSON.parse(event.data);
    document.getElementById("now-playing").innerHTML =
      '<img src="' + track.image_url + '" width="100">' +
      '<p><strong>' + track.track + '</strong> by ' + track.artist + '</p>';
  };
</script>
```

## Architecture

```
Hub (max 1000 pollers by default)
├── UserPoller("alice")  ← polls Last.fm every 5 s
│   ├── WebSocket Client 1
│   └── WebSocket Client 2
└── UserPoller("bob")    ← polls Last.fm every 5 s
    ├── WebSocket Client 3
    └── WebSocket Client 4
```

Each unique Last.fm username has **one** poller goroutine. When the last client disconnects, the poller is stopped. This keeps API usage low even with many concurrent connections.