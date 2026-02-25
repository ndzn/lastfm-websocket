package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
)

const (
	writeWait  = 10 * time.Second
	pongWait   = 60 * time.Second
	pingPeriod = (pongWait * 9) / 10

	maxMessageSize = 512
	pollInterval   = 5 * time.Second
)

// Message is the WebSocket message structure sent to clients.
type Message struct {
	Artist       string `json:"artist"`
	Track        string `json:"track"`
	ImageURL     string `json:"image_url"`
	TrackURL     string `json:"track_url"`
	IsNowPlaying bool   `json:"is_now_playing"`
}

// Client represents a single WebSocket connection.
type Client struct {
	hub      *Hub
	username string
	conn     *websocket.Conn
	send     chan []byte
}

// Hub maintains per-username pollers and manages client subscriptions.
// Multiple WebSocket clients watching the same username share a single
// Last.fm API poller, keeping API usage proportional to unique usernames
// rather than total connections.
type Hub struct {
	mu         sync.Mutex
	pollers    map[string]*UserPoller
	apiKey     string
	httpClient *http.Client
}

// UserPoller polls Last.fm for a specific username and broadcasts to all
// subscribed clients.
type UserPoller struct {
	username    string
	hub         *Hub
	mu          sync.RWMutex
	clients     map[*Client]bool
	cancel      context.CancelFunc
	lastMessage []byte
}

func newHub(apiKey string) *Hub {
	return &Hub{
		pollers: make(map[string]*UserPoller),
		apiKey:  apiKey,
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

func (h *Hub) subscribe(client *Client) {
	h.mu.Lock()
	defer h.mu.Unlock()

	poller, exists := h.pollers[client.username]
	if !exists {
		ctx, cancel := context.WithCancel(context.Background())
		poller = &UserPoller{
			username: client.username,
			hub:      h,
			clients:  make(map[*Client]bool),
			cancel:   cancel,
		}
		h.pollers[client.username] = poller
		go poller.run(ctx)
	}

	poller.mu.Lock()
	poller.clients[client] = true
	if poller.lastMessage != nil {
		select {
		case client.send <- poller.lastMessage:
		default:
		}
	}
	poller.mu.Unlock()
}

func (h *Hub) unsubscribe(client *Client) {
	h.mu.Lock()
	defer h.mu.Unlock()

	poller, exists := h.pollers[client.username]
	if !exists {
		return
	}

	poller.mu.Lock()
	delete(poller.clients, client)
	remaining := len(poller.clients)
	poller.mu.Unlock()

	close(client.send)

	if remaining == 0 {
		poller.cancel()
		delete(h.pollers, client.username)
	}
}

func (p *UserPoller) run(ctx context.Context) {
	log.Printf("Poller started for user: %s", p.username)
	defer log.Printf("Poller stopped for user: %s", p.username)

	var lastTrack Message

	p.poll(&lastTrack)

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.poll(&lastTrack)
		}
	}
}

func (p *UserPoller) poll(lastTrack *Message) {
	track, err := p.hub.getLastPlayedTrack(p.username)
	if err != nil {
		log.Printf("Error polling Last.fm for %s: %v", p.username, err)
		return
	}

	if track == nil {
		return
	}

	if *track != *lastTrack {
		*lastTrack = *track
		data, err := json.Marshal(track)
		if err != nil {
			log.Printf("Error marshaling track data: %v", err)
			return
		}
		p.broadcast(data)
	}
}

func (p *UserPoller) broadcast(message []byte) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	p.lastMessage = message
	for client := range p.clients {
		select {
		case client.send <- message:
		default:
		}
	}
}

// readPump reads and discards incoming WebSocket messages, handling pong
// frames to keep the connection alive.
func (c *Client) readPump() {
	defer func() {
		c.hub.unsubscribe(c)
		c.conn.Close()
	}()

	c.conn.SetReadLimit(maxMessageSize)
	c.conn.SetReadDeadline(time.Now().Add(pongWait))
	c.conn.SetPongHandler(func(string) error {
		c.conn.SetReadDeadline(time.Now().Add(pongWait))
		return nil
	})

	for {
		if _, _, err := c.conn.ReadMessage(); err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseNormalClosure) {
				log.Printf("WebSocket error for %s: %v", c.username, err)
			}
			break
		}
	}
}

