// Package appspec defines the Linux AppSpec subset implemented by nou10.
package appspec

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

var Events = []string{"ApplicationStop", "BeforeInstall", "AfterInstall", "ApplicationStart", "ValidateService"}

type Spec struct {
	Version            string            `yaml:"version"`
	OS                 string            `yaml:"os"`
	Files              []Mapping         `yaml:"files"`
	FileExistsBehavior string            `yaml:"file_exists_behavior"`
	Permissions        []Permission      `yaml:"permissions"`
	Hooks              map[string][]Hook `yaml:"hooks"`
}
type Mapping struct {
	Source      string `yaml:"source"`
	Destination string `yaml:"destination"`
}
type Hook struct {
	Location string `yaml:"location"`
	Timeout  *int   `yaml:"timeout"`
	RunAs    string `yaml:"runas"`
}

func (h Hook) Seconds() int {
	if h.Timeout == nil {
		return 3600
	}
	return *h.Timeout
}

type Permission struct {
	Object string   `yaml:"object"`
	Owner  string   `yaml:"owner"`
	Group  string   `yaml:"group"`
	Mode   *string  `yaml:"mode"`
	Type   []string `yaml:"type"`
}

func Relative(name string) (string, error) {
	if name == "" || strings.ContainsAny(name, "\\:\x00") || strings.HasPrefix(name, "/") {
		return "", errors.New("path must be relative to the bundle")
	}
	for _, part := range strings.Split(name, "/") {
		if part == ".." {
			return "", errors.New("parent traversal is forbidden")
		}
	}
	n := path.Clean(name)
	if len(n) > 4096 {
		return "", errors.New("path is too long")
	}
	return n, nil
}
func Source(name string) (string, error) {
	if name == "/" {
		return ".", nil
	}
	return Relative(name)
}
func Absolute(name string) bool {
	return filepath.IsAbs(name) && filepath.Clean(name) == name && !strings.ContainsAny(name, "\\\x00")
}
func (p Permission) FileMode() (uint32, error) {
	if p.Mode == nil {
		return 0, nil
	}
	value, err := strconv.ParseUint(*p.Mode, 8, 12)
	if err != nil || value > 0777 {
		return 0, errors.New("permissions.mode must be an octal value between 0000 and 0777")
	}
	return uint32(value), nil
}

func Parse(data []byte) (*Spec, error) {
	if len(data) > 1<<20 {
		return nil, errors.New("AppSpec exceeds 1 MiB")
	}
	var node yaml.Node
	if err := yaml.Unmarshal(data, &node); err != nil {
		return nil, errors.New("invalid AppSpec YAML")
	}
	var inspect func(*yaml.Node) error
	inspect = func(n *yaml.Node) error {
		if n.Kind == yaml.AliasNode || n.Tag == "!!merge" {
			return errors.New("AppSpec YAML aliases and merges are unsupported")
		}
		for _, c := range n.Content {
			if err := inspect(c); err != nil {
				return err
			}
		}
		return nil
	}
	if err := inspect(&node); err != nil {
		return nil, err
	}
	var spec Spec
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&spec); err != nil {
		return nil, fmt.Errorf("invalid or unsupported AppSpec field: %w", err)
	}
	if dec.Decode(new(any)) != io.EOF {
		return nil, errors.New("AppSpec must contain one YAML document")
	}
	if spec.Version != "0.0" || spec.OS != "linux" {
		return nil, errors.New("AppSpec must have version 0.0 and os linux")
	}
	if spec.FileExistsBehavior == "" {
		spec.FileExistsBehavior = "DISALLOW"
	}
	switch spec.FileExistsBehavior {
	case "DISALLOW", "OVERWRITE", "RETAIN":
	default:
		return nil, errors.New("unsupported file_exists_behavior")
	}
	for _, m := range spec.Files {
		if _, err := Source(m.Source); err != nil {
			return nil, fmt.Errorf("invalid files.source: %w", err)
		}
		if !Absolute(m.Destination) || m.Destination == "/" {
			return nil, errors.New("files.destination must be a clean absolute directory other than /")
		}
	}
	for _, p := range spec.Permissions {
		if !Absolute(p.Object) {
			return nil, errors.New("permissions.object must be a clean absolute path")
		}
		if _, err := p.FileMode(); err != nil {
			return nil, err
		}
		for _, kind := range p.Type {
			if kind != "file" && kind != "directory" {
				return nil, errors.New("permissions.type must contain file or directory")
			}
		}
		if p.Mode == nil && p.Owner == "" && p.Group == "" {
			return nil, errors.New("permissions entry needs mode, owner or group")
		}
	}
	if len(spec.Hooks["ValidateService"]) == 0 {
		return nil, errors.New("AppSpec requires a ValidateService hook")
	}
	for event, hooks := range spec.Hooks {
		known := false
		for _, name := range Events {
			known = known || name == event
		}
		if !known {
			return nil, fmt.Errorf("unsupported hook event: %s", event)
		}
		total := 0
		for _, h := range hooks {
			name, err := Relative(h.Location)
			if err != nil || name == "." {
				return nil, errors.New("hook location must be a relative file path")
			}
			if h.Seconds() < 1 || h.Seconds() > 3600 {
				return nil, errors.New("hook timeout must be 1..3600 seconds")
			}
			total += h.Seconds()
		}
		if total > 3600 {
			return nil, errors.New("sum of hook timeouts for an event exceeds 3600 seconds")
		}
	}
	return &spec, nil
}
