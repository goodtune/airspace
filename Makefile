# Airspace build. `make build` produces static linux binaries with the
# frontend embedded.

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
GOFLAGS := -trimpath -ldflags "-s -w"
CAPTURE := testdata/capture/global-2026-11-21.jsonl.gz

.PHONY: build local vendor test e2e dev-fake dev seed clean

build: vendor
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build $(GOFLAGS) -o bin/airspace-linux-amd64 ./cmd/airspace
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build $(GOFLAGS) -o bin/airspace-linux-arm64 ./cmd/airspace

# A binary for the machine you are on.
local: vendor
	CGO_ENABLED=0 go build $(GOFLAGS) -o bin/airspace ./cmd/airspace

# Fetch the pinned frontend libraries that get embedded.
vendor:
	./scripts/vendor.sh

test:
	go vet ./...
	go test -race ./...

# Browser tests. They run the real binary against a fake OpenSky.
e2e: local
	go build -o bin/devtool ./cmd/devtool
	cd e2e && npx playwright test --trace off

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
