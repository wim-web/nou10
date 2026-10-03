package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wim-web/nou10/internal/protocol"
	"gopkg.in/yaml.v3"
)

type Config struct {
	Repository       string        `yaml:"repository"`
	Application      string        `yaml:"application"`
	Environment      string        `yaml:"environment"`
	Task             string        `yaml:"task"`
	TokenFile        string        `yaml:"token_file"`
	PollInterval     time.Duration `yaml:"poll_interval"`
	StateDir         string        `yaml:"state_dir"`
	LockDir          string        `yaml:"lock_dir"`
	ExecutionTimeout time.Duration `yaml:"execution_timeout"`
	MaxDownloadBytes int64         `yaml:"max_download_bytes"`
	MaxExpandedBytes int64         `yaml:"max_expanded_bytes"`
	MaxFiles         int           `yaml:"max_files"`
	InstallRoot      string        `yaml:"install_root"`
	// Opt in to Git deployments; install_root is the persistent checkout.
	GitScript string `yaml:"git_script"`
}

func (c Config) Target() protocol.Target {
	return protocol.Target{Repository: c.Repository, Application: c.Application, Environment: c.Environment, Task: c.Task}
}

func Load(path string) (Config, error) {
	c := Config{Task: protocol.DefaultTask, PollInterval: 30 * time.Second, ExecutionTimeout: 10 * time.Minute,
		StateDir: "/var/lib/nou10", LockDir: "/run/nou10", MaxDownloadBytes: 512 << 20, MaxExpandedBytes: 2 << 30, MaxFiles: 100000}
	f, err := os.Open(path)
	if err != nil {
		return c, err
	}
	defer f.Close()
	dec := yaml.NewDecoder(io.LimitReader(f, 1<<20))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return c, fmt.Errorf("read config: %w", err)
	}
	if dec.Decode(new(any)) != io.EOF {
		return c, errors.New("config must contain one YAML document")
	}
	return c, c.Validate()
}

func (c Config) Validate() error {
	if !protocol.ValidRepository(c.Repository) {
		return errors.New("repository must be owner/repository")
	}
	if !protocol.ValidName(c.Application) || !protocol.ValidName(c.Environment) {
		return errors.New("application and environment must use letters, numbers, dot, underscore or hyphen")
	}
	if c.Task == "" || len(c.Task) > 128 || strings.ContainsAny(c.Task, "\r\n") {
		return errors.New("invalid task")
	}
	for _, p := range []string{c.TokenFile, c.StateDir, c.LockDir, c.InstallRoot} {
		if !filepath.IsAbs(p) {
			return errors.New("token_file, state_dir, lock_dir and install_root must be absolute paths")
		}
	}
	if filepath.Clean(c.InstallRoot) != c.InstallRoot || c.InstallRoot == "/" {
		return errors.New("install_root must be a clean absolute directory other than /")
	}
	if c.GitScript != "" && (!filepath.IsLocal(c.GitScript) || filepath.Clean(c.GitScript) != c.GitScript ||
		c.GitScript == "." || strings.ContainsAny(c.GitScript, "\\:\x00\r\n") || strings.EqualFold(strings.Split(c.GitScript, "/")[0], ".git")) {
		return errors.New("git_script must be a clean relative file path outside .git")
	}
	for _, protected := range []string{c.StateDir, c.LockDir, c.TokenFile} {
		if Contains(c.InstallRoot, protected) || Contains(protected, c.InstallRoot) {
			return errors.New("install_root must not overlap state_dir, lock_dir or token_file")
		}
	}
	if c.PollInterval < time.Second || c.ExecutionTimeout < time.Second || c.MaxDownloadBytes < 1 || c.MaxExpandedBytes < 1 || c.MaxFiles < 1 {
		return errors.New("invalid duration or size limit")
	}
	if c.MaxDownloadBytes > 1<<40 || c.MaxExpandedBytes > 1<<40 || c.MaxFiles > 1000000 {
		return errors.New("size limits exceed supported range")
	}
	return nil
}

// ReadToken is also called for each API request, so replacing a token file takes effect without restarting.
func ReadToken(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", errors.New("cannot stat token_file")
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return "", errors.New("token_file must be a regular file with mode 0600 or 0400")
	}
	f, err := os.Open(path)
	if err != nil {
		return "", errors.New("cannot read token_file")
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) || opened.Mode().Perm()&0077 != 0 {
		return "", errors.New("token_file changed while opening")
	}
	b, err := io.ReadAll(io.LimitReader(f, 16385))
	if err != nil || len(b) > 16384 {
		return "", errors.New("cannot read token_file or token too large")
	}
	token := strings.TrimSpace(string(b))
	if token == "" || strings.ContainsAny(token, "\r\n\t ") {
		return "", errors.New("token_file contains an invalid token")
	}
	return token, nil
}

// Contains checks component boundaries, not string prefixes.
func Contains(root, name string) bool {
	rel, err := filepath.Rel(root, name)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}
