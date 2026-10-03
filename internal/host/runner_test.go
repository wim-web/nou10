//go:build linux || darwin

package host

import (
	"context"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wim-web/nou10/internal/config"
	"github.com/wim-web/nou10/internal/testutil"
)

type fixture struct {
	t             *testing.T
	dir, previous string
	config        config.Config
	nextID        int64
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(dir, "app")
	if err := os.Mkdir(root, 0755); err != nil {
		t.Fatal(err)
	}
	return &fixture{t: t, dir: dir, config: config.Config{Application: "myapp", Environment: "production", Repository: "owner/repo", InstallRoot: root,
		StateDir: filepath.Join(dir, "state"), ExecutionTimeout: 10 * time.Second, MaxExpandedBytes: 1 << 20, MaxFiles: 100}}
}
func (f *fixture) files(version string) []testutil.File {
	var spec strings.Builder
	fmt.Fprintf(&spec, "version: 0.0\nos: linux\nfiles:\n  - source: app/\n    destination: %s\nhooks:\n", f.config.InstallRoot)
	files := []testutil.File{{Name: "manifest.json", Data: `{"source_sha":"` + testutil.SHA + `"}`}, {Name: "app/version", Data: version, Mode: 0644}}
	for _, event := range []string{"ApplicationStop", "BeforeInstall", "AfterInstall", "ApplicationStart", "ValidateService"} {
		name := "scripts/" + event + ".sh"
		fmt.Fprintf(&spec, "  %s:\n    - location: %s\n      timeout: 5\n", event, name)
		script := fmt.Sprintf("#!/bin/sh\nset -eu\nprintf '%%s:%%s\\n' \"$LIFECYCLE_EVENT\" %s >> %s\n", version, filepath.Join(f.dir, "events.log"))
		if event == "ValidateService" {
			script += fmt.Sprintf("test \"$(cat %s/version)\" = %s\n", f.config.InstallRoot, version)
		}
		files = append(files, testutil.File{Name: name, Data: script})
	}
	return append(files, testutil.File{Name: "appspec.yml", Data: spec.String()})
}
func (f *fixture) run(files []testutil.File) Result {
	f.t.Helper()
	f.nextID++
	job, err := os.MkdirTemp(f.dir, "job-")
	if err != nil {
		f.t.Fatal(err)
	}
	name := filepath.Join(job, "bundle.tgz")
	if err := os.WriteFile(name, testutil.Archive(f.t, files), 0600); err != nil {
		f.t.Fatal(err)
	}
	result := (Runner{Config: f.config}).Run(context.Background(), Request{BundlePath: name, JobDir: job, SourceSHA: testutil.SHA, DeploymentID: f.nextID, PreviousJobDir: f.previous})
	if result.State == "success" {
		f.previous = job
	}
	if result.State != "success" {
		log, _ := os.ReadFile(filepath.Join(job, "deploy.log"))
		f.t.Logf("%+v\n%s", result, log)
	}
	return result
}
func change(files []testutil.File, name string, fn func(string) string) []testutil.File {
	for i := range files {
		if files[i].Name == name {
			files[i].Data = fn(files[i].Data)
		}
	}
	return files
}
func (f *fixture) events() string {
	f.t.Helper()
	b, _ := os.ReadFile(filepath.Join(f.dir, "events.log"))
	return string(b)
}

