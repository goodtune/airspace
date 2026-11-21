# Installation

## Releases

Each [release](https://github.com/goodtune/airspace/releases) has:

- archives for Linux, macOS and Windows, on amd64 and arm64
- `rpm`, `deb` and `apk` packages for Linux
- a container image at `ghcr.io/goodtune/airspace`

## Linux packages

The packages install:

- `/usr/bin/airspace`
- a systemd unit, `airspace.service`, which is disabled until you enable it
- `/etc/airspace/airspace.env` for the settings, readable by root only

Put your OpenSky credentials in `/etc/airspace/airspace.env`, then:

```sh
sudo systemctl enable --now airspace.service
```

The service runs as a dynamic user. The database is in `/var/lib/airspace`.
For [HTTPS](https.md), uncomment the two capability lines in the unit so it
can open ports 80 and 443 (`sudo systemctl edit airspace.service`).

## Container

```sh
docker run -p 8080:8080 -v airspace-data:/data \
  -e OPENSKY_CLIENT_ID=... -e OPENSKY_CLIENT_SECRET=... \
  ghcr.io/goodtune/airspace
```

The database is at `/data/airspace.db`. Mount a volume there to keep
recordings.

## From source

`make build` makes static binaries for linux/amd64 and linux/arm64 in `bin/`.
`make local` makes one for the machine you are on. `Dockerfile` builds an
image from source. See [Development](../development.md).
