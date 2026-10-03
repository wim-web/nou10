package bundle

import (
	"archive/tar"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wim-web/nou10/internal/testutil"
)

func TestValidate(t *testing.T) {
	cases := []struct {
		name   string
		change func([]testutil.File) []testutil.File
		want   string
	}{
		{name: "valid"},
		{name: "dot prefixes", change: func(f []testutil.File) []testutil.File {
			for i := range f {
				f[i].Name = "./" + f[i].Name
			}
			return f
		}},
		{name: "traversal", change: func(f []testutil.File) []testutil.File { return append(f, testutil.File{Name: "../escape"}) }, want: "traversal"},
		{name: "absolute", change: func(f []testutil.File) []testutil.File { return append(f, testutil.File{Name: "/tmp/escape"}) }, want: "unsafe"},
		{name: "symlink", change: func(f []testutil.File) []testutil.File {
			return append(f, testutil.File{Name: "link", Type: tar.TypeSymlink, Link: "/etc"})
		}, want: "links"},
		{name: "hardlink", change: func(f []testutil.File) []testutil.File {
			return append(f, testutil.File{Name: "link", Type: tar.TypeLink, Link: "scripts/validate.sh"})
		}, want: "links"},
		{name: "device", change: func(f []testutil.File) []testutil.File {
			return append(f, testutil.File{Name: "device", Type: tar.TypeChar})
		}, want: "special"},
		{name: "duplicates", change: func(f []testutil.File) []testutil.File { return append(f, f[0]) }, want: "duplicate"},
		{name: "parent file", change: func(f []testutil.File) []testutil.File { return append(f, testutil.File{Name: "scripts"}) }, want: "parent"},
		{name: "suid", change: func(f []testutil.File) []testutil.File { f[2].Mode = 04755; return f }, want: "privileged"},
		{name: "wrong sha", change: func(f []testutil.File) []testutil.File { f[1].Data = `{"source_sha":"no"}`; return f }, want: "source_sha"},
		{name: "missing validation", change: func(f []testutil.File) []testutil.File { f[0].Data = "version: 0.0\nos: linux\n"; return f }, want: "ValidateService"},
		{name: "missing hook", change: func(f []testutil.File) []testutil.File { return f[:2] }, want: "executable"},
		{name: "nonexecutable hook", change: func(f []testutil.File) []testutil.File { f[2].Mode = 0644; return f }, want: "executable"},
		{name: "outside hook", change: func(f []testutil.File) []testutil.File {
			f[0].Data = strings.ReplaceAll(f[0].Data, "scripts/validate.sh", "../validate.sh")
			return f
		}, want: "relative file"},
		{name: "duplicate yaml key", change: func(f []testutil.File) []testutil.File { f[0].Data += "os: windows\n"; return f }, want: "AppSpec"},
		{name: "yaml documents", change: func(f []testutil.File) []testutil.File { f[0].Data += "---\nos: linux\n"; return f }, want: "one YAML"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			files := testutil.Files()
			if tc.change != nil {
				files = tc.change(files)
			}
			name := filepath.Join(t.TempDir(), "bundle.tgz")
			if err := os.WriteFile(name, testutil.Archive(t, files), 0600); err != nil {
				t.Fatal(err)
			}
			err := Validate(context.Background(), name, testutil.SHA, Limits{ExpandedBytes: 1 << 20, Files: 100})
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want %q", err, tc.want)
			}
		})
	}
}

func TestArchiveLimitsAndChecksum(t *testing.T) {
	data := testutil.Bundle(t)
	for _, tc := range []struct {
		name   string
		data   []byte
		limits Limits
	}{
		{"expanded", data, Limits{ExpandedBytes: 100, Files: 100}},
		{"entry count", data, Limits{ExpandedBytes: 1 << 20, Files: 2}},
		{"truncated", data[:len(data)-4], Limits{ExpandedBytes: 1 << 20, Files: 100}},
		{"concatenated", append(append([]byte{}, data...), data...), Limits{ExpandedBytes: 1 << 20, Files: 100}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			name := filepath.Join(t.TempDir(), "bundle.tgz")
			if err := os.WriteFile(name, tc.data, 0600); err != nil {
				t.Fatal(err)
			}
			if err := Validate(context.Background(), name, testutil.SHA, tc.limits); err == nil {
				t.Fatal("unsafe archive accepted")
			}
		})
	}
}
