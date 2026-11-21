// Package credentials loads the OpenSky client id and secret from dotvault,
// as an alternative to passing them in environment variables.
package credentials

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	dotvault "github.com/goodtune/dotvault/client"
)

// The fields a dotvault secret must hold.
const (
	FieldClientID     = "clientId"
	FieldClientSecret = "clientSecret"
)

// Reader reads one field of a per-user secret. *dotvault.Client is one.
type Reader interface {
	ReadUserSecret(ctx context.Context, service, field string) (value string, found bool, err error)
}

// FromDotvault reads the credentials from the named dotvault secret, using
// the machine's dotvault configuration. It never opens a browser or prompts:
// this is a server, so it uses only a login that already exists.
//
// configPath is dotvault's own configuration file. When it is empty the
// system file is used, or failing that the per-user one.
func FromDotvault(ctx context.Context, service, configPath string) (id, secret string, err error) {
	if configPath == "" {
		configPath = ConfigPath(dotvault.DefaultConfigPath(), os.UserHomeDir, exists)
	}
	cfg, err := dotvault.LoadConfig(configPath)
	if err != nil {
		return "", "", fmt.Errorf("dotvault: reading its configuration (set DOTVAULT_CONFIG if it is somewhere else): %w", err)
	}
	cli, err := dotvault.New(cfg)
	if err != nil {
		return "", "", fmt.Errorf("dotvault: %w", err)
	}
	if err := cli.AuthenticateCached(ctx); err != nil {
		switch {
		case errors.Is(err, dotvault.ErrLoginRequired):
			return "", "", fmt.Errorf("dotvault has no usable login for this user; run `dotvault login` and start again: %w", err)
		case errors.Is(err, dotvault.ErrUnreachable):
			return "", "", fmt.Errorf("dotvault cannot reach Vault: %w", err)
		}
		return "", "", fmt.Errorf("dotvault: %w", err)
	}
	return Read(ctx, cli, service)
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// ConfigPath picks dotvault's configuration file: the system one when it is
// there, else ~/.config/dotvault/config.yaml when that is. With neither, it
// returns the system path so the error names the usual place.
func ConfigPath(system string, home func() (string, error), exists func(string) bool) string {
	if exists(system) {
		return system
	}
	if h, err := home(); err == nil {
		if user := filepath.Join(h, ".config", "dotvault", "config.yaml"); exists(user) {
			return user
		}
	}
	return system
}

// Read takes the two fields from a secret. The values never appear in an
// error.
func Read(ctx context.Context, r Reader, service string) (id, secret string, err error) {
	get := func(field string) (string, error) {
		v, found, err := r.ReadUserSecret(ctx, service, field)
		switch {
		case err != nil:
			return "", fmt.Errorf("dotvault: reading %s from %q: %w", field, service, err)
		case !found || strings.TrimSpace(v) == "":
			return "", fmt.Errorf("dotvault secret %q has no %s field; it needs %s and %s", service, field, FieldClientID, FieldClientSecret)
		}
		return strings.TrimSpace(v), nil
	}
	if id, err = get(FieldClientID); err != nil {
		return "", "", err
	}
	if secret, err = get(FieldClientSecret); err != nil {
		return "", "", err
	}
	return id, secret, nil
}
