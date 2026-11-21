# Configuration

Settings are environment variables. Each also has a flag that overrides it:
lower-case the name and change `_` to `-`, for example `-poll-interval 15s`.
Run `airspace -h` for the list. Nothing here can be changed from the UI.

| Setting | Default | Meaning |
|---|---|---|
| `OPENSKY_CLIENT_ID` | required | OAuth2 client id |
| `OPENSKY_CLIENT_SECRET` | required | OAuth2 client secret |
| `OPENSKY_DOTVAULT` | unset | Read the id and secret from this dotvault secret instead. Flag: `--dotvault`. |
| `DOTVAULT_CONFIG` | unset | dotvault's own configuration file, when it is not in the usual place. See [Getting started](../getting-started.md#credentials-from-dotvault). |
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

HTTPS settings are in [HTTPS](https.md). Metrics settings are in
[Monitoring](monitoring.md).

## Settings for testing

`OPENSKY_TOKEN_URL` and `OPENSKY_API_URL` also exist. They point Airspace at
a fake OpenSky for development and tests. Do not set them in normal use.
