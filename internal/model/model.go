// Package model holds the normalised aircraft snapshot shared by the poller,
// the API, and storage.
package model

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"

	"github.com/goodtune/airspace/internal/scope"
)

// Aircraft is one state vector. Unknown numbers are NaN.
//
// In JSON it is an array, to keep snapshots compact:
// [icao24, callsign, country, lon, lat, baro, geo, onGround, velocity,
// track, verticalRate, squawk, timePosition, lastContact].
type Aircraft struct {
	ICAO24      string
	Callsign    string
	Country     string
	Lon, Lat    float64
	Baro, Geo   float64 // metres
	OnGround    bool
	Velocity    float64 // m/s
	Track       float64 // degrees clockwise from north
	VRate       float64 // m/s
	Squawk      string
	TimePos     int64 // unix seconds
	LastContact int64 // unix seconds
}

// Fields names the positions in the JSON array. It is served to clients.
var Fields = []string{"icao24", "callsign", "country", "lon", "lat", "baro", "geo", "onGround",
	"velocity", "track", "vrate", "squawk", "timePos", "lastContact"}

func appendNum(b []byte, f float64) []byte {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return append(b, "null"...)
	}
	return strconv.AppendFloat(b, f, 'f', -1, 64)
}

func appendStr(b []byte, s string) []byte {
	q, _ := json.Marshal(s)
	return append(b, q...)
}

// MarshalJSON writes the compact array form.
func (a Aircraft) MarshalJSON() ([]byte, error) {
	b := make([]byte, 0, 128)
	b = append(b, '[')
	b = appendStr(b, a.ICAO24)
	b = append(b, ',')
	b = appendStr(b, a.Callsign)
	b = append(b, ',')
	b = appendStr(b, a.Country)
	for _, f := range []float64{a.Lon, a.Lat, a.Baro, a.Geo} {
		b = append(b, ',')
		b = appendNum(b, f)
	}
	b = append(b, ',')
	b = strconv.AppendBool(b, a.OnGround)
	for _, f := range []float64{a.Velocity, a.Track, a.VRate} {
		b = append(b, ',')
		b = appendNum(b, f)
	}
	b = append(b, ',')
	b = appendStr(b, a.Squawk)
	b = append(b, ',')
	b = strconv.AppendInt(b, a.TimePos, 10)
	b = append(b, ',')
	b = strconv.AppendInt(b, a.LastContact, 10)
	return append(b, ']'), nil
}

// UnmarshalJSON reads the compact array form.
func (a *Aircraft) UnmarshalJSON(data []byte) error {
	var v []any
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}
	if len(v) < len(Fields) {
		return fmt.Errorf("aircraft array has %d fields, want %d", len(v), len(Fields))
	}
	*a = Aircraft{
		ICAO24: str(v[0]), Callsign: str(v[1]), Country: str(v[2]),
		Lon: num(v[3]), Lat: num(v[4]), Baro: num(v[5]), Geo: num(v[6]),
		OnGround: v[7] == true,
		Velocity: num(v[8]), Track: num(v[9]), VRate: num(v[10]),
		Squawk: str(v[11]), TimePos: int64(numOr(v[12], 0)), LastContact: int64(numOr(v[13], 0)),
	}
	return nil
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func num(v any) float64 { return numOr(v, math.NaN()) }

func numOr(v any, def float64) float64 {
	if f, ok := v.(float64); ok {
		return f
	}
	return def
}

// Snapshot is the set of aircraft from one poll.
type Snapshot struct {
	Version  int64      `json:"version"`
	Time     int64      `json:"time"`    // OpenSky data time, unix seconds
	Fetched  int64      `json:"fetched"` // when the server received it, unix milliseconds
	Scope    scope.Plan `json:"scope"`
	Fields   []string   `json:"fields"`
	Aircraft []Aircraft `json:"aircraft"`
}

// Encode returns the snapshot as gzip-compressed JSON, the stored blob form.
func (s *Snapshot) Encode() ([]byte, error) {
	var buf bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err := json.NewEncoder(zw).Encode(s); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Decode reads a blob written by Encode.
func Decode(blob []byte) (*Snapshot, error) {
	zr, err := gzip.NewReader(bytes.NewReader(blob))
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	raw, err := io.ReadAll(zr)
	if err != nil {
		return nil, err
	}
	var s Snapshot
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// ParseStates reads an OpenSky /states/all response body. Aircraft with no
// position are dropped.
func ParseStates(body []byte) (dataTime int64, aircraft []Aircraft, err error) {
	var resp struct {
		Time   int64   `json:"time"`
		States [][]any `json:"states"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return 0, nil, err
	}
	if resp.Time == 0 {
		return 0, nil, errors.New("response has no time")
	}
	aircraft = make([]Aircraft, 0, len(resp.States))
	for _, v := range resp.States {
		if len(v) < 15 {
			continue
		}
		lon, lat := num(v[5]), num(v[6])
		if math.IsNaN(lon) || math.IsNaN(lat) {
			continue
		}
		aircraft = append(aircraft, Aircraft{
			ICAO24: str(v[0]), Callsign: strings.TrimSpace(str(v[1])), Country: str(v[2]),
			TimePos: int64(numOr(v[3], 0)), LastContact: int64(numOr(v[4], 0)),
			Lon: lon, Lat: lat, Baro: num(v[7]), OnGround: v[8] == true,
			Velocity: num(v[9]), Track: num(v[10]), VRate: num(v[11]),
			Geo: num(v[13]), Squawk: str(v[14]),
		})
	}
	return resp.Time, aircraft, nil
}
