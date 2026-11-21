// Package opensky is a small client for the OpenSky Network REST API.
//
// It fetches an OAuth2 token only when a request is about to be made, and
// never in the background.
package opensky

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/goodtune/airspace/internal/model"
	"github.com/goodtune/airspace/internal/scope"
)

// tokenMargin is how long before expiry a cached token stops being used.
const tokenMargin = 30 * time.Second

// Client calls OpenSky with client credentials.
type Client struct {
	HTTP     *http.Client
	TokenURL string
	APIURL   string
	ID       string
	Secret   string

	// Observe, when set, is told about every request made to OpenSky, token
	// requests included. status is 0 and err is set when there was no
	// response. cost is the credit cost of a state query, 0 for a token.
	Observe func(endpoint string, status int, err error, d time.Duration, remaining int, hasRemaining bool, cost int)

	mu     sync.Mutex
	token  string
	expiry time.Time
}

// Result is one /states/all response.
type Result struct {
	Time         int64
	Aircraft     []model.Aircraft
	Remaining    int // credits left, valid when HasRemaining
	HasRemaining bool
}

// RateLimitError is an HTTP 429 from OpenSky.
type RateLimitError struct{ RetryAfter time.Duration }

func (e *RateLimitError) Error() string {
	return fmt.Sprintf("rate limited by OpenSky, retry after %s", e.RetryAfter)
}

// States fetches state vectors for a box, or for the whole world when box is
// nil.
func (c *Client) States(ctx context.Context, box *scope.Box) (*Result, error) {
	u := c.APIURL + "/states/all"
	if box != nil {
		q := url.Values{}
		q.Set("lamin", ff(box.LaMin))
		q.Set("lomin", ff(box.LoMin))
		q.Set("lamax", ff(box.LaMax))
		q.Set("lomax", ff(box.LoMax))
		u += "?" + q.Encode()
	}
	cost := scope.MaxCost
	if box != nil {
		cost = scope.Cost(box.Area())
	}
	resp, err := c.get(ctx, u, false, cost)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		// The token expired early. Refresh once and retry.
		drain(resp)
		if resp, err = c.get(ctx, u, true, cost); err != nil {
			return nil, err
		}
	}
	defer drain(resp)

	res := &Result{}
	res.Remaining, res.HasRemaining = remaining(resp)
	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		secs, _ := strconv.Atoi(resp.Header.Get("X-Rate-Limit-Retry-After-Seconds"))
		if secs <= 0 {
			secs = 60
		}
		return nil, &RateLimitError{RetryAfter: time.Duration(secs) * time.Second}
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("OpenSky returned HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, fmt.Errorf("reading OpenSky response: %w", err)
	}
	if res.Time, res.Aircraft, err = model.ParseStates(body); err != nil {
		return nil, fmt.Errorf("parsing OpenSky response: %w", err)
	}
	return res, nil
}

// remaining reads the credit balance from a response.
func remaining(resp *http.Response) (int, bool) {
	if v := resp.Header.Get("X-Rate-Limit-Remaining"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n, true
		}
	}
	return 0, false
}

func (c *Client) get(ctx context.Context, u string, forceToken bool, cost int) (*http.Response, error) {
	tok, err := c.accessToken(ctx, forceToken)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	start := time.Now()
	resp, err := c.HTTP.Do(req)
	if err != nil {
		c.observe("states", 0, err, start, 0, false, cost)
		return nil, fmt.Errorf("OpenSky request failed: %w", err)
	}
	left, has := remaining(resp)
	c.observe("states", resp.StatusCode, nil, start, left, has, cost)
	return resp, nil
}

func (c *Client) accessToken(ctx context.Context, force bool) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !force && c.token != "" && time.Now().Before(c.expiry) {
		return c.token, nil
	}
	c.token = ""
	form := url.Values{"grant_type": {"client_credentials"}, "client_id": {c.ID}, "client_secret": {c.Secret}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	start := time.Now()
	resp, err := c.HTTP.Do(req)
	if err != nil {
		c.observe("token", 0, err, start, 0, false, 0)
		return "", fmt.Errorf("OpenSky token request failed: %w", err)
	}
	c.observe("token", resp.StatusCode, nil, start, 0, false, 0)
	defer drain(resp)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("OpenSky token endpoint returned HTTP %d; check the client id and secret", resp.StatusCode)
	}
	var t struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&t); err != nil || t.AccessToken == "" {
		return "", fmt.Errorf("OpenSky token response was not usable")
	}
	if t.ExpiresIn <= 0 {
		t.ExpiresIn = 1800
	}
	life := time.Duration(t.ExpiresIn) * time.Second
	margin := tokenMargin
	if margin > life/2 {
		margin = life / 2
	}
	c.token, c.expiry = t.AccessToken, time.Now().Add(life-margin)
	return c.token, nil
}

func (c *Client) observe(endpoint string, status int, err error, start time.Time, remaining int, has bool, cost int) {
	if c.Observe != nil {
		c.Observe(endpoint, status, err, time.Since(start), remaining, has, cost)
	}
}

func ff(f float64) string { return strconv.FormatFloat(f, 'f', 4, 64) }

func drain(resp *http.Response) {
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
}
