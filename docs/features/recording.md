# Recording

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

## Storage figures

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
