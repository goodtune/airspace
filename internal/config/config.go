// Package config loads operator settings from environment variables, with
// command-line flags as overrides.
package config

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"time"
)

// Resolution is the time resolution of OpenSky data for authenticated users.
// No interval may be shorter.
const Resolution = 5 * time.Second

const (
	DefaultTokenURL = "https://auth.opensky-network.org/auth/realms/opensky-network/protocol/openid-connect/token"
	DefaultAPIURL   = "https://opensky-network.org/api"
)

// Config holds every operator setting.
type Config struct {
	ClientID      string
	ClientSecret  string
	ListenAddr    string
	PollInterval  time.Duration
	MinAuto       bool          // MIN_INTERVAL=auto
	MinInterval   time.Duration // fixed floor when MinAuto is false
	MaxInterval   time.Duration // 0 means unset
	DailyCredits  int
	DailyUseHours float64
	CreditReserve int
	HiddenTimeout time.Duration
	IdleTimeout   time.Duration // 0 disables
	DBPath        string
	MaxDBSize     int64      // bytes, 0 means unset
	RetentionDays int        // 0 means unset
	StartView     [3]float64 // longitude, latitude, height in metres

	// Upstream endpoints. Overridden only to point at a fake for testing.
	TokenURL string
	APIURL   string

	// Resolution is the hard lower limit for any interval. Tests shorten it.
	Resolution time.Duration
	// SessionGrace is how long the active count must be zero before a
	// session ends.
	SessionGrace time.Duration

	// Notes holds messages about adjustments made while loading, for the log.
	Notes []string
}

type setting struct {
	env, flag, def, help string
}

var settings = []setting{
	{"OPENSKY_CLIENT_ID", "opensky-client-id", "", "OAuth2 client id (required)"},
	{"OPENSKY_CLIENT_SECRET", "opensky-client-secret", "", "OAuth2 client secret (required)"},
	{"LISTEN_ADDR", "listen-addr", ":8080", "HTTP listen address"},
	{"POLL_INTERVAL", "poll-interval", "10s", "startup default for the shared interval"},
	{"MIN_INTERVAL", "min-interval", "auto", "shortest interval a client may set: auto or a duration"},
	{"MAX_INTERVAL", "max-interval", "", "longest interval a client may set"},
	{"DAILY_CREDITS", "daily-credits", "4000", "daily credit allowance"},
	{"DAILY_USE_HOURS", "daily-use-hours", "2", "expected hours of use per day"},
	{"CREDIT_RESERVE", "credit-reserve", "200", "credits the guard tries not to go below"},
	{"HIDDEN_TIMEOUT", "hidden-timeout", "60s", "a hidden tab stops counting as active after this"},
	{"IDLE_TIMEOUT", "idle-timeout", "30m", "pause when no client has had input for this long; 0 disables"},
	{"DB_PATH", "db-path", "./airspace.db", "SQLite database file"},
	{"MAX_DB_SIZE", "max-db-size", "", "prune oldest sessions above this size, for example 2GB"},
	{"RETENTION_DAYS", "retention-days", "", "prune sessions older than this many days"},
	{"START_VIEW", "start-view", "151.2,-33.9,450000", "opening camera: longitude,latitude,height in metres"},
	{"OPENSKY_TOKEN_URL", "opensky-token-url", DefaultTokenURL, "token endpoint (testing only)"},
	{"OPENSKY_API_URL", "opensky-api-url", DefaultAPIURL, "API root (testing only)"},
}

