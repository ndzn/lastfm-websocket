package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestMarshalTrackEvent(t *testing.T) {
	want := TrackPayload{
		Artist:       "Grant",
		ArtistMBID:   "artist-mbid",
		ArtistURL:    "https://example.com/artist",
		Track:        "Wishes",
		TrackMBID:    "track-mbid",
		Album:        "Perch",
		AlbumMBID:    "album-mbid",
		ImageURL:     "https://example.com/cover.jpg",
		TrackURL:     "https://example.com/track",
		IsNowPlaying: true,
		DateUTS:      1704067200,
		DateText:     "1 Jan 2024, 00:00",
		Loved:        true,
		ScrobbledAt:  "2024-01-01T00:00:00Z",
	}

	data, err := marshalTrackEvent(want)
	if err != nil {
		t.Fatalf("marshalTrackEvent() error = %v", err)
	}

	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(data, &envelope); err != nil {
		t.Fatalf("track event is not valid JSON: %v", err)
	}
	if len(envelope) != 2 {
		t.Fatalf("track event has %d top-level fields, want 2: %s", len(envelope), data)
	}

	var event trackEvent
	if err := json.Unmarshal(data, &event); err != nil {
		t.Fatalf("unmarshal track event: %v", err)
	}
	if event.Type != trackEventType {
		t.Errorf("event type = %q, want %q", event.Type, trackEventType)
	}
	if event.Data != want {
		t.Errorf("event data = %#v, want %#v", event.Data, want)
	}
}

func TestHeartbeatFrame(t *testing.T) {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(heartbeatFrame, &envelope); err != nil {
		t.Fatalf("heartbeat frame is not valid JSON: %v", err)
	}
	if len(envelope) != 1 {
		t.Fatalf("heartbeat has %d fields, want 1: %s", len(envelope), heartbeatFrame)
	}

	var event struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(heartbeatFrame, &event); err != nil {
		t.Fatalf("unmarshal heartbeat: %v", err)
	}
	if event.Type != heartbeatEventType {
		t.Errorf("heartbeat type = %q, want %q", event.Type, heartbeatEventType)
	}
}

