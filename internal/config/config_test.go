package config

import (
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) func(string) string {
	base := map[string]string{"OPENSKY_CLIENT_ID": "id", "OPENSKY_CLIENT_SECRET": "s3cret"}
	for k, v := range m {
		base[k] = v
	}
	return func(k string) string { return base[k] }
}

func TestDefaults(t *testing.T) {
	c, err := Load(nil, env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if c.PollInterval != 10*time.Second || !c.MinAuto || c.MaxInterval != 0 || c.DailyCredits != 4000 ||
		c.DailyUseHours != 2 || c.CreditReserve != 200 || c.HiddenTimeout != time.Minute ||
		c.IdleTimeout != 30*time.Minute || c.ListenAddr != ":8080" || c.DBPath != "./airspace.db" {
		t.Errorf("unexpected defaults: %+v", c)
	}
}

func TestFlagsOverrideEnv(t *testing.T) {
	c, err := Load([]string{"-poll-interval", "20s", "-listen-addr", ":9"}, env(map[string]string{"POLL_INTERVAL": "15s"}))
	if err != nil {
		t.Fatal(err)
	}
	if c.PollInterval != 20*time.Second || c.ListenAddr != ":9" {
		t.Errorf("flags did not override: %+v", c)
	}
}

func TestValidationFailures(t *testing.T) {
	for name, c := range map[string]struct {
		env  map[string]string
		want string
	}{
		"no id":           {map[string]string{"OPENSKY_CLIENT_ID": ""}, "required"},
		"no secret":       {map[string]string{"OPENSKY_CLIENT_SECRET": ""}, "required"},
		"min below 5s":    {map[string]string{"MIN_INTERVAL": "4s"}, "below 5s"},
		"min above max":   {map[string]string{"MIN_INTERVAL": "30s", "MAX_INTERVAL": "20s"}, "greater than MAX_INTERVAL"},
		"hours negative":  {map[string]string{"DAILY_USE_HOURS": "-1"}, "0 to 24"},
		"hours too large": {map[string]string{"DAILY_USE_HOURS": "25"}, "0 to 24"},
		"bad duration":    {map[string]string{"POLL_INTERVAL": "soon"}, "not a valid duration"},
	} {
		_, err := Load(nil, env(c.env))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, want error containing %q", name, err, c.want)
		}
		if err != nil && strings.Contains(err.Error(), "s3cret") {
			t.Errorf("%s: error leaks the secret", name)
		}
	}
}

func TestPollIntervalClamped(t *testing.T) {
	c, err := Load(nil, env(map[string]string{"POLL_INTERVAL": "10s", "MIN_INTERVAL": "30s"}))
	if err != nil {
		t.Fatal(err)
	}
	if c.PollInterval != 30*time.Second || len(c.Notes) != 1 {
		t.Errorf("want clamp to 30s with a note, got %s %v", c.PollInterval, c.Notes)
	}
	c, _ = Load(nil, env(map[string]string{"POLL_INTERVAL": "90s", "MAX_INTERVAL": "60s"}))
	if c.PollInterval != 60*time.Second {
		t.Errorf("want clamp to 60s, got %s", c.PollInterval)
	}
}

func TestAutoFloor(t *testing.T) {
	for hours, want := range map[string][4]int{"2": {5, 5, 6, 8}, "24": {23, 46, 69, 91}} {
		c, err := Load(nil, env(map[string]string{"DAILY_USE_HOURS": hours}))
		if err != nil {
			t.Fatal(err)
		}
		for cost := 1; cost <= 4; cost++ {
			if got := c.Floor(cost); got != time.Duration(want[cost-1])*time.Second {
				t.Errorf("hours %s cost %d: floor %s, want %ds", hours, cost, got, want[cost-1])
			}
		}
	}
}

