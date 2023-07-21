# LastFM to Websocket

> A module that can be used to get a user's [LastFM](https://last.fm) recent song(s) along with other data and send it over a websocket connection. This can be useful for realtime status updates on a website, or OBS overlays for streaming.


### Requesting data
```
ws://localhost:3621/fm/USERNAME
```

### Example JSON response
```json
{
  "artist": "Grant",
  "track": "Wishes",
  "image_url": "https://lastfm.freetls.fastly.net/i/u/174s/5bb0cf2d5c308fb4df40e6aab7514d4d.jpg",
  "track_url": "https://www.last.fm/music/Grant/_/Wishes",
  "is_now_playing": true
}
```

> #### This soulution polls LastFM every 2 seconds for an update. LastFM isnt exactly specific on their hard [rate limits](https://www.last.fm/api/intro) but it _should_ be fine for personal use.

## Running
### Docker
It is possible to run an instance using Docker. I _highly_ recommend using Docker compose. Check the example config and customize it to your needs.