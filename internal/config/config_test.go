package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wim-web/nou10/internal/protocol"
)

func TestStrictConfigAndDefaults(t *testing.T) {
	text := "repository: owner/repo\napplication: app\nenvironment: production\ntoken_file: /etc/nou10/token\ninstall_root: /srv/app\n"
	path := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(path, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Task != protocol.DefaultTask || c.PollInterval.String() != "30s" {
		t.Fatal("wrong defaults")
	}
	if err := os.WriteFile(path, []byte(text+"poll_intervall: 1s\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("typo in config accepted")
	}
}
func TestTokenProtectionAndRotation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("secret\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if got, err := ReadToken(path); err != nil || got != "secret" {
		t.Fatalf("%s %v", got, err)
	}
	if err := os.WriteFile(path, []byte("rotated"), 0600); err != nil {
		t.Fatal(err)
	}
	if got, err := ReadToken(path); err != nil || got != "rotated" {
		t.Fatalf("%s %v", got, err)
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadToken(path); err == nil || strings.Contains(err.Error(), "rotated") {
		t.Fatal("token permissions not enforced or token leaked")
	}
	link := path + "-link"
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadToken(link); err == nil {
		t.Fatal("token symlink accepted")
	}
}

func TestInstallRootBounds(t *testing.T) {
	c := Config{Repository: "owner/repo", Application: "app", Environment: "production", Task: protocol.DefaultTask,
		TokenFile: "/etc/nou10/token", StateDir: "/var/lib/nou10", LockDir: "/run/nou10", InstallRoot: "/srv/app",
		PollInterval: time.Second, ExecutionTimeout: time.Minute, MaxDownloadBytes: 1, MaxExpandedBytes: 1, MaxFiles: 1}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, root := range []string{"/", "relative", "/srv/../etc", "/etc/nou10", "/var/lib/nou10/app", "/var/lib", "/run/nou10"} {
		bad := c
		bad.InstallRoot = root
		if err := bad.Validate(); err == nil {
			t.Fatalf("accepted root %q", root)
		}
	}
}