func TestNativeDeploymentLifecycle(t *testing.T) {
	f := newFixture(t)
	files := append(f.files("one"), testutil.File{Name: "app/obsolete", Data: "old"})
	if result := f.run(files); result.State != "success" {
		t.Fatal(result)
	}
	if got := f.events(); got != "BeforeInstall:one\nAfterInstall:one\nApplicationStart:one\nValidateService:one\n" {
		t.Fatalf("first deployment order: %s", got)
	}
	if err := os.WriteFile(filepath.Join(f.config.InstallRoot, "user-data"), []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if result := f.run(f.files("two")); result.State != "success" {
		t.Fatal(result)
	}
	if !strings.HasSuffix(f.events(), "ApplicationStop:one\nBeforeInstall:two\nAfterInstall:two\nApplicationStart:two\nValidateService:two\n") {
		t.Fatalf("wrong revision/order: %s", f.events())
	}
	if _, err := os.Stat(filepath.Join(f.config.InstallRoot, "obsolete")); !os.IsNotExist(err) {
		t.Fatal("stale managed file survived")
	}
	if b, err := os.ReadFile(filepath.Join(f.config.InstallRoot, "user-data")); err != nil || string(b) != "keep" {
		t.Fatal("unmanaged file was removed")
	}
	// A manual redeploy can install an older artifact under a new deployment ID.
	if result := f.run(f.files("one")); result.State != "success" {
		t.Fatal(result)
	}
}

func TestHookFailuresAndUnknownOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name, event, script, state string
		unknown                    bool
	}{
		{"stop", "ApplicationStop", "exit 7", "failure", false},
		{"before", "BeforeInstall", "exit 7", "failure", false},
		{"after", "AfterInstall", "exit 7", "failure", false},
		{"start", "ApplicationStart", "exit 7", "failure", false},
		{"health", "ValidateService", "exit 8", "failure", false},
		{"exit two is a hook failure", "ValidateService", "exit 2", "failure", false},
		{"timeout", "ValidateService", "sleep 30", "error", true},
		{"signal", "ValidateService", "kill -KILL $$", "error", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			files := f.files("one")
			if tc.event == "ApplicationStop" {
				files = change(files, "scripts/ApplicationStop.sh", func(s string) string { return s + tc.script + "\n" })
				if r := f.run(files); r.State != "success" {
					t.Fatal(r)
				}
				files = f.files("two")
			} else {
				files = change(files, "scripts/"+tc.event+".sh", func(s string) string { return s + tc.script + "\n" })
			}
			if tc.name == "timeout" {
				files = change(files, "appspec.yml", func(s string) string { return strings.ReplaceAll(s, "timeout: 5", "timeout: 1") })
			}
			previous := f.previous
			result := f.run(files)
			if result.State != tc.state || result.Unknown != tc.unknown {
				t.Fatal(result)
			}
			if f.previous != previous {
				t.Fatal("failure advanced last successful revision")
			}
		})
	}
}

func TestPreflightRejectsUnsafeOrUnsupportedSpecs(t *testing.T) {
	for _, name := range []string{"unknown field", "unsupported permission", "unknown hook", "outside root", "source traversal", "runas", "missing source", "symlink destination"} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			files := f.files("one")
			files = change(files, "appspec.yml", func(s string) string {
				switch name {
				case "unknown field":
					return s + "typo: true\n"
				case "unsupported permission":
					return s + "permissions:\n  - object: " + f.config.InstallRoot + "\n    acls: [x]\n"
				case "unknown hook":
					return strings.ReplaceAll(s, "BeforeInstall:", "Install:")
				case "outside root":
					return strings.ReplaceAll(s, f.config.InstallRoot, f.dir)
				case "source traversal":
					return strings.ReplaceAll(s, "source: app/", "source: ../outside")
				case "runas":
					return strings.ReplaceAll(s, "timeout: 5", "timeout: 5\n      runas: nou10-nonexistent-user")
				case "missing source":
					return strings.ReplaceAll(s, "source: app/", "source: absent/")
				}
				return s
			})
			if name == "symlink destination" {
				outside := filepath.Join(f.dir, "outside")
				if err := os.WriteFile(outside, []byte("keep"), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, filepath.Join(f.config.InstallRoot, "version")); err != nil {
					t.Fatal(err)
				}
			}
			if r := f.run(files); r.State != "error" || r.Unknown {
				t.Fatal(r)
			}
			if f.events() != "" {
				t.Fatal("hook ran before preflight rejection")
			}
		})
	}
}

func TestExistingFilePolicies(t *testing.T) {
	for _, policy := range []string{"DISALLOW", "OVERWRITE", "RETAIN"} {
		t.Run(policy, func(t *testing.T) {
			f := newFixture(t)
			name := filepath.Join(f.config.InstallRoot, "local-config")
			if err := os.WriteFile(name, []byte("existing"), 0600); err != nil {
				t.Fatal(err)
			}
			files := append(f.files("one"), testutil.File{Name: "app/local-config", Data: "new"})
			files = change(files, "appspec.yml", func(s string) string { return s + "file_exists_behavior: " + policy + "\n" })
			r := f.run(files)
			if policy == "DISALLOW" {
				if r.State != "error" || f.events() != "" {
					t.Fatal(r)
				}
				return
			}
			if r.State != "success" {
				t.Fatal(r)
			}
			b, _ := os.ReadFile(name)
			want := "new"
			if policy == "RETAIN" {
				want = "existing"
			}
			if string(b) != want {
				t.Fatalf("got %q", b)
			}
			if r := f.run(f.files("two")); r.State != "success" {
				t.Fatal(r)
			}
			_, err := os.Stat(name)
			if policy == "RETAIN" && err != nil {
				t.Fatal("retained unmanaged file became owned/deleted")
			}
			if policy == "OVERWRITE" && !os.IsNotExist(err) {
				t.Fatal("overwritten managed file was not cleaned up")
			}
		})
	}
}

