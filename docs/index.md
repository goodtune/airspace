# Airspace

Airspace shows live aircraft on a 3D globe. Everyone connected sees the same
session: the same camera, the same selected aircraft, the same refresh rate.
It records what it shows, replays it later, and exports replays as video.

<video src="assets/demo.webm" controls autoplay loop muted playsinline width="100%"></video>

It is one Go binary for personal use on a private network. Aircraft data
comes from the [OpenSky Network](https://opensky-network.org). Imagery is
NASA Blue Marble from GIBS.

## Where to start

- [Getting started](getting-started.md): create an OpenSky client and run it.
- [How it works](how-it-works.md): one shared session, and no requests when
  nobody is watching.

## Features

- [Credits and query scope](features/credits.md): what each poll costs, and
  the limits that keep you inside the daily allowance.
- [Recording](features/recording.md): what is stored, and how much space it
  takes.
- [Replay and video export](features/replay.md): watch a session again and
  export it as MP4.

- [Finding bad data](features/anomalies.md): list the moments in a recording
  that cannot be right, and watch them back.

## Administration

- [Installation](admin/installation.md): packages, container, systemd.
- [Configuration](admin/configuration.md): every setting.
- [HTTPS](admin/https.md): certificates from Let's Encrypt or a private ACME
  server.
- [Monitoring](admin/monitoring.md): OpenTelemetry metrics.

## Reference

- [HTTP API](api.md)
- [Development](development.md): tests, tooling, releases.
- [Attribution](attribution.md)
