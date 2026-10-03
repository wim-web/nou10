// Package github implements only the API operations used by nou10.
package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/wim-web/nou10/internal/protocol"
)

const APIURL = "https://api.github.com"

type Error struct {
	Kind       string
	Code       int
	RetryAfter time.Duration
}

func (e *Error) Error() string { return fmt.Sprintf("GitHub %s (HTTP %d)", e.Kind, e.Code) }
func Permanent(err error) bool { var e *Error; return errors.As(err, &e) && e.Kind == "permanent" }
func Delay(err error, fallback time.Duration) time.Duration {
	var e *Error
	if errors.As(err, &e) && e.RetryAfter > fallback {
		return e.RetryAfter
	}
	return fallback
}

type cached struct {
	ETag string
	Body []byte
	Next bool
}

// Client is used serially by each agent/CLI invocation.
type Client struct {
	base         string
	repo         string
	token        func() (string, error)
	http         *http.Client
	cache        map[string]cached
	nextWrite    time.Time
	PollInterval time.Duration
}

func New(base, repository string, token func() (string, error), client *http.Client) (*Client, error) {
	if !protocol.ValidRepository(repository) {
		return nil, errors.New("invalid repository")
	}
	u, err := url.Parse(base)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "https" && u.Scheme != "http") {
		return nil, errors.New("invalid API base URL")
	}
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Minute}
	}
	return &Client{base: strings.TrimRight(base, "/"), repo: "/repos/" + repository, token: token, http: client, cache: map[string]cached{}}, nil
}

func (c *Client) response(ctx context.Context, method, path string, body any, accept, etag string, download bool) (*http.Response, error) {
	var data io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		data = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+c.repo+path, data)
	if err != nil {
		return nil, errors.New("invalid API request")
	}
	token, err := c.token()
	if err != nil || token == "" {
		return nil, &Error{Kind: "authentication", RetryAfter: 15 * time.Minute}
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", accept)
	req.Header.Set("X-GitHub-Api-Version", "2026-03-10")
	req.Header.Set("User-Agent", "nou10/0.1")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	hc := *c.http
	hc.CheckRedirect = func(next *http.Request, via []*http.Request) error {
		if !download {
			return errors.New("API redirects are not supported")
		}
		if len(via) >= 10 {
			return errors.New("too many redirects")
		}
		if next.URL.Scheme != "https" || next.URL.User != nil {
			return errors.New("unsafe download redirect")
		}
		for _, prev := range append(via, next) {
			if !sameOrigin(prev.URL, req.URL) {
				next.Header.Del("Authorization")
				break
			}
		}
		return nil
	}
	resp, err := hc.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		// Do not log raw transport errors: they can contain signed download URLs.
		return nil, &Error{Kind: "transport"}
	}
	if n, _ := strconv.ParseInt(resp.Header.Get("X-Poll-Interval"), 10, 32); n > 0 {
		c.PollInterval = max(c.PollInterval, time.Duration(n)*time.Second)
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 || resp.StatusCode == http.StatusNotModified {
		return resp, nil
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 65536))
	e := &Error{Kind: "permanent", Code: resp.StatusCode}
	retry := retryAfter(resp.Header.Get("Retry-After"))
	rate := resp.StatusCode == 429 || resp.StatusCode == 403 && (resp.Header.Get("X-RateLimit-Remaining") == "0" || retry > 0 || bytes.Contains(bytes.ToLower(b), []byte("rate limit")))
	switch {
	case rate:
		e.Kind = "rate_limit"
		e.RetryAfter = max(retry, time.Minute)
		if resp.Header.Get("X-RateLimit-Remaining") == "0" {
			n, _ := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64)
			e.RetryAfter = max(e.RetryAfter, time.Until(time.Unix(n, 0))+time.Second)
		}
	case resp.StatusCode == 401 || resp.StatusCode == 403:
		e.Kind = "authentication"
		e.RetryAfter = 15 * time.Minute
	case resp.StatusCode >= 500 || resp.StatusCode == 408:
		e.Kind = "transient"
		e.RetryAfter = retry
	}
	return nil, e
}

func sameOrigin(a, b *url.URL) bool { return a.Scheme == b.Scheme && strings.EqualFold(a.Host, b.Host) }
func retryAfter(s string) time.Duration {
	if n, err := strconv.ParseInt(s, 10, 32); err == nil && n > 0 {
		return time.Duration(n) * time.Second
	}
	if t, err := http.ParseTime(s); err == nil {
		return max(0, time.Until(t))
	}
	return 0
}
func hasNext(s string) bool {
	for _, link := range strings.Split(s, ",") {
		if strings.Contains(link, `rel="next"`) {
			return true
		}
	}
	return false
}

