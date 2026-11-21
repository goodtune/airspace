# Development

## Tests

```sh
make test        # Go tests, with the race detector
node --test web/test/geo.test.js    # motion and interpolation maths
make acme-test   # a real certificate exchange against Pebble, a test ACME server
make e2e         # browser tests (Playwright), in e2e/
```

All tests use a fake OpenSky that counts every request, token requests
included. No test calls the real API.

`.github/workflows/ci.yml` runs all four on every push to `main` and on every
pull request. CI machines have no GPU, so the browser tests there draw in
software on a smaller page, and take about 20 minutes.

## Working without spending credits

```sh
make dev-fake    # terminal 1: a fake OpenSky replaying captured world traffic
make dev         # terminal 2: Airspace against it, on :8080
make seed        # optional: load the capture into dev.db as a session to replay
```

Add `?replay=latest` to the address to open straight into replay of the
newest session, or `?replay=<session id>` for a particular one. Add
`?imagery=off` to draw a plain grid in place of NASA imagery; the browser
tests use it so they need no internet.

## Test data

`testdata/capture/` holds 87 whole-world snapshots at 10 s spacing (14.5
minutes), stored with Git LFS. `cmd/capture` records more; it stops before
the balance runs out.

`make demo` renders the clip on the front page from that data, through
Airspace's own video export. It needs `ffmpeg` and internet for imagery, and
currently a Mac.

## Frontend libraries

CesiumJS, Datastar and mp4-muxer are fetched at pinned versions by
`scripts/vendor.sh` and embedded in the binary. Nothing loads from a CDN at
run time. The only thing a browser fetches from the internet is imagery tiles
from NASA GIBS.

## Releases

Publishing a GitHub release runs GoReleaser (`.github/workflows/release.yml`,
`.goreleaser.yaml`). It builds the archives, the Linux packages and the
container image, with the version from the tag. `make snapshot` builds the
same artefacts locally, apart from the image, without publishing.
