package credentials

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type fakeReader struct {
	fields map[string]string
	err    error
	asked  []string
}

func (f *fakeReader) ReadUserSecret(_ context.Context, service, field string) (string, bool, error) {
	f.asked = append(f.asked, service+"/"+field)
	if f.err != nil {
		return "", false, f.err
	}
	v, ok := f.fields[field]
	return v, ok, nil
}

func TestRead(t *testing.T) {
	r := &fakeReader{fields: map[string]string{"clientId": "my-client ", "clientSecret": "s3cret\n"}}
	id, secret, err := Read(context.Background(), r, "opensky")
	if err != nil || id != "my-client" || secret != "s3cret" {
		t.Fatalf("got %q %q %v", id, secret, err)
	}
	if strings.Join(r.asked, ",") != "opensky/clientId,opensky/clientSecret" {
		t.Errorf("read %v", r.asked)
	}
}

func TestConfigPath(t *testing.T) {
	home := func() (string, error) { return "/home/gary", nil }
	user := "/home/gary/.config/dotvault/config.yaml"
	has := func(paths ...string) func(string) bool {
		return func(p string) bool {
			for _, x := range paths {
				if x == p {
					return true
				}
			}
			return false
		}
	}
	for name, tc := range map[string]struct {
		exists func(string) bool
		want   string
	}{
		"system file wins":       {has("/etc/dv.yaml", user), "/etc/dv.yaml"},
		"falls back to the user": {has(user), user},
		"neither: system path":   {has(), "/etc/dv.yaml"},
	} {
		if got := ConfigPath("/etc/dv.yaml", home, tc.exists); got != tc.want {
			t.Errorf("%s: got %q, want %q", name, got, tc.want)
		}
	}
}

func TestReadFailures(t *testing.T) {
	for name, tc := range map[string]struct {
		r    *fakeReader
		want string
	}{
		"no secret at all": {&fakeReader{}, "no clientId field"},
		"missing secret":   {&fakeReader{fields: map[string]string{"clientId": "x"}}, "no clientSecret field"},
		"empty field":      {&fakeReader{fields: map[string]string{"clientId": "x", "clientSecret": "  "}}, "no clientSecret field"},
		"vault error":      {&fakeReader{err: errors.New("permission denied")}, "permission denied"},
	} {
		_, _, err := Read(context.Background(), tc.r, "opensky")
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got %v, want error containing %q", name, err, tc.want)
		}
	}
	// A value that was read never shows up in a later error.
	r := &fakeReader{fields: map[string]string{"clientId": "visible-id-value"}}
	if _, _, err := Read(context.Background(), r, "opensky"); strings.Contains(err.Error(), "visible-id-value") {
		t.Error("error leaks a value")
	}
}
