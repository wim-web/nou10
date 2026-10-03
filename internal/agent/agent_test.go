package agent

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wim-web/nou10/internal/config"
	"github.com/wim-web/nou10/internal/github"
	"github.com/wim-web/nou10/internal/host"
	"github.com/wim-web/nou10/internal/ledger"
	"github.com/wim-web/nou10/internal/protocol"
	"github.com/wim-web/nou10/internal/testutil"
)

type brokenDisk struct{}

func (brokenDisk) Write([]byte) (int, error) { return 0, errors.New("disk full") }
func TestDownloadPreservesDiskErrors(t *testing.T) {
	a, _, _ := setup(t)
	sink := &writerPair{file: brokenDisk{}, hash: sha256.New()}
	_, _ = a.GitHub.Download(context.Background(), 20, sink, 1<<20)
	var disk *storageError
	if !errors.As(sink.err, &disk) {
		t.Fatalf("lost local disk failure: %v", sink.err)
	}
}

type fakeRunner struct {
	runs   int
	result host.Result
	check  func()
}

func (r *fakeRunner) Run(context.Context, host.Request) host.Result {
	r.runs++
	if r.check != nil {
		r.check()
	}
	return r.result
}

type fixture struct {
	mu        sync.Mutex
	ds        []protocol.Deployment
	bundle    []byte
	reports   map[int64][]string
	failState string
	failCode  int
	assetID   int64
	listFail  bool
	downloads int
}

func (f *fixture) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case strings.HasSuffix(r.URL.Path, "/statuses"):
		var id int64
		fmt.Sscanf(r.URL.Path, "/repos/owner/repo/deployments/%d/statuses", &id)
		var s protocol.Status
		_ = json.NewDecoder(r.Body).Decode(&s)
		if s.State == f.failState {
			w.WriteHeader(f.failCode)
			return
		}
		f.reports[id] = append(f.reports[id], s.State)
		_ = json.NewEncoder(w).Encode(s)
	case strings.HasSuffix(r.URL.Path, "/deployments"):
		if f.listFail {
			w.WriteHeader(503)
			return
		}
		_ = json.NewEncoder(w).Encode(f.ds)
	case strings.HasSuffix(r.URL.Path, "/releases/10/assets"):
		_ = json.NewEncoder(w).Encode([]github.Asset{{ID: f.assetID, Name: "bundle.tgz", Size: int64(len(f.bundle)), State: "uploaded"}})
	case strings.HasSuffix(r.URL.Path, "/releases/assets/20"):
		f.downloads++
		_, _ = w.Write(f.bundle)
	default:
		http.NotFound(w, r)
	}
}
func setup(t *testing.T) (*Agent, *fixture, *fakeRunner) {
	t.Helper()
	fx := &fixture{ds: []protocol.Deployment{}, bundle: testutil.Bundle(t), reports: map[int64][]string{}, assetID: 20, failCode: 503}
	server := httptest.NewServer(http.HandlerFunc(fx.serve))
	t.Cleanup(server.Close)
	c, _ := github.New(server.URL, "owner/repo", func() (string, error) { return "secret", nil }, server.Client())
	target := testutil.Target()
	dir := filepath.Join(t.TempDir(), "state")
	s, err := ledger.Open(dir, target)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	r := &fakeRunner{result: host.Result{State: "success", Summary: "done"}}
	a := &Agent{Config: config.Config{Repository: target.Repository, Application: target.Application, Environment: target.Environment, Task: target.Task, StateDir: dir, MaxDownloadBytes: 1 << 20, MaxExpandedBytes: 1 << 20, MaxFiles: 100}, GitHub: c, Store: s, Runner: r, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	now := time.Now().UTC().Truncate(time.Second)
	a.Now = func() time.Time { return now }
	if err := a.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	return a, fx, r
}
func (f *fixture) set(ds ...protocol.Deployment) { f.mu.Lock(); defer f.mu.Unlock(); f.ds = ds }
func (f *fixture) fail(state string)             { f.mu.Lock(); defer f.mu.Unlock(); f.failState = state }
func (f *fixture) last(id int64) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.reports[id]
	if len(r) == 0 {
		return ""
	}
	return r[len(r)-1]
}