func TestPollBroadcastsAndCachesTypedTrackEvent(t *testing.T) {
	const response = `{
		"recenttracks": {
			"track": [{
				"artist": {
					"name": "Grant",
					"mbid": "artist-mbid",
					"url": "https://example.com/artist"
				},
				"name": "Wishes",
				"mbid": "track-mbid",
				"album": {"#text": "Perch", "mbid": "album-mbid"},
				"image": [{"#text": "https://example.com/cover.jpg", "size": "large"}],
				"url": "https://example.com/track",
				"loved": "1",
				"date": {"uts": "not-used-for-now-playing"},
				"@attr": {"nowplaying": "true"}
			}]
		}
	}`

	hub := newTestHub(t, response, func(request *http.Request) {
		query := request.URL.Query()
		if query.Get("method") != "user.getrecenttracks" {
			t.Errorf("method = %q, want user.getrecenttracks", query.Get("method"))
		}
		if query.Get("limit") != "1" {
			t.Errorf("limit = %q, want 1", query.Get("limit"))
		}
		if query.Get("extended") != "1" {
			t.Errorf("extended = %q, want 1", query.Get("extended"))
		}
	})

	client := &Client{send: make(chan []byte, 1)}
	poller := &UserPoller{
		username: "listener",
		hub:      hub,
		clients:  map[*Client]bool{client: true},
	}
	var lastTrack TrackPayload

	poller.poll(&lastTrack)

	var message []byte
	select {
	case message = <-client.send:
	default:
		t.Fatal("poll did not broadcast a track event")
	}
	if !bytes.Equal(poller.lastMessage, message) {
		t.Fatalf("cached message = %s, broadcast message = %s", poller.lastMessage, message)
	}

	var event trackEvent
	if err := json.Unmarshal(message, &event); err != nil {
		t.Fatalf("unmarshal broadcast event: %v", err)
	}
	if event.Type != trackEventType {
		t.Errorf("event type = %q, want %q", event.Type, trackEventType)
	}
	if event.Data != lastTrack {
		t.Errorf("event data = %#v, last track = %#v", event.Data, lastTrack)
	}
	want := TrackPayload{
		Artist:       "Grant",
		ArtistMBID:   "artist-mbid",
		ArtistURL:    "https://example.com/artist",
		Track:        "Wishes",
		TrackMBID:    "track-mbid",
		Album:        "Perch",
		AlbumMBID:    "album-mbid",
		ImageURL:     "https://example.com/cover.jpg",
		TrackURL:     "https://example.com/track",
		IsNowPlaying: true,
		Loved:        true,
	}
	if event.Data != want {
		t.Errorf("event data = %#v, want %#v", event.Data, want)
	}
	var rawEvent struct {
		Data map[string]json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(message, &rawEvent); err != nil {
		t.Fatalf("unmarshal raw broadcast event: %v", err)
	}
	if _, exists := rawEvent.Data["scrobbled_at"]; exists {
		t.Errorf("now-playing event unexpectedly includes scrobbled_at: %s", message)
	}

	poller.poll(&lastTrack)
	select {
	case duplicate := <-client.send:
		t.Fatalf("unchanged track was broadcast again: %s", duplicate)
	default:
	}
}

func TestGetLastPlayedTrackParsesCompactCompletedTrack(t *testing.T) {
	const response = `{
		"recenttracks": {
			"track": [{
				"artist": {"#text": "Grant", "mbid": "artist-mbid"},
				"name": "Wishes",
				"mbid": "track-mbid",
				"album": {"#text": "Perch", "mbid": "album-mbid"},
				"image": [{"#text": "https://example.com/cover.jpg", "size": "large"}],
				"url": "https://example.com/track",
				"loved": "0",
				"date": {"uts": "1704067200", "#text": "1 Jan 2024, 00:00"}
			}]
		}
	}`

	hub := newTestHub(t, response, nil)
	track, err := hub.getLastPlayedTrack("listener")
	if err != nil {
		t.Fatalf("getLastPlayedTrack() error = %v", err)
	}
	want := &TrackPayload{
		Artist:       "Grant",
		ArtistMBID:   "artist-mbid",
		Track:        "Wishes",
		TrackMBID:    "track-mbid",
		Album:        "Perch",
		AlbumMBID:    "album-mbid",
		ImageURL:     "https://example.com/cover.jpg",
		TrackURL:     "https://example.com/track",
		IsNowPlaying: false,
		DateUTS:      1704067200,
		DateText:     "1 Jan 2024, 00:00",
		Loved:        false,
		ScrobbledAt:  "2024-01-01T00:00:00Z",
	}
	if track == nil || *track != *want {
		t.Errorf("getLastPlayedTrack() = %#v, want %#v", track, want)
	}
}

func TestGetLastPlayedTrackClearsMalformedTimestamp(t *testing.T) {
	const response = `{
		"recenttracks": {
			"track": [{
				"artist": {"#text": "Grant"},
				"name": "Wishes",
				"date": {"uts": "not-a-timestamp"}
			}]
		}
	}`

	hub := newTestHub(t, response, nil)
	track, err := hub.getLastPlayedTrack("listener")
	if err != nil {
		t.Fatalf("getLastPlayedTrack() error = %v", err)
	}
	if track == nil {
		t.Fatal("getLastPlayedTrack() returned nil track")
	}
	if track.DateUTS != 0 || track.DateText != "" || track.ScrobbledAt != "" {
		t.Fatalf("malformed timestamp fields = (%d, %q, %q), want (0, empty, empty)", track.DateUTS, track.DateText, track.ScrobbledAt)
	}
}

func newTestHub(t *testing.T, response string, inspectRequest func(*http.Request)) *Hub {
	t.Helper()
	hub := newHub("test-key", 1)
	hub.httpClient = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if inspectRequest != nil {
			inspectRequest(request)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(response)),
			Header:     make(http.Header),
		}, nil
	})}
	return hub
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}