func TestHookEnvironmentAndBasicPermissions(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "do-not-inherit")
	t.Setenv("OTHER_SECRET", "do-not-inherit")
	f := newFixture(t)
	files := f.files("one")
	current, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	files = change(files, "appspec.yml", func(s string) string {
		return strings.ReplaceAll(s, "timeout: 5", "timeout: 5\n      runas: "+current.Username) + fmt.Sprintf("permissions:\n  - object: %s/version\n    mode: \"0600\"\n", f.config.InstallRoot)
	})
	files = change(files, "scripts/ValidateService.sh", func(s string) string {
		return s + `test -z "${GITHUB_TOKEN:-}"
test -z "${OTHER_SECRET:-}"
test "$APPLICATION_NAME" = myapp
test "$DEPLOYMENT_ID" = 1
test "$DEPLOYMENT_GROUP_NAME" = myapp-production
test -f manifest.json
`
	})
	if r := f.run(files); r.State != "success" {
		t.Fatal(r)
	}
	info, err := os.Stat(filepath.Join(f.config.InstallRoot, "version"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("permissions not applied: %v %v", info, err)
	}
}

func TestInterruptedInstallationIsUnknown(t *testing.T) {
	f := newFixture(t)
	files := change(f.files("one"), "scripts/ValidateService.sh", func(s string) string { return s + "sleep 30\n" })
	f.config.ExecutionTimeout = 500 * time.Millisecond
	if r := f.run(files); !r.Unknown || r.State != "error" {
		t.Fatal(r)
	}
}

func TestMissingPreviousHistoryFailsClosed(t *testing.T) {
	f := newFixture(t)
	f.previous = t.TempDir()
	if r := f.run(f.files("one")); r.State != "error" || f.events() != "" {
		t.Fatal(r)
	}
}

func TestMappingsAndPermissionsTypes(t *testing.T) {
	f := newFixture(t)
	files := f.files("one")
	files = append(files, testutil.File{Name: "config.txt", Data: "config", Mode: 0644}, testutil.File{Name: "app/nested/data", Data: "nested", Mode: 0644})
	files = change(files, "appspec.yml", func(s string) string {
		s = strings.Replace(s, "hooks:\n", fmt.Sprintf("  - source: config.txt\n    destination: %s/config\nhooks:\n", f.config.InstallRoot), 1)
		return s + fmt.Sprintf("permissions:\n  - object: %s\n    type: [file]\n    mode: \"0600\"\n  - object: %s\n    type: [directory]\n    mode: \"0750\"\n", f.config.InstallRoot, f.config.InstallRoot)
	})
	if r := f.run(files); r.State != "success" {
		t.Fatal(r)
	}
	for name, want := range map[string]os.FileMode{"version": 0600, "config": 0750, "config/config.txt": 0644, "nested": 0750, "nested/data": 0644} {
		info, err := os.Stat(filepath.Join(f.config.InstallRoot, name))
		if err != nil || info.Mode().Perm() != want {
			t.Fatalf("%s: %v %v", name, info, err)
		}
	}
}

func TestDoctorChecksInstallRootWithoutCodeDeploy(t *testing.T) {
	f := newFixture(t)
	f.config.LockDir = filepath.Join(f.dir, "lock")
	f.config.TokenFile = filepath.Join(f.dir, "token")
	for _, dir := range []string{f.config.StateDir, f.config.LockDir} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(f.config.TokenFile, []byte("placeholder"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := Check(context.Background(), f.config); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(f.config.InstallRoot)
	if err != nil || len(entries) != 0 {
		t.Fatal("doctor left temporary files")
	}
	link := filepath.Join(f.dir, "linked-root")
	if err := os.Symlink(f.config.InstallRoot, link); err != nil {
		t.Fatal(err)
	}
	bad := f.config
	bad.InstallRoot = link
	if err := Check(context.Background(), bad); err == nil {
		t.Fatal("symlink install_root accepted")
	}
}