func TestExecutionOrderingDuplicateAndReportingRetry(t *testing.T) {
	a, fx, runner := setup(t)
	d := testutil.Deployment(1, "request", a.now(), fx.bundle)
	dup := d
	dup.ID = 2
	fx.set(dup, d)
	fx.fail("success")
	runner.check = func() {
		st, err := a.Store.Read()
		if err != nil {
			t.Fatal(err)
		}
		if st.Entries["request:request"].Phase != "running" {
			t.Error("start was not durable before execution")
		}
		if fx.last(1) != "in_progress" || fx.last(2) != "in_progress" {
			t.Error("executed before reporting in_progress to both IDs")
		}
	}
	if err := a.Step(context.Background()); err == nil {
		t.Fatal("expected report failure")
	}
	st, _ := a.Store.Read()
	if runner.runs != 1 || st.Entries["request:request"].Status != "success" || len(st.Outbox) == 0 {
		t.Fatal("final result was not persisted")
	}
	// Simulate normal restart after only reporting failed.
	if err := a.Store.Recover(a.now()); err != nil {
		t.Fatal(err)
	}
	fx.fail("")
	if err := a.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if runner.runs != 1 || fx.last(1) != "success" || fx.last(2) != "success" {
		t.Fatal("did not retry just the reports")
	}
	st, _ = a.Store.Read()
	if st.Held || len(st.Outbox) != 0 {
		t.Fatal("report did not settle")
	}
}

func TestNoExecutionUntilStartReportedAndDeadlineRechecked(t *testing.T) {
	for _, expire := range []bool{false, true} {
		t.Run(fmt.Sprint(expire), func(t *testing.T) {
			a, fx, r := setup(t)
			fx.set(testutil.Deployment(1, "r", a.now(), fx.bundle))
			fx.fail("in_progress")
			if err := a.Step(context.Background()); err == nil || r.runs != 0 {
				t.Fatal("ran despite start report failure")
			}
			fx.fail("")
			if expire {
				now := a.now().Add(time.Hour)
				a.Now = func() time.Time { return now }
			}
			if err := a.Step(context.Background()); err != nil {
				t.Fatal(err)
			}
			if expire {
				if r.runs != 0 || fx.last(1) != "error" {
					t.Fatal("expired request ran")
				}
			} else if r.runs != 1 || fx.last(1) != "success" {
				t.Fatal("request did not resume")
			}
		})
	}
}

func TestCrashAndUnknownStopTarget(t *testing.T) {
	a, fx, r := setup(t)
	d := testutil.Deployment(1, "one", a.now(), fx.bundle)
	fx.set(d)
	fx.fail("in_progress")
	if err := a.Step(context.Background()); err == nil {
		t.Fatal("expected failure")
	}
	if err := a.Store.Recover(a.now()); err != nil {
		t.Fatal(err)
	}
	fx.fail("")
	fx.set(d, testutil.Deployment(2, "two", a.now(), fx.bundle))
	if err := a.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	st, _ := a.Store.Read()
	if !st.Held || r.runs != 0 || fx.last(1) != "error" {
		t.Fatal("crash was replayed or next request executed")
	}
}

func TestRejectInvalidRequestsBeforeRunning(t *testing.T) {
	for _, tc := range []string{"schema", "expired", "hash", "release", "wrong environment", "wrong application", "wrong task", "mutable ref"} {
		t.Run(tc, func(t *testing.T) {
			a, fx, r := setup(t)
			d := testutil.Deployment(1, "r", a.now(), fx.bundle)
			p, _ := protocol.Parse(d)
			switch tc {
			case "schema":
				p.SchemaVersion = 2
			case "expired":
				p.StartBefore = a.now()
			case "hash":
				p.SHA256 = strings.Repeat("0", 64)
			case "release":
				fx.assetID = 99
			case "wrong environment":
				d.Environment = "staging"
			case "wrong application":
				p.Application = "other"
			case "wrong task":
				d.Task = "deploy"
			case "mutable ref":
				d.Ref = "main"
			}
			d.Payload, _ = json.Marshal(p)
			fx.set(d)
			if err := a.Step(context.Background()); err != nil {
				t.Fatal(err)
			}
			if r.runs != 0 {
				t.Fatal("invalid request executed")
			}
			if !strings.HasPrefix(tc, "wrong ") && fx.last(1) != "error" {
				t.Fatalf("rejection not reported: %s", fx.last(1))
			}
		})
	}
}

func TestOldestFirstAndManualRedeploy(t *testing.T) {
	a, fx, r := setup(t)
	one := testutil.Deployment(1, "one", a.now().Add(-time.Minute), fx.bundle)
	two := testutil.Deployment(2, "two", a.now(), fx.bundle)
	fx.set(two, one)
	if err := a.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fx.last(1) != "success" || fx.last(2) != "queued" {
		t.Fatal("requests were not ordered")
	}
	if err := a.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r.runs != 2 || fx.last(2) != "success" {
		t.Fatal("new request with old asset was not executed")
	}
}
