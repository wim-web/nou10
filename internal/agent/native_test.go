package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wim-web/nou10/internal/host"
	"github.com/wim-web/nou10/internal/protocol"
	"github.com/wim-web/nou10/internal/testutil"
)

// Exercises GitHub discovery, durable history, real file writes/hooks and result
// retry together. The HTTP endpoint is local and no CodeDeploy binary is used.
func TestNativeAgentIntegration(t *testing.T) {
	a, fx, _ := setup(t)
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a.Config.InstallRoot = filepath.Join(dir, "app")
	a.Config.ExecutionTimeout = 10 * time.Second
	if err := os.Mkdir(a.Config.InstallRoot, 0755); err != nil {
		t.Fatal(err)
	}
	a.Runner = host.Runner{Config: a.Config}
	trace := filepath.Join(dir, "trace")
	build := func(version string, fail bool) []byte {
		spec := fmt.Sprintf("version: 0.0\nos: linux\nfiles:\n  - source: app/\n    destination: %s\nhooks:\n  ApplicationStop:\n    - location: scripts/stop.sh\n      timeout: 5\n  ValidateService:\n    - location: scripts/check.sh\n      timeout: 5\n", a.Config.InstallRoot)
		check := fmt.Sprintf("#!/bin/sh\nset -eu\necho check-%s >> %s\ntest \"$(cat %s/version)\" = %s\n", version, trace, a.Config.InstallRoot, version)
		if fail {
			check += "exit 7\n"
		}
		return testutil.Archive(t, []testutil.File{{Name: "appspec.yml", Data: spec}, {Name: "manifest.json", Data: `{"source_sha":"` + testutil.SHA + `"}`},
			{Name: "app/version", Data: version, Mode: 0644}, {Name: "scripts/stop.sh", Data: fmt.Sprintf("#!/bin/sh\necho stop-%s >> %s\n", version, trace)}, {Name: "scripts/check.sh", Data: check}})
	}
	var ds []protocol.Deployment
	add := func(id int64, version string, fail bool) {
		body := build(version, fail)
		fx.mu.Lock()
		fx.bundle = body
		fx.mu.Unlock()
		ds = append(ds, testutil.Deployment(id, version, a.now(), body))
		fx.set(ds...)
	}
	add(1, "one", false)
	if err := a.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	add(2, "two", false)
	fx.fail("success")
	if err := a.Step(context.Background()); err == nil {
		t.Fatal("expected result reporting outage")
	}
	st, _ := a.Store.Read()
	if st.LastSuccess != "request:two" {
		t.Fatal("native result was not durable before reporting")
	}
	firstTrace, _ := os.ReadFile(trace)
	fx.fail("")
	if err := a.Store.Recover(a.now()); err != nil {
		t.Fatal(err)
	}
	if err := a.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	afterRetry, _ := os.ReadFile(trace)
	if string(firstTrace) != string(afterRetry) {
		t.Fatal("report retry replayed hooks")
	}
	add(3, "three", true)
	if err := a.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	st, _ = a.Store.Read()
	if st.LastSuccess != "request:two" || fx.last(3) != "failure" {
		t.Fatal("failed validation replaced successful history")
	}
	add(4, "four", false)
	if err := a.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	log, _ := os.ReadFile(trace)
	if got := string(log); !strings.HasSuffix(got, "stop-two\ncheck-four\n") || strings.Contains(got, "stop-three") {
		t.Fatalf("wrong previous stop revision: %s", got)
	}
	if b, err := os.ReadFile(filepath.Join(a.Config.InstallRoot, "version")); err != nil || string(b) != "four" {
		t.Fatalf("wrong installed file: %q %v", b, err)
	}
	duplicate := ds[len(ds)-1]
	duplicate.ID = 5
	ds = append(ds, duplicate)
	fx.set(ds...)
	if err := a.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	finalTrace, _ := os.ReadFile(trace)
	if string(finalTrace) != string(log) || fx.last(5) != "success" {
		t.Fatal("duplicate replayed native deployment")
	}
}
