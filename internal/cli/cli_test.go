package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wim-web/nou10/internal/github"
	"github.com/wim-web/nou10/internal/ledger"
	"github.com/wim-web/nou10/internal/protocol"
	"github.com/wim-web/nou10/internal/testutil"
)

type roundTripper func(*http.Request) (*http.Response, error)

func (f roundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestDeployCLIWithExistingAssetAndWait(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "workflow-secret")
	t.Setenv("GITHUB_RUN_ID", "1234")
	t.Setenv("GITHUB_RUN_ATTEMPT", "2")
	for _, source := range []string{"bundle", "git"} {
		for _, final := range []string{"success", "failure"} {
			t.Run(source+"-"+final, func(t *testing.T) {
				var created protocol.CreateDeployment
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Header.Get("Authorization") != "Bearer workflow-secret" {
						t.Error("missing workflow token")
					}
					switch {
					case strings.HasSuffix(r.URL.Path, "/releases/10/assets"):
						if source == "git" {
							t.Error("Git deploy queried release assets")
						}
						fmt.Fprint(w, `[{"id":20,"name":"bundle.tgz","state":"uploaded","size":100}]`)
					case strings.HasSuffix(r.URL.Path, "/deployments"):
						if err := json.NewDecoder(r.Body).Decode(&created); err != nil {
							t.Error(err)
							w.WriteHeader(400)
							return
						}
						w.WriteHeader(201)
						fmt.Fprintf(w, `{"id":30,"sha":%q}`, testutil.SHA)
					case strings.HasSuffix(r.URL.Path, "/30/statuses"):
						fmt.Fprintf(w, `[{"state":%q}]`, final)
					default:
						http.NotFound(w, r)
					}
				}))
				defer server.Close()
				base, _ := url.Parse(server.URL)
				oldTransport := http.DefaultTransport
				http.DefaultTransport = roundTripper(func(r *http.Request) (*http.Response, error) {
					clone := r.Clone(r.Context())
					clone.URL.Scheme = base.Scheme
					clone.URL.Host = base.Host
					return oldTransport.RoundTrip(clone)
				})
				defer func() { http.DefaultTransport = oldTransport }()
				var out bytes.Buffer
				args := []string{"deploy", "--repository", "owner/repo", "--application", "app", "--environment", "production", "--sha", testutil.SHA,
					"--source", source, "--wait"}
				if source == "bundle" {
					args = append(args, "--release-id", "10", "--asset-id", "20", "--sha256", strings.Repeat("a", 64))
				}
				err := Run(context.Background(), args, &out, io.Discard, "test")
				if (err == nil) != (final == "success") {
					t.Fatalf("terminal %s: %v", final, err)
				}
				if err := created.Payload.Validate(); err != nil {
					t.Fatal(err)
				}
				if source == "git" && (created.Payload.Source != "git" || created.Payload.SchemaVersion != 2) {
					t.Fatal("not a Git request")
				}
				if created.AutoMerge || created.RequiredContexts == nil || len(created.RequiredContexts) != 0 || created.Ref != testutil.SHA || created.Task != protocol.DefaultTask || created.Payload.RequestID != "1234:2:"+testutil.Target().Key() {
					t.Fatalf("wrong request: %+v", created)
				}
				if !strings.Contains(out.String(), `"deployment_id":30`) || strings.Contains(out.String(), "workflow-secret") {
					t.Fatalf("bad output: %s", out.String())
				}
			})
		}
	}

}

func TestWaitOnlySucceedsOnSuccess(t *testing.T) {
	for _, state := range []string{"success", "failure", "error", "inactive", "pending"} {
		t.Run(state, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprintf(w, `[{"state":%q}]`, state) }))
			defer server.Close()
			c, _ := github.New(server.URL, "owner/repo", func() (string, error) { return "secret", nil }, server.Client())
			err := Wait(context.Background(), c, 1, 100*time.Millisecond, 10*time.Millisecond, io.Discard)
			if (err == nil) != (state == "success") {
				t.Fatalf("state %s: %v", state, err)
			}
		})
	}
}

func TestHelpAndInvalidCommands(t *testing.T) {
	var out bytes.Buffer
	if err := Run(context.Background(), nil, &out, io.Discard, "test"); err != nil || !strings.Contains(out.String(), "recover") {
		t.Fatal("missing help")
	}
	for _, args := range [][]string{{"unknown"}, {"deploy", "--sha", "main"}, {"status", "--deployment-id", "0"}} {
		if err := Run(context.Background(), args, io.Discard, io.Discard, "test"); err == nil {
			t.Fatalf("accepted invalid args %v", args)
		}
	}
}

func TestOperatorRecoveryPreservesUnknownAndDiscardsPending(t *testing.T) {
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	lockDir := filepath.Join(dir, "locks")
	path := filepath.Join(dir, "config.yml")
	configText := fmt.Sprintf("repository: owner/repo\napplication: app\nenvironment: production\ntoken_file: /unused\nstate_dir: %s\nlock_dir: %s\ninstall_root: /srv/app\n", stateDir, lockDir)
	if err := os.WriteFile(path, []byte(configText), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := ledger.Open(stateDir, testutil.Target())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := s.Ingest(nil, now); err != nil {
		t.Fatal(err)
	}
	if err := s.Ingest([]protocol.Deployment{testutil.Deployment(1, "one", now, testutil.Bundle(t)), testutil.Deployment(2, "two", now, testutil.Bundle(t))}, now); err != nil {
		t.Fatal(err)
	}
	if err := s.Update(func(st *ledger.State) error {
		return st.Transition("request:one", "running", "in_progress", "running", now)
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := Run(context.Background(), []string{"recover", "--config", path}, io.Discard, io.Discard, "test"); err == nil {
		t.Fatal("unconfirmed recovery accepted")
	}
	if err := Run(context.Background(), []string{"recover", "--config", path, "--confirm-stopped"}, io.Discard, io.Discard, "test"); err != nil {
		t.Fatal(err)
	}
	s, err = ledger.Open(stateDir, testutil.Target())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	st, err := s.Read()
	if err != nil {
		t.Fatal(err)
	}
	if st.Held || st.Initialized || st.Entries["request:one"].Phase != "unknown" || len(st.Pending()) != 0 {
		t.Fatalf("unsafe recovery: %+v", st)
	}
}
