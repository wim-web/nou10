package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wim-web/nou10/internal/protocol"
	"github.com/wim-web/nou10/internal/testutil"
)

func TestPaginationConditionalRequestsAndPartialFailure(t *testing.T) {
	var mu sync.Mutex
	fail := true
	cachedRequests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer secret" {
			t.Error("missing API auth")
		}
		if r.URL.Query().Get("task") != protocol.DefaultTask || r.URL.Query().Get("environment") != "production" {
			t.Error("missing filters")
		}
		if r.URL.Query().Get("page") == "1" {
			w.Header().Set("Link", `<https://api.github.com/ignored>; rel="next"`)
			w.Header().Set("ETag", `"one"`)
			w.Header().Set("X-Poll-Interval", "45")
			if r.Header.Get("If-None-Match") == `"one"` {
				cachedRequests++
				w.WriteHeader(304)
				return
			}
			fmt.Fprint(w, `[{"id":2}]`)
			return
		}
		if fail {
			w.WriteHeader(503)
			return
		}
		fmt.Fprint(w, `[{"id":1}]`)
	}))
	defer server.Close()
	c, _ := New(server.URL, "owner/repo", func() (string, error) { return "secret", nil }, server.Client())
	if ds, err := c.ListDeployments(context.Background(), testutil.Target()); err == nil || ds != nil {
		t.Fatal("accepted partial scan")
	}
	mu.Lock()
	fail = false
	mu.Unlock()
	ds, err := c.ListDeployments(context.Background(), testutil.Target())
	if err != nil {
		t.Fatal(err)
	}
	if len(ds) != 2 || ds[0].ID != 2 || ds[1].ID != 1 {
		t.Fatalf("missing pages: %+v", ds)
	}
	mu.Lock()
	n := cachedRequests
	mu.Unlock()
	if n != 1 || c.PollInterval != 45*time.Second {
		t.Fatal("conditional request/poll hint not honored")
	}
}

func TestErrors(t *testing.T) {
	for _, tc := range []struct {
		code       int
		header     http.Header
		body, kind string
		delay      time.Duration
	}{
		{401, nil, "sensitive body", "authentication", 15 * time.Minute},
		{403, nil, "forbidden", "authentication", 15 * time.Minute},
		{403, http.Header{"X-Ratelimit-Remaining": {"0"}}, "", "rate_limit", time.Minute},
		{403, nil, `{"message":"secondary rate limit"}`, "rate_limit", time.Minute},
		{429, http.Header{"Retry-After": {"120"}}, "", "rate_limit", 2 * time.Minute},
		{503, http.Header{"Retry-After": {"90"}}, "", "transient", 90 * time.Second},
		{404, nil, "private info", "permanent", 0},
	} {
		t.Run(fmt.Sprint(tc.code, tc.kind, tc.delay), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				for k, v := range tc.header {
					w.Header()[k] = v
				}
				w.WriteHeader(tc.code)
				fmt.Fprint(w, tc.body)
			}))
			defer server.Close()
			c, _ := New(server.URL, "owner/repo", func() (string, error) { return "secret", nil }, server.Client())
			_, err := c.LatestStatus(context.Background(), 1)
			var api *Error
			if !errors.As(err, &api) || api.Kind != tc.kind || api.RetryAfter < tc.delay {
				t.Fatalf("unexpected %v", err)
			}
			if strings.Contains(err.Error(), "sensitive") || strings.Contains(err.Error(), "private") {
				t.Fatal("leaked response body")
			}
		})
	}
}

func TestCrossOriginDownloadStripsAuthorizationAndEnforcesSize(t *testing.T) {
	download := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("PAT leaked across origins")
		}
		fmt.Fprint(w, "bundle")
	}))
	defer download.Close()
	api := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" {
			t.Error("API token missing")
		}
		http.Redirect(w, r, download.URL+"/signed?credential=private", 302)
	}))
	defer api.Close()
	c, _ := New(api.URL, "owner/repo", func() (string, error) { return "secret", nil }, download.Client())
	var out bytes.Buffer
	if _, err := c.Download(context.Background(), 1, &out, 100); err != nil {
		t.Fatal(err)
	}
	if out.String() != "bundle" {
		t.Fatal("missing download")
	}
	if _, err := c.Download(context.Background(), 1, &out, 3); !Permanent(err) {
		t.Fatalf("missing size limit: %v", err)
	}
}

func TestCreateContractAndNoAutomaticInactive(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/statuses") {
			if body["auto_inactive"] != false {
				t.Error("auto_inactive must be false")
			}
			fmt.Fprint(w, `{"state":"success"}`)
			return
		}
		if body["auto_merge"] != false {
			t.Error("auto_merge must be false")
		}
		if contexts, ok := body["required_contexts"].([]any); !ok || len(contexts) != 0 {
			t.Error("required_contexts must be []")
		}
		w.WriteHeader(201)
		fmt.Fprintf(w, `{"id":1,"sha":%q}`, testutil.SHA)
	}))
	defer server.Close()
	c, _ := New(server.URL, "owner/repo", func() (string, error) { return "secret", nil }, server.Client())
	if _, err := c.CreateDeployment(context.Background(), protocol.CreateDeployment{Ref: testutil.SHA, RequiredContexts: []string{}}); err != nil {
		t.Fatal(err)
	}
	if err := c.Report(context.Background(), 1, protocol.Status{State: "success"}); err != nil {
		t.Fatal(err)
	}
}
