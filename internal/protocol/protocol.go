// Package protocol defines the versioned Deployment contract shared by the CLI and agent.
package protocol

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"
)

const DefaultTask = "deploy:nou10"

var shaPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)
var digestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
var namePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)
var repoPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

func ValidSHA(s string) bool  { return shaPattern.MatchString(s) }
func ValidName(s string) bool { return namePattern.MatchString(s) }
func ValidRepository(s string) bool {
	if !repoPattern.MatchString(s) {
		return false
	}
	for _, p := range strings.Split(s, "/") {
		if p == "." || p == ".." {
			return false
		}
	}
	return true
}

type Target struct {
	Repository  string `json:"repository"`
	Application string `json:"application"`
	Environment string `json:"environment"`
	Task        string `json:"task"`
}

// Key deliberately excludes task: changing the task must not bypass a target's lock.
func (t Target) Key() string {
	b, _ := json.Marshal([]string{strings.ToLower(t.Repository), t.Application, t.Environment})
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

type Payload struct {
	SchemaVersion int       `json:"schema_version"`
	RequestID     string    `json:"request_id"`
	Application   string    `json:"application"`
	ReleaseID     int64     `json:"release_id"`
	AssetID       int64     `json:"asset_id"`
	SHA256        string    `json:"sha256"`
	StartBefore   time.Time `json:"start_before"`
}

type Deployment struct {
	ID          int64           `json:"id"`
	SHA         string          `json:"sha"`
	Ref         string          `json:"ref"`
	Environment string          `json:"environment"`
	Task        string          `json:"task"`
	Payload     json.RawMessage `json:"payload"`
	CreatedAt   time.Time       `json:"created_at"`
}

type CreateDeployment struct {
	Ref              string   `json:"ref"`
	Environment      string   `json:"environment"`
	Task             string   `json:"task"`
	AutoMerge        bool     `json:"auto_merge"`
	RequiredContexts []string `json:"required_contexts"`
	Payload          Payload  `json:"payload"`
}

type Status struct {
	ID           int64  `json:"id,omitempty"`
	State        string `json:"state"`
	Description  string `json:"description"`
	AutoInactive bool   `json:"auto_inactive"`
}

func Terminal(state string) bool {
	return state == "success" || state == "failure" || state == "error" || state == "inactive"
}

func payloadBytes(raw json.RawMessage) ([]byte, error) {
	b := bytes.TrimSpace(raw)
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return nil, err
		}
		b = []byte(s)
	}
	if len(b) == 0 || b[0] != '{' {
		return nil, errors.New("payload must be a JSON object")
	}
	return b, nil
}

// Application permits routing before validating the rest of an untrusted payload.
func Application(raw json.RawMessage) string {
	b, err := payloadBytes(raw)
	if err != nil {
		return ""
	}
	var p struct {
		Application string `json:"application"`
	}
	_ = json.Unmarshal(b, &p)
	return p.Application
}

func Parse(d Deployment) (Payload, error) {
	var p Payload
	b, err := payloadBytes(d.Payload)
	if err != nil {
		return p, err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return p, errors.New("invalid payload fields")
	}
	if dec.Decode(new(any)) != io.EOF {
		return p, errors.New("trailing payload data")
	}
	if d.ID <= 0 || !ValidSHA(d.SHA) || d.Ref != d.SHA {
		return p, errors.New("ref and sha must be the same full commit SHA")
	}
	if d.CreatedAt.IsZero() {
		return p, errors.New("missing deployment creation time")
	}
	return p, p.Validate()
}

func (p Payload) Validate() error {
	switch {
	case p.SchemaVersion != 1:
		return errors.New("unsupported schema_version")
	case len(p.RequestID) == 0 || len(p.RequestID) > 256 || strings.ContainsAny(p.RequestID, "\x00\r\n"):
		return errors.New("invalid request_id")
	case !ValidName(p.Application):
		return errors.New("invalid application")
	case p.ReleaseID <= 0 || p.AssetID <= 0:
		return errors.New("release_id and asset_id must be positive")
	case !digestPattern.MatchString(p.SHA256):
		return errors.New("sha256 must be 64 lowercase hex characters")
	case p.StartBefore.IsZero():
		return errors.New("start_before is required")
	}
	_, offset := p.StartBefore.Zone()
	if offset != 0 {
		return errors.New("start_before must be UTC")
	}
	return nil
}

func Fingerprint(d Deployment, p Payload) string {
	b, _ := json.Marshal(struct {
		SHA         string
		Environment string
		Task        string
		Payload     Payload
	}{d.SHA, d.Environment, d.Task, p})
	return fmt.Sprintf("%x", sha256.Sum256(b))
}
