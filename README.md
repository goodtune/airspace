# Airspace

Airspace shows live aircraft on a 3D globe. Everyone connected sees the same
session: the same camera, the same selected aircraft, the same refresh rate.
It records what it shows, replays it later, and exports replays as video.

https://github.com/user-attachments/assets/e3d32eb3-17f0-4226-adcb-a0c803bfa717

*Twelve minutes of recorded world traffic, replayed at 60× and exported by
Airspace itself.*

It is one Go binary for personal use on a private network. Aircraft data
comes from the [OpenSky Network](https://opensky-network.org). Imagery is
NASA Blue Marble from
[GIBS](https://www.earthdata.nasa.gov/engage/open-data-services-software/earthdata-developer-portal/gibs-api).

## Quick start

1. Create an API client on your [OpenSky](https://opensky-network.org)
   account page.
2. Download a binary from the
   [releases page](https://github.com/goodtune/airspace/releases), or build
   one with `make local`.
3. Run it:

   ```sh
   OPENSKY_CLIENT_ID=... OPENSKY_CLIENT_SECRET=... ./bin/airspace
   ```

4. Open `http://<host>:8080`.

[Getting started](docs/getting-started.md) has the detail, including reading
the credentials from dotvault.

## What it does

- **One shared session.** Five browsers cost the same OpenSky credits as one.
- **Nobody watching, nothing requested.** With no active browser it makes no
  request to OpenSky at all.
- **Viewport first.** Each poll asks only for the area being looked at, and
  the interval is held to what the daily credit allowance can afford.
- **Recording, replay, export.** Sessions are recorded to SQLite, replayed at
  no credit cost, and exported to MP4 in the browser.

## Documentation

| | |
|---|---|
| [Getting started](docs/getting-started.md) | Create an OpenSky client and run Airspace |
| [How it works](docs/how-it-works.md) | The shared session and the activity rules |
| [Credits and query scope](docs/features/credits.md) | What each poll costs, and the limits that protect the allowance |
| [Recording](docs/features/recording.md) | What is stored, and measured storage figures |
| [Replay and video export](docs/features/replay.md) | Watching a session again, and exporting MP4 |
| [Installation](docs/admin/installation.md) | Packages, container, systemd |
| [Configuration](docs/admin/configuration.md) | Every setting |
| [HTTPS](docs/admin/https.md) | Certificates from Let's Encrypt or a private ACME server |
| [Monitoring](docs/admin/monitoring.md) | OpenTelemetry metrics |
| [HTTP API](docs/api.md) | Endpoints |
| [Development](docs/development.md) | Tests, tooling, releases |
| [Attribution](docs/attribution.md) | Data, imagery and library credits |

## Licence

MIT. See [LICENSE](LICENSE). Aircraft data is from The OpenSky Network and
imagery is from NASA GIBS; see [Attribution](docs/attribution.md).
