# Monitoring

Airspace emits OpenTelemetry metrics over OTLP. Export is off until you
point it at a collector with the standard variables:

```sh
OTEL_EXPORTER_OTLP_ENDPOINT=http://collector.example.org:4318 ./bin/airspace
```

It honours the usual `OTEL_*` settings: `OTEL_EXPORTER_OTLP_PROTOCOL`
(`http/protobuf` by default, or `grpc`), `OTEL_EXPORTER_OTLP_HEADERS`,
`OTEL_METRIC_EXPORT_INTERVAL`, `OTEL_SERVICE_NAME` (default `airspace`),
`OTEL_RESOURCE_ATTRIBUTES`, and `OTEL_SDK_DISABLED`. Sending metrics is not
an OpenSky request and does not depend on anyone watching.

| Metric | Type | Attributes | Meaning |
|---|---|---|---|
| `airspace.build_info` | gauge | `version`, `revision`, `goversion` | Always 1. Identifies the running build. |
| `airspace.opensky.requests` | counter | `endpoint` (`states`, `token`), `outcome` (`ok`, `rate_limited`, `unauthorized`, `error`), `http.response.status_code` | Every request made to OpenSky, token requests included |
| `airspace.opensky.credits.remaining` | gauge | | The balance from `X-Rate-Limit-Remaining`, recorded with the request that returned it |
| `airspace.opensky.credits.spent` | counter | | Credits spent, from the area of each successful query |
| `airspace.opensky.request.duration` | histogram | `endpoint` | Time taken by each OpenSky request |
| `airspace.polls` | counter | `outcome`, `coverage` (`viewport`, `global`) | Poll cycles. One poll can make several requests. |
| `airspace.snapshot.aircraft` | gauge | | Aircraft in the latest snapshot |
| `airspace.snapshot.age` | gauge | | Seconds since the last good snapshot |
| `airspace.poll.interval` | gauge | | Effective poll interval, seconds |
| `airspace.poll.cost` | gauge | | Credit cost of one poll at the current scope |
| `airspace.poller.state` | gauge | `state` (`polling`, `paused`, `stopped`) | 1 for the current state, 0 for the others |
| `airspace.clients.connected` | gauge | | Browsers holding the event stream |
| `airspace.clients.active` | gauge | | Browsers counted as watching live |
| `airspace.state.changes` | counter | `type` | Changes to shared state: camera, interval, selected, coverage, scope, and so on |
| `airspace.http.server.requests` | counter | `http.route`, `http.request.method`, `http.response.status_code` | Every HTTP request served. The route is the pattern, such as `/api/sessions/{id}`; files are `static`. |
| `airspace.http.server.request.duration` | histogram | `http.route` | Time to serve each request. The event stream is left out. |
| `airspace.recording.snapshots`, `.bytes` | counter | | Snapshots, and compressed bytes, written to the database |
| `airspace.recording.failures` | counter | `reason` (`error`, `dropped`) | Recording writes that failed or were dropped |
| `airspace.recording.sessions.pruned` | counter | | Sessions deleted by retention |
| `airspace.recording.active`, `.healthy` | gauge | | 1 while recording, and 1 while storage works |
| `airspace.recording.database.size` | gauge | | Database size, bytes |
