# Airspace build. `make build` produces static linux binaries with the
# frontend embedded.

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
GOFLAGS := -trimpath -ldflags "-s -w -X main.version=$(VERSION)"
CAPTURE := testdata/capture/global-2026-11-21.jsonl.gz

.PHONY: build local vendor test acme-test e2e demo snapshot dev-fake dev seed clean

build: vendor
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build $(GOFLAGS) -o bin/airspace-linux-amd64 ./cmd/airspace
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build $(GOFLAGS) -o bin/airspace-linux-arm64 ./cmd/airspace

# A binary for the machine you are on.
local: vendor
	CGO_ENABLED=0 go build $(GOFLAGS) -o bin/airspace ./cmd/airspace

# Build every release artefact locally (archives, packages) without
# publishing anything. Releases themselves are made by GoReleaser in CI when a
# GitHub release is published.
snapshot:
	goreleaser release --snapshot --clean --skip=docker

# Fetch the pinned frontend libraries that get embedded.
vendor:
	./scripts/vendor.sh

test:
	go vet ./...
	go test -race ./...

# A real certificate exchange against Pebble, Let's Encrypt's test ACME
# server. Pebble is a test tool only; it is not part of the binary.
PEBBLE_VERSION := v2.10.3
bin/pebble:
	GOBIN=$(CURDIR)/bin go install github.com/letsencrypt/pebble/v2/cmd/pebble@$(PEBBLE_VERSION)

acme-test: bin/pebble
	PEBBLE=$(CURDIR)/bin/pebble go test -race -count=1 -tags acme -run ACME ./internal/app

# Browser tests. They run the real binary against a fake OpenSky.
e2e: local
	go build -o bin/devtool ./cmd/devtool
	cd e2e && npx playwright test --trace off

# Render the README clip from the captured data. Needs ffmpeg, and internet
# for NASA imagery. Makes no OpenSky request.
demo: local
	go build -o bin/devtool ./cmd/devtool
	cd e2e && node demo.mjs .tmp/demo.mp4
	ffmpeg -v error -y -i e2e/.tmp/demo.mp4 -c:v libvpx-vp9 -b:v 0 -crf 46 -row-mt 1 -an docs/demo.webm

# Run a fake OpenSky that replays the captured global traffic.
dev-fake:
	go run ./cmd/devtool fake -capture $(CAPTURE)

# Run the app against the fake. Costs no credits.
dev: vendor
	OPENSKY_CLIENT_ID=dev OPENSKY_CLIENT_SECRET=dev \
	OPENSKY_TOKEN_URL=http://127.0.0.1:9090/token OPENSKY_API_URL=http://127.0.0.1:9090/api \
	DB_PATH=dev.db go run ./cmd/airspace

# Load the captured traffic into dev.db as a recorded session for replay.
seed:
	go run ./cmd/devtool seed -db dev.db -capture $(CAPTURE)

clean:
	rm -rf bin web/static/vendor