func (c *Client) json(ctx context.Context, method, path string, body, out any, cacheable bool) (bool, error) {
	old := c.cache[path]
	etag := ""
	if cacheable {
		etag = old.ETag
	}
	resp, err := c.response(ctx, method, path, body, "application/vnd.github+json", etag, false)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	var b []byte
	next := hasNext(resp.Header.Get("Link"))
	if resp.StatusCode == http.StatusNotModified {
		if !cacheable || old.Body == nil {
			return false, errors.New("unexpected API 304")
		}
		b = old.Body
		next = old.Next
	} else {
		b, err = io.ReadAll(io.LimitReader(resp.Body, (16<<20)+1))
		if err != nil {
			return false, &Error{Kind: "transport"}
		}
		if len(b) > 16<<20 {
			return false, errors.New("API response too large")
		}
	}
	if err := json.Unmarshal(b, out); err != nil {
		return false, errors.New("invalid API response")
	}
	if cacheable && resp.StatusCode != http.StatusNotModified {
		c.cache[path] = cached{ETag: resp.Header.Get("ETag"), Body: b, Next: next}
	}
	return next, nil
}

// ListDeployments scans every page, including cached pages. No cursor is advanced
// on a partial scan; re-scanning also tolerates insertions/deletions between pages.
func (c *Client) ListDeployments(ctx context.Context, t protocol.Target) ([]protocol.Deployment, error) {
	var all []protocol.Deployment
	seen := map[int64]bool{}
	for page := 1; page <= 10000; page++ {
		q := url.Values{"environment": {t.Environment}, "task": {t.Task}, "per_page": {"100"}, "page": {strconv.Itoa(page)}}
		var ds []protocol.Deployment
		next, err := c.json(ctx, "GET", "/deployments?"+q.Encode(), nil, &ds, true)
		if err != nil {
			return nil, err
		}
		if ds == nil {
			return nil, errors.New("invalid deployment list: expected an array")
		}
		for _, d := range ds {
			if !seen[d.ID] {
				all = append(all, d)
				seen[d.ID] = true
			}
		}
		if !next {
			return all, nil
		}
	}
	return nil, errors.New("deployment pagination limit exceeded")
}

func (c *Client) CreateDeployment(ctx context.Context, d protocol.CreateDeployment) (protocol.Deployment, error) {
	var out protocol.Deployment
	_, err := c.json(ctx, "POST", "/deployments", d, &out, false)
	if err == nil && (out.ID <= 0 || out.SHA != d.Ref) {
		err = errors.New("API did not create the requested deployment")
	}
	return out, err
}
func (c *Client) Report(ctx context.Context, id int64, s protocol.Status) error {
	// GitHub recommends at least one second between mutating requests. This
	// also avoids bursting the durable outbox after an outage.
	if delay := time.Until(c.nextWrite); delay > 0 {
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	c.nextWrite = time.Now().Add(time.Second)
	var out protocol.Status
	_, err := c.json(ctx, "POST", fmt.Sprintf("/deployments/%d/statuses", id), s, &out, false)
	return err
}
func (c *Client) LatestStatus(ctx context.Context, id int64) (protocol.Status, error) {
	var statuses []protocol.Status
	_, err := c.json(ctx, "GET", fmt.Sprintf("/deployments/%d/statuses?per_page=1", id), nil, &statuses, true)
	if err != nil {
		return protocol.Status{}, err
	}
	if len(statuses) == 0 {
		return protocol.Status{State: "pending"}, nil
	}
	return statuses[0], nil
}

type Asset struct {
	ID    int64  `json:"id"`
	Name  string `json:"name"`
	State string `json:"state"`
	Size  int64  `json:"size"`
}

func (c *Client) FindAsset(ctx context.Context, releaseID, assetID int64) (Asset, error) {
	for page := 1; page <= 10000; page++ {
		var assets []Asset
		next, err := c.json(ctx, "GET", fmt.Sprintf("/releases/%d/assets?per_page=100&page=%d", releaseID, page), nil, &assets, false)
		if err != nil {
			return Asset{}, err
		}
		for _, a := range assets {
			if a.ID == assetID {
				return a, nil
			}
		}
		if !next {
			return Asset{}, &Error{Kind: "permanent", Code: 404}
		}
	}
	return Asset{}, errors.New("asset pagination limit exceeded")
}

func (c *Client) Download(ctx context.Context, assetID int64, w io.Writer, limit int64) (int64, error) {
	resp, err := c.response(ctx, "GET", fmt.Sprintf("/releases/assets/%d", assetID), nil, "application/octet-stream", "", true)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.ContentLength > limit {
		return 0, &Error{Kind: "permanent", Code: 413}
	}
	n, err := io.Copy(w, io.LimitReader(resp.Body, limit+1))
	if n > limit {
		return n, &Error{Kind: "permanent", Code: 413}
	}
	if err != nil {
		return n, &Error{Kind: "transport"}
	}
	return n, nil
}

// Probe is read-only. Status-write permission cannot be proven without a write.
func (c *Client) Probe(ctx context.Context, t protocol.Target) error {
	var repo any
	if _, err := c.json(ctx, "GET", "", nil, &repo, false); err != nil {
		return err
	}
	var contents any
	if _, err := c.json(ctx, "GET", "/contents/", nil, &contents, false); err != nil {
		return err
	}
	_, err := c.ListDeployments(ctx, t)
	return err
}