// writePump sends messages from the send channel and periodic pings.
func (c *Client) writePump() {
	ticker := time.NewTicker(pingPeriod)
	defer func() {
		ticker.Stop()
		c.conn.Close()
	}()

	for {
		select {
		case message, ok := <-c.send:
			c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if !ok {
				c.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}
			if err := c.conn.WriteMessage(websocket.TextMessage, message); err != nil {
				return
			}
		case <-ticker.C:
			c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

var upgrader = websocket.Upgrader{
	CheckOrigin:     func(r *http.Request) bool { return true },
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
}

func handleWebSocket(hub *Hub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		username := strings.TrimPrefix(r.URL.Path, "/fm/")
		if len(username) == 0 {
			http.Error(w, "Username not provided", http.StatusBadRequest)
			return
		}

		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			log.Printf("WebSocket upgrade error: %v", err)
			return
		}

		client := &Client{
			hub:      hub,
			username: username,
			conn:     conn,
			send:     make(chan []byte, 16),
		}

		hub.subscribe(client)

		go client.writePump()
		client.readPump()
	}
}

// getLastPlayedTrack fetches the most recent track from Last.fm.
func (h *Hub) getLastPlayedTrack(username string) (*Message, error) {
	url := fmt.Sprintf(
		"http://ws.audioscrobbler.com/2.0/?method=user.getrecenttracks&user=%s&limit=1&api_key=%s&format=json",
		username, h.apiKey,
	)

	resp, err := h.httpClient.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, resp.Body)
		return nil, fmt.Errorf("Last.fm API returned status %d for user %s", resp.StatusCode, username)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var data struct {
		RecentTracks struct {
			Track []struct {
				Artist struct {
					Name string `json:"#text"`
				} `json:"artist"`
				Name  string `json:"name"`
				Image []struct {
					URL  string `json:"#text"`
					Size string `json:"size"`
				} `json:"image"`
				URL  string `json:"url"`
				Attr struct {
					NowPlaying string `json:"nowplaying"`
				} `json:"@attr"`
			} `json:"track"`
		} `json:"recenttracks"`
	}

	if err := json.Unmarshal(body, &data); err != nil {
		return nil, fmt.Errorf("failed to parse Last.fm response for user %s: %w", username, err)
	}

	if len(data.RecentTracks.Track) == 0 {
		return nil, nil
	}

	track := data.RecentTracks.Track[0]
	isNowPlaying := strings.TrimSpace(track.Attr.NowPlaying) == "true"
	var imageURL string

	for _, image := range track.Image {
		if image.Size == "large" {
			imageURL = image.URL
			break
		}
	}

	return &Message{
		Artist:       track.Artist.Name,
		Track:        track.Name,
		ImageURL:     imageURL,
		TrackURL:     track.URL,
		IsNowPlaying: isNowPlaying,
	}, nil
}

