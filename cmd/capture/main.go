// Command capture records raw OpenSky /states/all responses to a gzip JSONL
// file, one response per line, for use as test and replay data.
//
// It is a build-time tool. It stops early when the credit balance is low or
// the API rate limits it, so it cannot burn an allowance to zero.
package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	tokenURL  = "https://auth.opensky-network.org/auth/realms/opensky-network/protocol/openid-connect/token"
	statesURL = "https://opensky-network.org/api/states/all"
)

func main() {
	out := flag.String("out", "", "output file (.jsonl.gz)")
	count := flag.Int("count", 90, "number of snapshots")
	interval := flag.Duration("interval", 10*time.Second, "poll interval")
	reserve := flag.Int("reserve", 20, "stop when remaining credits fall below this")
	bbox := flag.String("bbox", "", "lamin,lomin,lamax,lomax; empty means global")
	flag.Parse()
	if *out == "" {
		log.Fatal("-out is required")
	}

	q := url.Values{}
	if *bbox != "" {
		p := strings.Split(*bbox, ",")
		if len(p) != 4 {
			log.Fatal("-bbox needs four values")
		}
		q.Set("lamin", p[0])
		q.Set("lomin", p[1])
		q.Set("lamax", p[2])
		q.Set("lomax", p[3])
	}

	f, err := os.Create(*out)
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()
	zw, _ := gzip.NewWriterLevel(f, gzip.BestCompression)
	defer zw.Close()

	client := &http.Client{Timeout: 60 * time.Second}
	token := ""
	if id, secret := os.Getenv("OPENSKY_CLIENT_ID"), os.Getenv("OPENSKY_CLIENT_SECRET"); id != "" && secret != "" {
		token, err = fetchToken(client, id, secret)
		if err != nil {
			log.Fatalf("token: %v", err)
		}
		log.Print("authenticated")
	} else {
		log.Print("anonymous (400 credits a day, 10 s resolution)")
	}

	var lastTime int64
	saved := 0
	tick := time.NewTicker(*interval)
	defer tick.Stop()
	for saved < *count {
		body, remaining, status, retry, err := fetch(client, q, token)
		switch {
		case err != nil:
			log.Printf("error: %v", err)
		case status == http.StatusTooManyRequests:
			log.Printf("rate limited, retry after %s s; stopping with %d snapshots", retry, saved)
			return
		case status != http.StatusOK:
			log.Printf("status %d", status)
		default:
			var head struct {
				Time   int64             `json:"time"`
				States []json.RawMessage `json:"states"`
			}
			if err := json.Unmarshal(body, &head); err != nil {
				log.Printf("bad body: %v", err)
				break
			}
			// OpenSky sometimes serves the same data again, or older data.
			if head.Time <= lastTime {
				log.Printf("data time %d is not after %d, skipped", head.Time, lastTime)
				break
			}
			lastTime = head.Time
			var compact bytes.Buffer
			if err := json.Compact(&compact, body); err != nil {
				log.Printf("compact: %v", err)
				break
			}
			compact.WriteByte('\n')
			if _, err := zw.Write(compact.Bytes()); err != nil {
				log.Fatal(err)
			}
			zw.Flush()
			saved++
			log.Printf("%d/%d time=%d aircraft=%d remaining=%s", saved, *count, head.Time, len(head.States), remaining)
			if n, err := strconv.Atoi(remaining); err == nil && n < *reserve {
				log.Printf("remaining credits %d below reserve %d; stopping", n, *reserve)
				return
			}
		}
		if saved < *count {
			<-tick.C
		}
	}
}

func fetch(c *http.Client, q url.Values, token string) (body []byte, remaining string, status int, retry string, err error) {
	u := statesURL
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, _ := http.NewRequest(http.MethodGet, u, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, "", 0, "", err
	}
	defer resp.Body.Close()
	body, err = io.ReadAll(resp.Body)
	return body, resp.Header.Get("X-Rate-Limit-Remaining"), resp.StatusCode,
		resp.Header.Get("X-Rate-Limit-Retry-After-Seconds"), err
}

func fetchToken(c *http.Client, id, secret string) (string, error) {
	form := url.Values{"grant_type": {"client_credentials"}, "client_id": {id}, "client_secret": {secret}}
	resp, err := c.PostForm(tokenURL, form)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("status %d", resp.StatusCode)
	}
	var t struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&t); err != nil {
		return "", err
	}
	return t.AccessToken, nil
}
