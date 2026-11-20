# Airspace

Airspace shows live aircraft on a 3D globe. Everyone connected sees the same
session: the same camera, the same selected aircraft, the same refresh rate.
It records what it shows, replays it later, and exports replays as video.

<video src="docs/demo.webm" autoplay loop muted playsinline width="100%">
  <a href="docs/demo.webm">Watch the demo clip (WebM, 12 seconds)</a>
</video>

*Twelve minutes of recorded world traffic, replayed at 60× and exported by
Airspace itself. Make it again with `make demo`.*

It is one Go binary for personal use on a private network. Aircraft data
comes from the [OpenSky Network](https://opensky-network.org). Imagery is
NASA Blue Marble from [GIBS](https://www.earthdata.nasa.gov/engage/open-data-services-software/earthdata-developer-portal/gibs-api).

## Quick start

1. Create an OpenSky API client (see below).
2. Build: `make build`. The binaries are `bin/airspace-linux-amd64` and
   `bin/airspace-linux-arm64`. `make local` builds `bin/airspace` for the
   machine you are on.
3. Run:

   ```sh
   OPENSKY_CLIENT_ID=... OPENSKY_CLIENT_SECRET=... ./bin/airspace
   ```

4. Open `http://<host>:8080`.

`deploy/airspace.service` is an example systemd unit. `Dockerfile` builds a
minimal image.

## Create an OpenSky API client

1. Register a free account at <https://opensky-network.org> and sign in.
2. Open your **Account** page and find the **API Client** section.
3. Create a client. OpenSky gives you a `client_id` and a `client_secret`.
4. Give them to Airspace as `OPENSKY_CLIENT_ID` and `OPENSKY_CLIENT_SECRET`.

The secret stays on the server. Airspace never sends it, or the access token,
to a browser, and never writes either to the log.

## How it uses OpenSky

- One poller, shared by every browser. Five browsers cost the same as one.
- **Nobody watching, nothing requested.** With no active browser, Airspace
  makes no request to OpenSky at all: no poll, no token request, no
  keep-alive. It makes no request at startup.
- **Viewport first.** Each poll asks only for the area being looked at. A
  whole-world query is an explicit, temporary choice ("Global" coverage).
- A browser is *active* when its connection is open and its tab was visible
  within `HIDDEN_TIMEOUT`. A browser in replay is not active.
- After `IDLE_TIMEOUT` with no input from anyone, polling pauses until
  someone clicks.

## Configuration

Settings are environment variables. Each also has a flag that overrides it:
lower-case the name and change `_` to `-`, for example `-poll-interval 15s`.
Run `airspace -h` for the list. Nothing here can be changed from the UI.

| Setting | Default | Meaning |
|---|---|---|
| `OPENSKY_CLIENT_ID` | required | OAuth2 client id |
| `OPENSKY_CLIENT_SECRET` | required | OAuth2 client secret |
| `LISTEN_ADDR` | `:8080` | HTTP listen address |
| `POLL_INTERVAL` | `10s` | The shared refresh interval at startup |
| `MIN_INTERVAL` | `auto` | Shortest interval a browser may set: `auto`, or a duration of 5 s or more |
| `MAX_INTERVAL` | unset | Longest interval a browser may set |
| `DAILY_CREDITS` | `4000` | Your daily allowance, used by `auto` and by the forecast |
| `DAILY_USE_HOURS` | `2` | Hours of use you expect in a day, used by `auto` |
| `CREDIT_RESERVE` | `200` | Balance the guard tries not to go below |
| `HIDDEN_TIMEOUT` | `60s` | A hidden tab stops counting as active after this |
| `IDLE_TIMEOUT` | `30m` | Pause after this long with no input; `0` disables |
| `DB_PATH` | `./airspace.db` | SQLite database file |
| `MAX_DB_SIZE` | unset | Delete the oldest sessions above this size, for example `2GB` |
| `RETENTION_DAYS` | unset | Delete sessions older than this many days |
| `START_VIEW` | `151.2,-33.9,450000` | Opening camera as longitude, latitude, height in metres. Used until a shared camera has been saved. |

Durations are Go durations (`10s`, `2m`) or a bare number of seconds.

Airspace refuses to start, with a message, when:

- a credential is missing
- a fixed `MIN_INTERVAL` is below 5 s, the resolution of OpenSky data
- `MIN_INTERVAL` is greater than `MAX_INTERVAL`
- `DAILY_USE_HOURS` is outside 0 to 24

These checks are local. If `POLL_INTERVAL` is outside the bounds, Airspace
clamps it and logs that it did.

`OPENSKY_TOKEN_URL` and `OPENSKY_API_URL` also exist. They point Airspace at
a fake OpenSky for development and tests. Do not set them in normal use.

## Credit arithmetic

A free registered OpenSky account has 4,000 credits a day. The cost of one
`/states/all` request depends on the area asked for:

| Area | Cost |
|---|---|
| 25 sq° or less | 1 credit |
| 25 to 100 sq° | 2 credits |
| 100 to 400 sq° | 3 credits |
| more than 400 sq°, or the whole world | 4 credits |

Credits used in an hour:

| Interval | 1 credit | 2 credits | 3 credits | 4 credits |
|---|---|---|---|---|
| 5 s | 720 | 1,440 | 2,160 | 2,880 |
| 10 s | 360 | 720 | 1,080 | 1,440 |
| 60 s | 60 | 120 | 180 | 240 |

The defaults assume two hours of use a day:

- At the default 10 s, two hours costs at most 2,880 credits, even zoomed
  all the way out.
- At 5 s, two hours of a regional view costs 1,440 or 2,880 credits.
- At 5 s, the two larger tiers would not last two hours. So `MIN_INTERVAL=auto`
  raises the floor for them.

The `auto` floor, in seconds, rounded up:

    max(5, DAILY_USE_HOURS × 3600 × cost of the poll ÷ (DAILY_CREDITS − CREDIT_RESERVE))

| Poll cost | Floor at 2 hours | Floor at 24 hours |
|---|---|---|
| 1 credit | 5 s | 23 s |
| 2 credits | 5 s | 46 s |
| 3 credits | 6 s | 69 s |
| 4 credits | 8 s | 91 s |

When the view crosses into a costlier tier, the floor rises, and the interval
rises with it if it must. Zoom back in and the interval returns to what was
asked for. The UI names the reason whenever the interval is not the one a
browser chose: "operator minimum", "operator maximum", "credit reserve", or
"rate limited".

### Query scope

- Airspace pads the viewed box by up to 20% a side, so aircraft near the edge
  are there before they scroll in. Padding never moves a query into a
  costlier tier.
- A view that crosses the antimeridian becomes two boxes.
- A browser that detaches from the shared camera adds its own box. Airspace
  then picks the cheapest of: separate boxes, one box around them all, or one
  global query.
- One poll never costs more than 4 credits. A plan that would cost 4 or more
  becomes a single global query.
- A view must shrink 10% below a tier boundary before the plan drops a tier,
  so a small wobble does not flip the cost back and forth.
- Moving the camera never causes a request. The new scope applies at the next
  scheduled poll.
- The dashed outline on the globe shows what was queried. Outside it, an
  empty sky means "not asked", not "no traffic".

### Credit guard

- Airspace reads the balance from `X-Rate-Limit-Remaining` on every response.
- Below `CREDIT_RESERVE`, it stretches the interval in proportion: at half
  the reserve the interval doubles, at a quarter it is four times as long.
  The balance drains more and more slowly and does not reach zero. This may
  take the interval above `MAX_INTERVAL`.
- On HTTP 429 it waits for `X-Rate-Limit-Retry-After-Seconds`, then tries
  again.
- The panel shows credits left, burn rate, viewing time left at the current
  rate, and the clock time that is. It warns under 30 minutes.

### Refill time: not established

The OpenSky documentation says only that credits refill "daily". It gives no
time and no time zone. This build was made with the anonymous allowance and
did not run across a refill, so the refill time was not observed either.

Airspace therefore shows the forecast alone ("runs out at 14:22") and says in
the panel that it does not know whether credits run out before the refill.
If you observe the refill (the balance jumps back to 4,000), the time it
happens can be added as a setting.

## Recording

Recording is on by default. A session starts when the first browser becomes
active. It ends when no browser has been active for 2 minutes; the 2 minutes
only stop a page reload from splitting a session. Polling still stops the
moment the last browser leaves.

A session stores exactly what was fetched, plus every change to shared state
(camera, interval, selection, coverage, scope, clients, pauses). Recording
never causes a request and never changes what is queried.

So a normal session holds traffic **inside the viewed area only**. Replay can
move the camera anywhere, but aircraft exist only where the session looked.
To record the whole world, so the session can be re-shot from any angle,
switch **Coverage** to **Global**. The UI shows the cost first. Global
coverage turns itself off when the session ends.

If the database cannot be opened or written, Airspace carries on live-only
and shows a warning. Storage problems do not take the live view down.

### Storage figures

Measured on real OpenSky data, captured 21 November 2026. Each snapshot is
one gzip-compressed row; it comes to about 37 bytes an aircraft.

| View | Aircraft | Bytes per snapshot | Interval | Per hour |
|---|---|---|---|---|
| 25 sq° around Sydney | 48 | 2.1 kB | 5 s | 1.5 MB |
| 25 sq° around Frankfurt | 158 | 6.3 kB | 5 s | 4.5 MB |
| 100 sq° of western Europe | 573 | 20.8 kB | 5 s | 15 MB |
| 400 sq° of Europe | 1,381 | 49 kB | 6 s | 30 MB |
| Whole world | 12,444 | 459 kB | 8 s | 207 MB |

Two hours a day of a busy regional view is about 10 MB a day. Two hours of
global coverage is about 400 MB. Set `MAX_DB_SIZE` or `RETENTION_DAYS` to
have the oldest sessions deleted automatically.

## Replay and video export

Open **Sessions**, then **Replay**. Replay reads only the database. It costs
no credits, and a browser in replay does not keep the live poller running.
Replay is local to your browser; others stay on the live session.

- Play, pause, scrub, speeds from 1× to 120×, and jump to the previous or
  next recorded event.
- Aircraft are interpolated along a great circle between the two snapshots
  either side of the playhead. Aircraft in only one of the two fade in or
  out.
- Camera: **As recorded** follows the recorded camera, **Free** lets you fly,
  **Follow aircraft** tracks the aircraft you clicked, **Keyframes** moves
  between cameras you placed with **Add keyframe**.
- For a viewport session, a keyframe that looks outside the recorded area is
  marked red and warned about.

**Export video** renders the replay to MP4 in the browser, frame by frame. It
waits for imagery on every frame, so the result is the same on a slow machine
as on a fast one. Options: time range, speed, resolution up to 4K, frame
rate, camera, scope outline, and burned-in clock and attribution.

Export needs WebCodecs (current Chrome, Edge, Safari). Without it the dialog
says so; there is no lower-quality fallback. Chrome and Edge encode H.264.
A browser with no H.264 encoder gets VP9 or AV1 in the MP4 instead, and the
dialog says which.

## HTTP API

| Endpoint | Purpose |
|---|---|
| `GET /api/states` | Latest snapshot. Compact JSON, gzip, ETag. |
| `GET /api/status` | Poller, credit, client, and storage status |
| `GET /api/sessions` | Recorded sessions |
| `GET /api/sessions/{id}` | One session and the times of its snapshots |
| `PATCH /api/sessions/{id}` | Rename: `{"name": "..."}` |
| `DELETE /api/sessions/{id}` | Delete |
| `GET /api/sessions/{id}/snapshots?from=&to=&limit=` | A page of snapshots. Times are Unix milliseconds. When `more` is true, ask again with `from=next`. |
| `GET /api/sessions/{id}/events?from=&to=&after=&limit=&types=&exclude=` | A page of the event track |
| `GET /healthz` | Liveness |
| `GET /api/stream` | The Datastar event stream each browser holds |
| `POST /api/action/{name}` | A change to shared state from one browser |

None of these causes a request to OpenSky. `/api/states`, `/api/status`, and
`/healthz` do not count as someone watching.

Each aircraft in a snapshot is an array. The `fields` entry names the
positions: icao24, callsign, country, lon, lat, baro, geo, onGround,
velocity, track, vrate, squawk, timePos, lastContact. Aircraft with no
position are left out.

## Development

```sh
make test        # Go tests, with the race detector
node --test web/test/geo.test.js    # motion and interpolation maths
make e2e         # browser tests (Playwright), in e2e/
```

All tests use a fake OpenSky that counts every request, token requests
included. No test calls the real API.

To work on the frontend without spending credits:

```sh
make dev-fake    # terminal 1: a fake OpenSky replaying captured world traffic
make dev         # terminal 2: Airspace against it, on :8080
make seed        # optional: load the capture into dev.db as a session to replay
```

`testdata/capture/` holds 87 whole-world snapshots at 10 s spacing (14.5
minutes), stored with Git LFS. `cmd/capture` records more; it stops before
the balance runs out.

Frontend libraries (CesiumJS, Datastar, mp4-muxer) are fetched at pinned
versions by `scripts/vendor.sh` and embedded in the binary. Nothing loads
from a CDN at run time. The only thing a browser fetches from the internet is
imagery tiles from NASA GIBS.

## Design notes

- **Hidden tabs.** By default the Datastar client closes its stream when a
  tab is hidden and reopens it when the tab shows again. Airspace opens the
  stream with Datastar's `openWhenHidden` option, so the connection stays up
  and the page reports its visibility. That lets the server apply
  `HIDDEN_TIMEOUT` exactly, and a brief tab switch does not look like a
  disconnect.
- **Aircraft data does not travel over Datastar.** Signals carry control
  state. When the snapshot version changes, each browser fetches
  `/api/states`.
- **No authentication.** Airspace assumes a trusted network.

## Attribution

- Aircraft data: [The OpenSky Network](https://opensky-network.org). OpenSky
  asks that work using its data cites: Matthias Schäfer, Martin Strohmeier,
  Vincent Lenders, Ivan Martinovic and Matthias Wilhelm, "Bringing Up OpenSky:
  A Large-scale ADS-B Sensor Network for Research", IPSN 2014. The free API
  is for non-commercial use.
- Imagery: NASA Global Imagery Browse Services (GIBS), layer
  `BlueMarble_ShadedRelief_Bathymetry`. NASA asks for the credit: "We
  acknowledge the use of imagery provided by services from NASA's Global
  Imagery Browse Services (GIBS), part of NASA's Earth Science Data and
  Information System (ESDIS)."
- [CesiumJS](https://cesium.com/cesiumjs/) (Apache 2.0),
  [Datastar](https://data-star.dev) (MIT), and
  [mp4-muxer](https://github.com/Vanilagy/mp4-muxer) (MIT) are embedded.

Both credits are shown in the UI and can be burned into exported video.