// Load reads settings from getenv, then applies flags from args on top.
func Load(args []string, getenv func(string) string) (*Config, error) {
	fs := flag.NewFlagSet("airspace", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	vals := map[string]*string{}
	for _, s := range settings {
		def := s.def
		if v := getenv(s.env); v != "" {
			def = v
		}
		vals[s.env] = fs.String(s.flag, def, s.help+" (env "+s.env+")")
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			var b strings.Builder
			fs.SetOutput(&b)
			fs.PrintDefaults()
			return nil, fmt.Errorf("%w\nusage: airspace [flags]\n%s", flag.ErrHelp, redact(b.String(), *vals["OPENSKY_CLIENT_SECRET"]))
		}
		return nil, err
	}
	get := func(k string) string { return strings.TrimSpace(*vals[k]) }

	c := &Config{
		ClientID:     get("OPENSKY_CLIENT_ID"),
		ClientSecret: get("OPENSKY_CLIENT_SECRET"),
		ListenAddr:   get("LISTEN_ADDR"),
		DBPath:       get("DB_PATH"),
		TokenURL:     get("OPENSKY_TOKEN_URL"),
		APIURL:       strings.TrimRight(get("OPENSKY_API_URL"), "/"),
		Resolution:   Resolution,
		SessionGrace: 2 * time.Minute,
	}
	if c.ClientID == "" || c.ClientSecret == "" {
		return nil, errors.New("OPENSKY_CLIENT_ID and OPENSKY_CLIENT_SECRET are required: create an API client on your OpenSky account page")
	}

	var err error
	dur := func(k string, into *time.Duration, allowZero bool) {
		if err != nil || get(k) == "" {
			return
		}
		d, e := parseDuration(get(k))
		if e != nil || d < 0 || (d == 0 && !allowZero) {
			err = fmt.Errorf("%s: %q is not a valid duration", k, get(k))
			return
		}
		*into = d
	}
	dur("POLL_INTERVAL", &c.PollInterval, false)
	dur("MAX_INTERVAL", &c.MaxInterval, false)
	dur("HIDDEN_TIMEOUT", &c.HiddenTimeout, true)
	dur("IDLE_TIMEOUT", &c.IdleTimeout, true)
	if strings.EqualFold(get("MIN_INTERVAL"), "auto") {
		c.MinAuto = true
	} else {
		dur("MIN_INTERVAL", &c.MinInterval, false)
	}
	if err != nil {
		return nil, err
	}

	if c.DailyCredits, err = strconv.Atoi(get("DAILY_CREDITS")); err != nil || c.DailyCredits <= 0 {
		return nil, fmt.Errorf("DAILY_CREDITS: %q is not a positive whole number", get("DAILY_CREDITS"))
	}
	if c.CreditReserve, err = strconv.Atoi(get("CREDIT_RESERVE")); err != nil || c.CreditReserve < 0 {
		return nil, fmt.Errorf("CREDIT_RESERVE: %q is not a whole number", get("CREDIT_RESERVE"))
	}
	if c.CreditReserve >= c.DailyCredits {
		return nil, fmt.Errorf("CREDIT_RESERVE (%d) must be less than DAILY_CREDITS (%d)", c.CreditReserve, c.DailyCredits)
	}
	if c.DailyUseHours, err = strconv.ParseFloat(get("DAILY_USE_HOURS"), 64); err != nil ||
		math.IsNaN(c.DailyUseHours) || c.DailyUseHours < 0 || c.DailyUseHours > 24 {
		return nil, fmt.Errorf("DAILY_USE_HOURS: %q must be a number from 0 to 24", get("DAILY_USE_HOURS"))
	}
	if v := get("RETENTION_DAYS"); v != "" {
		if c.RetentionDays, err = strconv.Atoi(v); err != nil || c.RetentionDays <= 0 {
			return nil, fmt.Errorf("RETENTION_DAYS: %q is not a positive whole number", v)
		}
	}
	if v := get("MAX_DB_SIZE"); v != "" {
		if c.MaxDBSize, err = ParseSize(v); err != nil {
			return nil, fmt.Errorf("MAX_DB_SIZE: %v", err)
		}
	}
	parts := strings.Split(get("START_VIEW"), ",")
	if len(parts) != 3 {
		return nil, fmt.Errorf("START_VIEW: %q must be longitude,latitude,height", get("START_VIEW"))
	}
	for i, p := range parts {
		if c.StartView[i], err = strconv.ParseFloat(strings.TrimSpace(p), 64); err != nil {
			return nil, fmt.Errorf("START_VIEW: %q must be longitude,latitude,height", get("START_VIEW"))
		}
	}

	if !c.MinAuto && c.MinInterval < Resolution {
		return nil, fmt.Errorf("MIN_INTERVAL: %s is below %s, the resolution of OpenSky data", c.MinInterval, Resolution)
	}
	if !c.MinAuto && c.MaxInterval > 0 && c.MinInterval > c.MaxInterval {
		return nil, fmt.Errorf("MIN_INTERVAL (%s) is greater than MAX_INTERVAL (%s)", c.MinInterval, c.MaxInterval)
	}
	if c.MaxInterval > 0 && c.MaxInterval < Resolution {
		return nil, fmt.Errorf("MAX_INTERVAL: %s is below %s, the resolution of OpenSky data", c.MaxInterval, Resolution)
	}

	// Clamp the startup interval against the bounds for the cheapest poll.
	if clamped, _ := c.Clamp(c.PollInterval, 1); clamped != c.PollInterval {
		c.Notes = append(c.Notes, fmt.Sprintf("POLL_INTERVAL %s is outside the bounds; clamped to %s", c.PollInterval, clamped))
		c.PollInterval = clamped
	}
	return c, nil
}

