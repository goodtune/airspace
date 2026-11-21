# HTTP API

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
