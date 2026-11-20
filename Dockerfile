# Build: docker build -t airspace .
# Run:   docker run -p 8080:8080 -v airspace-data:/data \
#          -e OPENSKY_CLIENT_ID=... -e OPENSKY_CLIENT_SECRET=... airspace
FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN ./scripts/vendor.sh \
 && CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /airspace ./cmd/airspace \
 && mkdir /data

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /airspace /airspace
COPY --from=build --chown=nonroot:nonroot /data /data
ENV DB_PATH=/data/airspace.db LISTEN_ADDR=:8080
VOLUME /data
EXPOSE 8080
ENTRYPOINT ["/airspace"]