// Two hours at the floor in every tier must fit the allowance less reserve.
func TestTwoHoursAtFloorFits(t *testing.T) {
	c, _ := Load(nil, env(nil))
	for cost := 1; cost <= 4; cost++ {
		used := 2 * 3600 / c.Floor(cost).Seconds() * float64(cost)
		if used > float64(c.DailyCredits-c.CreditReserve) {
			t.Errorf("cost %d: two hours uses %.0f credits", cost, used)
		}
	}
}

func TestClamp(t *testing.T) {
	c, _ := Load(nil, env(map[string]string{"MIN_INTERVAL": "30s", "MAX_INTERVAL": "2m"}))
	for _, tc := range []struct {
		in, want time.Duration
		reason   string
	}{
		{10 * time.Second, 30 * time.Second, ReasonMin},
		{45 * time.Second, 45 * time.Second, ""},
		{5 * time.Minute, 2 * time.Minute, ReasonMax},
	} {
		got, reason := c.Clamp(tc.in, 1)
		if got != tc.want || reason != tc.reason {
			t.Errorf("Clamp(%s) = %s %q, want %s %q", tc.in, got, reason, tc.want, tc.reason)
		}
	}
}

func TestParseSize(t *testing.T) {
	for in, want := range map[string]int64{"500MB": 500 << 20, "2GB": 2 << 30, "1024": 1024, "1.5G": 3 << 29} {
		if got, err := ParseSize(in); err != nil || got != want {
			t.Errorf("ParseSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
}

func TestACME(t *testing.T) {
	c, err := Load(nil, env(nil))
	if err != nil || len(c.ACMEDomains) != 0 || c.ListenAddr != ":8080" {
		t.Fatalf("TLS must be off by default: %+v %v", c, err)
	}
	c, err = Load(nil, env(map[string]string{"ACME_DOMAINS": "Air.Example.org, b.example.org", "DB_PATH": "/var/lib/airspace/a.db"}))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.ACMEDomains) != 2 || c.ACMEDomains[0] != "air.example.org" || c.ACMEDirectoryURL != LetsEncryptURL ||
		c.TLSListenAddr != ":443" || c.ListenAddr != ":80" || c.ACMECacheDir != "/var/lib/airspace/acme" || c.ACMERenewBefore != 0 {
		t.Errorf("ACME defaults: %+v", c)
	}
	// An explicit plain port is kept.
	c, _ = Load([]string{"-listen-addr", ":8080"}, env(map[string]string{"ACME_DOMAINS": "a.example.org"}))
	if c.ListenAddr != ":8080" {
		t.Errorf("explicit LISTEN_ADDR was overridden: %s", c.ListenAddr)
	}
	c, err = Load(nil, env(map[string]string{"ACME_DOMAINS": "a.example.org", "ACME_EAB_KID": "kid", "ACME_EAB_HMAC_KEY": "c2VjcmV0LWtleQ", "ACME_RENEW_BEFORE": "48h"}))
	if err != nil || string(c.ACMEEABKey) != "secret-key" || c.ACMERenewBefore != 48*time.Hour {
		t.Errorf("EAB and renew: %+v %v", c, err)
	}
	for name, tc := range map[string]struct {
		env  map[string]string
		want string
	}{
		"wildcard":  {map[string]string{"ACME_DOMAINS": "*.example.org"}, "wildcards are not supported"},
		"http url":  {map[string]string{"ACME_DOMAINS": "a.example.org", "ACME_DIRECTORY_URL": "http://ca/dir"}, "https URL"},
		"half EAB":  {map[string]string{"ACME_DOMAINS": "a.example.org", "ACME_EAB_KID": "kid"}, "set together"},
		"bad EAB":   {map[string]string{"ACME_DOMAINS": "a.example.org", "ACME_EAB_KID": "kid", "ACME_EAB_HMAC_KEY": "!!!"}, "base64url"},
		"bad renew": {map[string]string{"ACME_DOMAINS": "a.example.org", "ACME_RENEW_BEFORE": "soon"}, "ACME_RENEW_BEFORE"},
	} {
		if _, err := Load(nil, env(tc.env)); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got %v, want error containing %q", name, err, tc.want)
		}
	}
}