// Floor is the shortest interval allowed for a poll of the given cost.
func (c *Config) Floor(cost int) time.Duration {
	if !c.MinAuto {
		return c.MinInterval
	}
	if cost < 1 {
		cost = 1
	}
	res := c.Resolution
	if res <= 0 {
		res = Resolution
	}
	secs := c.DailyUseHours * 3600 * float64(cost) / float64(c.DailyCredits-c.CreditReserve)
	// The tiny subtraction keeps float noise from rounding an exact value up.
	floor := time.Duration(math.Ceil(secs-1e-9)) * time.Second
	if floor < res {
		floor = res
	}
	return floor
}

// Ceiling is the longest interval a client may set for a poll of the given
// cost, or 0 when there is none. It is never below the floor.
func (c *Config) Ceiling(cost int) time.Duration {
	if c.MaxInterval == 0 {
		return 0
	}
	if f := c.Floor(cost); f > c.MaxInterval {
		return f
	}
	return c.MaxInterval
}

// Reasons an interval differs from what the client asked for.
const (
	ReasonMin     = "operator minimum"
	ReasonMax     = "operator maximum"
	ReasonReserve = "credit reserve"
	ReasonLimited = "rate limited"
)

// Clamp applies the operator bounds to a requested interval.
func (c *Config) Clamp(d time.Duration, cost int) (time.Duration, string) {
	if f := c.Floor(cost); d < f {
		return f, ReasonMin
	}
	if m := c.Ceiling(cost); m > 0 && d > m {
		return m, ReasonMax
	}
	return d, ""
}

// parseDuration accepts Go durations, and bare numbers as seconds.
func parseDuration(s string) (time.Duration, error) {
	if n, err := strconv.ParseFloat(s, 64); err == nil {
		return time.Duration(n * float64(time.Second)), nil
	}
	return time.ParseDuration(s)
}

// ParseSize reads a byte size such as 500MB or 2GB.
func ParseSize(s string) (int64, error) {
	u := strings.ToUpper(strings.TrimSpace(s))
	mult := int64(1)
	for _, suf := range []struct {
		s string
		m int64
	}{{"KB", 1 << 10}, {"MB", 1 << 20}, {"GB", 1 << 30}, {"TB", 1 << 40}, {"K", 1 << 10}, {"M", 1 << 20}, {"G", 1 << 30}, {"B", 1}} {
		if strings.HasSuffix(u, suf.s) {
			mult, u = suf.m, strings.TrimSuffix(u, suf.s)
			break
		}
	}
	n, err := strconv.ParseFloat(strings.TrimSpace(u), 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%q is not a valid size", s)
	}
	return int64(n * float64(mult)), nil
}

func redact(s, secret string) string {
	if secret == "" {
		return s
	}
	return strings.ReplaceAll(s, secret, "[redacted]")
}
