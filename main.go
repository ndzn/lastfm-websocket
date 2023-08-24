package main

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

// websocket message structure
type Message struct {
	Artist       string `json:"artist"`
	Track        string `json:"track"`
	ImageURL     string `json:"image_url"`
	TrackURL     string `json:"track_url"`
	IsNowPlaying bool   `json:"is_now_playing"`
}

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

func main() {
	// get API key from env
	apiKey := os.Getenv("LASTFM_API_KEY")
	if apiKey == "" {
		log.Fatal("LASTFM_API_KEY environment variable not set. Please provide a valid Last.fm API key.")
	}

	port := os.Getenv("PORT")
	if port == "" {
		port = "3621" // default port
	}

	http.HandleFunc("/fm/", handleWebSocket)
    c := make(chan os.Signal, 1)
    signal.Notify(c, os.Interrupt)
    go func() {
        <-c
        log.Println("Shutting down gracefully...")
        os.Exit(0)
    }()

    log.Fatal(http.ListenAndServe(":"+port, nil))
}

func handleWebSocket(w http.ResponseWriter, r *http.Request) {
	username := strings.TrimPrefix(r.URL.Path, "/fm/")
	if len(username) == 0 {
		http.Error(w, "Username not provided", http.StatusBadRequest)
		return
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println(err)
		return
	}
	defer conn.Close()

	// create a channel to signal when to send data
	dataCh := make(chan *Message)

	// goroutine to fetch and send the most recent track
	go func() {
		lastTrack := &Message{} // init with an empty track

		for {
			track, err := getLastPlayedTrack(username)
			if err != nil {
				log.Println(err)
				continue
			}

			// check if the track has changed
			if track != nil && (*track != *lastTrack || lastTrack.IsNowPlaying != track.IsNowPlaying) {
				dataCh <- track
				lastTrack = track
			}

			time.Sleep(2 * time.Second) // poll interval
		}
	}()

    // send data over websocket when needed
    for data := range dataCh {
        message, _ := json.Marshal(data)
        if err := conn.WriteMessage(websocket.TextMessage, message); err != nil {
            log.Println("Error writing to WebSocket (Websocket probably closed):", err)
        }
    }
}


// helper function to get most recent track or most recent scrobble
func getLastPlayedTrack(username string) (*Message, error) {
	apiKey := os.Getenv("LASTFM_API_KEY")

	// make req
	url := "http://ws.audioscrobbler.com/2.0/?method=user.getrecenttracks&user=" + username + "&limit=1&api_key=" + apiKey + "&format=json"
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	// parse json
	var data struct {
		RecentTracks struct {
			Track []struct {
				Artist struct {
					Name string `json:"#text"`
				} `json:"artist"`
				Name   string `json:"name"`
				Image  []struct {
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
		return nil, err
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

	message := &Message{
		Artist:       track.Artist.Name,
		Track:        track.Name,
		ImageURL:     imageURL,
		TrackURL:     track.URL,
		IsNowPlaying: isNowPlaying,
	}

	return message, nil
}