func main() {
	apiKey := os.Getenv("LASTFM_API_KEY")
	if apiKey == "" {
		log.Fatal("LASTFM_API_KEY environment variable not set")
	}

	port := os.Getenv("PORT")
	if port == "" {
		port = "3621"
	}

	hub := newHub(apiKey)

	mux := http.NewServeMux()
	mux.HandleFunc("/fm/", handleWebSocket(hub))
	mux.HandleFunc("/", serveExample)

	srv := &http.Server{
		Addr:         ":" + port,
		Handler:      mux,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	go func() {
		log.Printf("Server starting on :%s", port)
		if err := srv.ListenAndServe(); err != http.ErrServerClosed {
			log.Fatalf("Server error: %v", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, os.Interrupt, syscall.SIGTERM)
	<-quit

	log.Println("Shutting down gracefully...")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		log.Fatalf("Server forced to shutdown: %v", err)
	}
	log.Println("Server stopped")
}

func serveExample(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, exampleHTML)
}

const exampleHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>Last.fm Now Playing</title>
<style>
  * { margin: 0; padding: 0; box-sizing: border-box; }
  body {
    font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif;
    background: #121212; color: #fff;
    display: flex; justify-content: center; align-items: center;
    min-height: 100vh;
  }
  .container { text-align: center; max-width: 420px; width: 100%; padding: 20px; }
  h1 { font-size: 1.2rem; color: #b3b3b3; margin-bottom: 24px; }
  #connect-form { margin-bottom: 24px; display: flex; gap: 8px; justify-content: center; }
  #connect-form input {
    padding: 10px 14px; border-radius: 8px; border: 1px solid #333;
    background: #1e1e1e; color: #fff; font-size: 1rem; width: 200px;
  }
  #connect-form button {
    padding: 10px 18px; border-radius: 8px; border: none;
    background: #1db954; color: #fff; font-size: 1rem; cursor: pointer;
    font-weight: 600;
  }
  #connect-form button:hover { background: #1ed760; }
  .player { display: none; margin-top: 12px; }
  .player.active { display: block; }
  .album-art {
    width: 200px; height: 200px; border-radius: 12px;
    margin: 0 auto 16px; background: #282828;
    overflow: hidden; box-shadow: 0 8px 24px rgba(0,0,0,.5);
  }
  .album-art img { width: 100%; height: 100%; object-fit: cover; }
  .track-name { font-size: 1.3rem; font-weight: 700; margin-bottom: 4px; }
  .track-name a { color: #fff; text-decoration: none; }
  .track-name a:hover { text-decoration: underline; }
  .artist-name { font-size: 1rem; color: #b3b3b3; }
  .badge {
    display: inline-block; margin-top: 12px; padding: 4px 12px;
    border-radius: 12px; font-size: .75rem; font-weight: 600;
  }
  .badge.playing { background: #1db954; color: #fff; }
  .badge.scrobbled { background: #333; color: #b3b3b3; }
  .status { margin-top: 16px; font-size: .8rem; color: #666; }
</style>
</head>
<body>
<div class="container">
  <h1>Last.fm Now Playing</h1>
  <form id="connect-form">
    <input type="text" id="username" placeholder="Last.fm username" required>
    <button type="submit">Connect</button>
  </form>
  <div class="player" id="player">
    <div class="album-art"><img id="art" src="" alt="Album Art"></div>
    <div class="track-name"><a id="track" href="#" target="_blank"></a></div>
    <div class="artist-name" id="artist"></div>
    <span class="badge" id="badge"></span>
  </div>
  <div class="status" id="status"></div>
</div>
<script>
(function () {
  var ws, reconnectTimer, username;
  var form = document.getElementById("connect-form");
  var status = document.getElementById("status");
  var player = document.getElementById("player");

  form.addEventListener("submit", function (e) {
    e.preventDefault();
    username = document.getElementById("username").value.trim();
    if (!username) return;
    connect();
  });

  function connect() {
    if (ws) { ws.onclose = null; ws.close(); }
    var proto = location.protocol === "https:" ? "wss:" : "ws:";
    var url = proto + "//" + location.host + "/fm/" + encodeURIComponent(username);
    status.textContent = "Connecting…";
    ws = new WebSocket(url);

    ws.onopen = function () { status.textContent = "Connected"; };

    ws.onmessage = function (evt) {
      try {
        var d = JSON.parse(evt.data);
        document.getElementById("art").src = d.image_url || "";
        document.getElementById("track").textContent = d.track;
        document.getElementById("track").href = d.track_url || "#";
        document.getElementById("artist").textContent = d.artist;
        var badge = document.getElementById("badge");
        badge.textContent = d.is_now_playing ? "Now Playing" : "Last Played";
        badge.className = "badge " + (d.is_now_playing ? "playing" : "scrobbled");
        player.classList.add("active");
      } catch (e) { console.error("Parse error", e); }
    };

    ws.onclose = function () {
      status.textContent = "Disconnected – reconnecting in 3s…";
      reconnectTimer = setTimeout(connect, 3000);
    };

    ws.onerror = function () { ws.close(); };
  }
})();
</script>
</body>
</html>`
