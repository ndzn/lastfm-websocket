# LastFM to WebSocket

> A headless service that streams a user's [Last.fm](https://last.fm) currently-playing (or most recently scrobbled) track over a WebSocket connection.

## Features

- **Shared polling** – multiple WebSocket clients watching the same Last.fm username share a single API poller, keeping API usage proportional to unique usernames rather than total connections.
- **Ping/pong keep-alive** – dead connections are detected and cleaned up automatically.
- **Browser heartbeats** – typed heartbeat events let browser clients detect silently dead sockets and reconnect.
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

Requires Go 1.26 or newer.

```bash
export LASTFM_API_KEY=your_key_here
go run main.go
```

Connect a WebSocket client to:

```
ws://localhost:3621/fm/USERNAME
```

### Docker (pre-built image)

A pre-built image is published to GitHub Container Registry on every push to `main` and on version tags.

```bash
docker pull ghcr.io/ndzn/lastfm-websocket:main
docker run -p 3621:3621 -e LASTFM_API_KEY=your_key_here ghcr.io/ndzn/lastfm-websocket:main
```

Or use Docker Compose – see `docker-compose.yml.example`.

### Docker (build locally)

```bash
docker build -t lastfm-websocket .
docker run -p 3621:3621 -e LASTFM_API_KEY=your_key_here lastfm-websocket
```

## WebSocket API

### Connecting

```
ws://your-server:3621/fm/USERNAME
```

Replace `USERNAME` with any Last.fm username (alphanumeric, hyphens, underscores, max 15 characters). The server sends typed JSON events. Clients should ignore event types they do not recognize.

### Track Event

```json
{
  "type": "track",
  "data": {
    "artist": "Grant",
    "artist_mbid": "",
    "artist_url": "https://www.last.fm/music/Grant",
    "track": "Wishes",
    "track_mbid": "",
    "album": "Perch",
    "album_mbid": "",
    "image_url": "https://lastfm.freetls.fastly.net/i/u/174s/5bb0cf2d5c308fb4df40e6aab7514d4d.jpg",
    "track_url": "https://www.last.fm/music/Grant/_/Wishes",
    "is_now_playing": true,
    "date_uts": 0,
    "date_text": "",
    "loved": false
  }
}
```

`is_now_playing` is `true` when the user is actively listening; `false` means this is the most recently scrobbled track. MusicBrainz IDs and other metadata may be empty when Last.fm has no value. Completed tracks also include `scrobbled_at` as an RFC 3339 UTC timestamp, such as `2026-08-19T14:30:00Z`.

The `scrobbled_at` field is omitted while a track is currently playing or when Last.fm returns an invalid timestamp. `loved` reflects whether the monitored user has loved the track on Last.fm.

### Heartbeat Event

```json
{"type":"heartbeat"}
```

The server sends a heartbeat every 20 seconds. Browser clients should consider the connection stale after 60 seconds without any valid event, close it, and reconnect. WebSocket ping/pong frames separately detect dead connections on the server.

`date_uts` is the Unix timestamp of when the track was scrobbled and `date_text` is the human-readable date string from the Last.fm API (e.g. `"01 Jan 2025, 12:00"`). When `is_now_playing` is `true`, or when Last.fm returns an invalid timestamp, these fields are `0` and `""` respectively.

### Embedding in Your Own Page

```html
<div id="now-playing"></div>
<script>
  const socketURL = "ws://your-server:3621/fm/YOUR_USERNAME";
  const staleAfter = 60_000;
  let retryDelay = 1_000;

  function renderTrack(track) {
    var container = document.getElementById("now-playing");
    container.innerHTML = "";

    if (track.image_url) {
      var img = document.createElement("img");
      img.src = track.image_url;
      img.width = 100;
      container.appendChild(img);
    }

    var p = document.createElement("p");
    var strong = document.createElement("strong");
    strong.textContent = track.track;
    p.appendChild(strong);
    p.appendChild(document.createTextNode(" by " + track.artist));

    container.appendChild(p);
  }

  function connect() {
    const ws = new WebSocket(socketURL);
    let lastSeen = Date.now();
    let reconnectScheduled = false;

    const watchdog = setInterval(function () {
      if (Date.now() - lastSeen > staleAfter) {
        reconnect();
      }
    }, 5_000);

    function reconnect() {
      if (reconnectScheduled) return;
      reconnectScheduled = true;
      clearInterval(watchdog);

      try {
        ws.close();
      } catch (_) {
        // The socket may still be connecting.
      }

      const delay = retryDelay + Math.random() * retryDelay * 0.25;
      setTimeout(connect, delay);
      retryDelay = Math.min(retryDelay * 2, 30_000);
    }

    ws.onmessage = function (event) {
      let message;
      try {
        message = JSON.parse(event.data);
      } catch (_) {
        return;
      }

      if (!message || typeof message.type !== "string") return;

      lastSeen = Date.now();
      retryDelay = 1_000;

      if (message.type === "track" && message.data) {
        renderTrack(message.data);
      }
      // Heartbeats and unknown event types require no additional handling.
    };

    ws.onerror = reconnect;
    ws.onclose = reconnect;
  }

  connect();
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
