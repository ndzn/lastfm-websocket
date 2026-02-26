package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"strconv"
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

	maxMessageSize    = 512
	pollInterval      = 5 * time.Second
	defaultMaxPollers = 1000
)

var usernameRe = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,15}$`)

// Message is the WebSocket message structure sent to clients.
type Message struct {
	Artist       string `json:"artist"`
	Track        string `json:"track"`
	ImageURL     string `json:"image_url"`
	TrackURL     string `json:"track_url"`
	IsNowPlaying bool   `json:"is_now_playing"`
	DateUTS      int64  `json:"date_uts"`
	DateText     string `json:"date_text"`
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
	maxPollers int
	apiKey     string
	httpClient *http.Client
}

// UserPoller polls Last.fm for a specific username and broadcasts to all
// subscribed clients.
type UserPoller struct {
	username    string
	hub         *Hub
	mu          sync.Mutex
	clients     map[*Client]bool
	cancel      context.CancelFunc
	lastMessage []byte
}

func newHub(apiKey string, maxPollers int) *Hub {
	return &Hub{
		pollers:    make(map[string]*UserPoller),
		maxPollers: maxPollers,
		apiKey:     apiKey,
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

func (h *Hub) subscribe(client *Client) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	poller, exists := h.pollers[client.username]
	if !exists {
		if len(h.pollers) >= h.maxPollers {
			return fmt.Errorf("maximum number of monitored users reached")
		}
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
			log.Printf("Initial cached message for user %q dropped: client send channel is full", client.username)
		}
	}
	poller.mu.Unlock()

	return nil
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
	if remaining == 0 {
		poller.cancel()
		delete(h.pollers, client.username)
	}
	poller.mu.Unlock()

	close(client.send)
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
	p.mu.Lock()
	defer p.mu.Unlock()

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
	defer ticker.Stop()

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

func newUpgrader(allowedOrigins string) websocket.Upgrader {
	origins := parseAllowedOrigins(allowedOrigins)
	return websocket.Upgrader{
		ReadBufferSize:  1024,
		WriteBufferSize: 1024,
		CheckOrigin: func(r *http.Request) bool {
			if len(origins) == 0 {
				return true
			}
			origin := r.Header.Get("Origin")
			for _, allowed := range origins {
				if allowed == "*" || strings.EqualFold(origin, allowed) {
					return true
				}
			}
			return false
		},
	}
}

func parseAllowedOrigins(s string) []string {
	if s == "" {
		return nil
	}
	var origins []string
	for _, o := range strings.Split(s, ",") {
		o = strings.TrimSpace(o)
		if o != "" {
			origins = append(origins, o)
		}
	}
	return origins
}

func handleWebSocket(hub *Hub, upgrader websocket.Upgrader) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		username := strings.TrimPrefix(r.URL.Path, "/fm/")
		if len(username) == 0 {
			http.Error(w, "Username not provided", http.StatusBadRequest)
			return
		}
		if !usernameRe.MatchString(username) {
			http.Error(w, "Invalid username", http.StatusBadRequest)
			return
		}
		username = strings.ToLower(username)

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

		if err := hub.subscribe(client); err != nil {
			log.Printf("Subscribe error for %s: %v", username, err)
			conn.WriteMessage(websocket.CloseMessage,
				websocket.FormatCloseMessage(websocket.CloseTryAgainLater, err.Error()))
			conn.Close()
			close(client.send)
			return
		}

		go client.writePump()
		client.readPump()
	}
}

// getLastPlayedTrack fetches the most recent track from Last.fm.
func (h *Hub) getLastPlayedTrack(username string) (*Message, error) {
	params := url.Values{}
	params.Set("method", "user.getrecenttracks")
	params.Set("user", username)
	params.Set("limit", "1")
	params.Set("api_key", h.apiKey)
	params.Set("format", "json")
	reqURL := "http://ws.audioscrobbler.com/2.0/?" + params.Encode()

	resp, err := h.httpClient.Get(reqURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		const maxErrorBodyBytes = 1024
		bodySnippet, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
		errText := strings.TrimSpace(string(bodySnippet))
		if errText == "" {
			errText = "<empty>"
		}
		return nil, fmt.Errorf("Last.fm API returned status %d for user %s: %s", resp.StatusCode, username, errText)
	}

	const maxResponseBytes = 1 << 20 // 1 MiB
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
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
				Date struct {
					UTS  string `json:"uts"`
					Text string `json:"#text"`
				} `json:"date"`
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

	var dateUTS int64
	if track.Date.UTS != "" {
		if parsed, err := strconv.ParseInt(track.Date.UTS, 10, 64); err != nil {
			log.Printf("Failed to parse date UTS %q for user %s: %v", track.Date.UTS, username, err)
		} else {
			dateUTS = parsed
		}
	}

	return &Message{
		Artist:       track.Artist.Name,
		Track:        track.Name,
		ImageURL:     imageURL,
		TrackURL:     track.URL,
		IsNowPlaying: isNowPlaying,
		DateUTS:      dateUTS,
		DateText:     track.Date.Text,
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
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		log.Fatalf("Invalid PORT value %q: must be an integer between 1 and 65535", port)
	}

	maxPollers := defaultMaxPollers
	if v := os.Getenv("MAX_POLLERS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			if n > 0 {
				maxPollers = n
			} else {
				log.Printf("Invalid MAX_POLLERS value %q: must be greater than 0; using default %d", v, defaultMaxPollers)
			}
		} else {
			log.Printf("Invalid MAX_POLLERS value %q: not a valid integer; using default %d", v, defaultMaxPollers)
		}
	}

	hub := newHub(apiKey, maxPollers)
	upgrader := newUpgrader(os.Getenv("ALLOWED_ORIGINS"))

	mux := http.NewServeMux()
	mux.HandleFunc("/fm/", handleWebSocket(hub, upgrader))

	srv := &http.Server{
		Addr:        ":" + port,
		Handler:     mux,
		ReadTimeout: 15 * time.Second,
		IdleTimeout: 60 * time.Second,
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
