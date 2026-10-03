//go:build linux || darwin

package host

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Only the HTTPS transport is replaced: init, fetch, checkout and validation
// use real Git with a local repository, so these tests need no GitHub access.
type gitFixture struct {
	*fixture
	remote, binary, fetchArgs string
}

func newGitFixture(t *testing.T) *gitFixture {
	t.Helper()
	binary, err := exec.LookPath("git")
	if err != nil {
		t.Fatal("Git is required for Git source tests")
	}
	f := &gitFixture{fixture: newFixture(t), binary: binary}
	f.remote = filepath.Join(f.dir, "remote")
	if err := os.Mkdir(f.remote, 0755); err != nil {
		t.Fatal(err)
	}
	f.gitAt(f.remote, "init", "--template=", ".")
	f.config.GitScript = "deploy.sh"
	f.config.TokenFile = filepath.Join(f.dir, "token")
	f.write(f.config.TokenFile, "host-test-secret", 0600)
	f.write(filepath.Join(f.remote, ".gitignore"), ".env\n", 0644)
	f.fetchArgs = filepath.Join(f.dir, "fetch-args")
	bin := filepath.Join(f.dir, "bin")
	if err := os.Mkdir(bin, 0755); err != nil {
		t.Fatal(err)
	}
	// No real network call may escape this wrapper. Auth is supplied only to fetch.
	wrapper := fmt.Sprintf(`#!/bin/sh
set -eu
fetch=false
for arg do
  test "$arg" != fetch || fetch=true
  last=$arg
done
if "$fetch"; then
  test "${NOU10_GIT_AUTHORIZATION:-}" = %s
  printf '%%s\n' "$@" > %s
  exec %s -C %s fetch --no-tags --depth=1 -- %s "$last"
fi
test -z "${NOU10_GIT_AUTHORIZATION:-}"
exec %s "$@"
`, shellQuote("Authorization: Basic "+base64.StdEncoding.EncodeToString([]byte("x-access-token:host-test-secret"))), shellQuote(f.fetchArgs), shellQuote(binary), shellQuote(f.config.InstallRoot), shellQuote("file://"+f.remote), shellQuote(binary))
	f.write(filepath.Join(bin, "git"), wrapper, 0755)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GITHUB_TOKEN", "must-not-reach-script")
	return f
}
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
func (f *gitFixture) write(path, data string, mode os.FileMode) {
	f.t.Helper()
	if err := os.WriteFile(path, []byte(data), mode); err != nil {
		f.t.Fatal(err)
	}
}
func (f *gitFixture) gitAt(dir string, args ...string) string {
	f.t.Helper()
	cmd := exec.Command(f.binary, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com", "GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com")
	b, err := cmd.CombinedOutput()
	if err != nil {
		f.t.Fatalf("git %v: %v\n%s", args, err, b)
	}
	return strings.TrimSpace(string(b))
}
func (f *gitFixture) commit(script string) string {
	f.t.Helper()
	f.write(filepath.Join(f.remote, "deploy.sh"), "#!/bin/sh\nset -eu\n"+script+"\n", 0755)
	f.gitAt(f.remote, "add", ".")
	f.gitAt(f.remote, "commit", "-m", "test")
	return f.gitAt(f.remote, "rev-parse", "HEAD")
}
func (f *gitFixture) deploy(sha string) (Result, string) {
	f.t.Helper()
	job := f.t.TempDir()
	result := (Runner{Config: f.config}).Run(context.Background(), Request{Source: "git", JobDir: job, SourceSHA: sha, DeploymentID: 1})
	log, err := os.ReadFile(filepath.Join(job, "deploy.log"))
	if err != nil {
		f.t.Fatal(err)
	}
	return result, string(log)
}
func (f *gitFixture) assertSuccess(sha string) {
	f.t.Helper()
	if result, log := f.deploy(sha); result.State != "success" || result.Unknown {
		f.t.Fatalf("%+v\n%s", result, log)
	}
}

func TestGitDeployExactCommitAndPersistentEnv(t *testing.T) {
	f := newGitFixture(t)
	env := filepath.Join(f.dir, "runtime.env")
	f.write(env, "keep-this-env", 0600)
	if err := os.Symlink(env, filepath.Join(f.config.InstallRoot, ".env")); err != nil {
		t.Fatal(err)
	}
	first := f.commit(`test -z "${GITHUB_TOKEN:-}"
test -z "${NOU10_GIT_AUTHORIZATION:-}"
test "$(cat .env)" = keep-this-env
printf '%s' "$NOU10_SOURCE_SHA" > ../executed-sha`)
	second := f.commit(`printf second > ../executed-sha`)
	// HEAD of the remote has advanced; deploy must still execute the requested SHA.
	f.assertSuccess(first)
	actual, _ := os.ReadFile(filepath.Join(f.dir, "executed-sha"))
	if string(actual) != first || f.gitAt(f.config.InstallRoot, "rev-parse", "HEAD") != first {
		t.Fatal("deployed branch tip instead of requested SHA")
	}
	// Reuse a checkout with an unrelated origin; the host's repository selects source.
	f.gitAt(f.config.InstallRoot, "remote", "add", "origin", "https://example.invalid/wrong/repo")
	f.assertSuccess(second)
	actual, _ = os.ReadFile(filepath.Join(f.dir, "executed-sha"))
	if string(actual) != "second" {
		t.Fatalf("update did not run: %q", actual)
	}
	if got, _ := os.Readlink(filepath.Join(f.config.InstallRoot, ".env")); got != env {
		t.Fatal(".env link changed")
	}
	args, _ := os.ReadFile(f.fetchArgs)
	cfg, _ := os.ReadFile(filepath.Join(f.config.InstallRoot, ".git", "config"))
	if !strings.Contains(string(args), "https://github.com/owner/repo.git\n"+second) {
		t.Fatalf("wrong source: %s", args)
	}
	if strings.Contains(string(args)+string(cfg), "host-test-secret") || strings.Contains(string(cfg), "NOU10_GIT_AUTHORIZATION") {
		t.Fatal("persisted Git credential")
	}
	// Existing manual redeploy semantics also permit an older explicit commit.
	f.assertSuccess(first)
}

func TestGitRejectsUnsafeCheckoutBeforeScript(t *testing.T) {
	for _, scenario := range []string{"dirty", "ignored conflict", "untracked conflict", "fetch failure", "symlink script", "untracked script", "nonexecutable script", "linked git", "rewritten URL"} {
		t.Run(scenario, func(t *testing.T) {
			f := newGitFixture(t)
			sha := f.commit("touch ../executed")
			switch scenario {
			case "dirty", "ignored conflict", "untracked conflict", "untracked script", "rewritten URL":
				f.assertSuccess(sha)
				if err := os.Remove(filepath.Join(f.dir, "executed")); err != nil {
					t.Fatal(err)
				}
			}
			switch scenario {
			case "dirty":
				f.write(filepath.Join(f.config.InstallRoot, "deploy.sh"), "#!/bin/sh\necho local-change\n", 0755)
			case "ignored conflict":
				f.write(filepath.Join(f.config.InstallRoot, ".env"), "keep", 0600)
				f.write(filepath.Join(f.remote, ".env"), "overwrite", 0600)
				f.gitAt(f.remote, "add", "-f", ".env")
				sha = f.commit("touch ../executed\n# env conflict")
			case "untracked conflict":
				f.write(filepath.Join(f.config.InstallRoot, "local"), "keep", 0600)
				f.write(filepath.Join(f.remote, "local"), "overwrite", 0600)
				sha = f.commit("touch ../executed\n# conflict")
			case "fetch failure":
				sha = strings.Repeat("1", 40)
			case "symlink script":
				if err := os.Remove(filepath.Join(f.remote, "deploy.sh")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("/bin/true", filepath.Join(f.remote, "deploy.sh")); err != nil {
					t.Fatal(err)
				}
				f.gitAt(f.remote, "add", ".")
				f.gitAt(f.remote, "commit", "-m", "symlink")
				sha = f.gitAt(f.remote, "rev-parse", "HEAD")
			case "untracked script":
				f.config.GitScript = "local.sh"
				f.write(filepath.Join(f.config.InstallRoot, "local.sh"), "#!/bin/sh\ntouch ../executed\n", 0755)
			case "nonexecutable script":
				f.gitAt(f.remote, "update-index", "--chmod=-x", "deploy.sh")
				f.gitAt(f.remote, "commit", "-m", "nonexecutable")
				sha = f.gitAt(f.remote, "rev-parse", "HEAD")
			case "linked git":
				if err := os.Symlink(filepath.Join(f.remote, ".git"), filepath.Join(f.config.InstallRoot, ".git")); err != nil {
					t.Fatal(err)
				}
			case "rewritten URL":
				f.gitAt(f.config.InstallRoot, "config", "url.https://example.invalid/.insteadOf", "https://github.com/")
			}
			result, log := f.deploy(sha)
			if result.State != "error" || result.Unknown {
				t.Fatalf("%+v\n%s", result, log)
			}
			if _, err := os.Stat(filepath.Join(f.dir, "executed")); !os.IsNotExist(err) {
				t.Fatal("ran deployment script after preparation failed")
			}
			if scenario == "ignored conflict" || scenario == "untracked conflict" {
				name := ".env"
				if scenario == "untracked conflict" {
					name = "local"
				}
				b, _ := os.ReadFile(filepath.Join(f.config.InstallRoot, name))
				if string(b) != "keep" {
					t.Fatal("overwrote host file")
				}
			}
		})
	}
}

func TestGitScriptFailureAndInterruption(t *testing.T) {
	for _, tc := range []struct {
		name, script, state string
		unknown             bool
	}{
		{"failure", "exit 7", "failure", false},
		{"signal", "kill -KILL $$", "error", true},
		{"timeout", "sleep 30", "error", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newGitFixture(t)
			sha := f.commit(tc.script)
			if tc.name == "timeout" {
				f.config.ExecutionTimeout = time.Second
			}
			result, log := f.deploy(sha)
			if result.State != tc.state || result.Unknown != tc.unknown {
				t.Fatalf("%+v\n%s", result, log)
			}
		})
	}
}
