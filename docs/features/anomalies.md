# Finding bad data

OpenSky data is not always right. An aircraft can appear hundreds of
kilometres from where it is for a snapshot or two, or report a heading it is
plainly not flying. Airspace records and replays exactly what it was given;
it does not correct anything. `airspace anomalies` reads the recordings and
lists the moments that cannot be right, so you can watch them back.

```sh
airspace anomalies -db /var/lib/airspace/airspace.db -url http://localhost:8080
```

```
Session 1795209778001  Whole world (global)
2026-11-21 08:35:13  position_excursion  486492  KLM83K  away for 2 snapshot(s) over 21s, then back on its earlier path: moved 679 km in 3 s ...
                                                         watch: http://localhost:8080/?replay=1795209778001&at=1795210505000&aircraft=486492
2026-11-21 08:37:52  track_mismatch      486492  KLM83K  reports track 46° at 428 km/h, but flew 121° at 358 km/h
                                                         watch: http://localhost:8080/?replay=1795209778001&at=1795210664000&aircraft=486492
```

Open a `watch:` link in a browser. The replay opens a few seconds before the
anomaly, with the aircraft selected and the camera following it. Press Play.

The command reads the database only. It makes no request to OpenSky, changes
no data, and is safe to run while the server is running.

## What it looks for

| Kind | Meaning |
|---|---|
| `position_excursion` | The aircraft jumped away, then jumped back onto the path it was flying before. Reported once, with how long it was away. |
| `position_jump` | The aircraft moved faster than anything flies, and did not come back within 5 minutes. |
| `track_mismatch` | The reported track disagrees with the direction the positions show by more than 60°. When the reported speed and track exactly repeat values the aircraft sent over a minute earlier, the report says so. |
| `altitude_jump` | Altitude changed by more than 1,500 m at more than 150 m/s. |
| `time_reversal` | A snapshot's data time is earlier than the one before it. This is about the snapshot, not one aircraft. |

A move counts as a jump when it is at least 15 km and implies more than
2,520 km/h, about Mach 2. A turning aircraft is not a track mismatch: its
path lies between its old and new tracks, and the check allows for that.

On the 14.5 minutes of captured world traffic (about 1.1 million aircraft
states) it reports a few hundred findings.

## Options

| Flag | Default | Meaning |
|---|---|---|
| `-db` | `DB_PATH`, or `./airspace.db` | The database to read |
| `-session` | `all` | `all`, `latest`, or a session id |
| `-format` | `text` | `text`, `json`, or `csv` |
| `-o` | standard output | Write to this file |
| `-kind` | all kinds | Only these kinds, comma separated |
| `-aircraft` | all aircraft | Only this aircraft, by icao24 or callsign |
| `-url` | none | Address of a running Airspace, to print complete links |
| `-max-speed` | `2520` | Implied speed, km/h, above which a move is a jump |
| `-min-jump-km` | `15` | Shortest move that can be a jump |

The `json` and `csv` forms carry the same findings with exact start and end
times, the number of snapshots affected, the distance for jumps, and the
replay link.

## Replay links

Any replay can be opened at a moment, on an aircraft:

    /?replay=<session id>&at=<unix milliseconds>&aircraft=<icao24>

`replay=latest` opens the newest session. `at` and `aircraft` are optional.
