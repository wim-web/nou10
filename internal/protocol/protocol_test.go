package protocol_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/wim-web/nou10/internal/protocol"
	"github.com/wim-web/nou10/internal/testutil"
)

func TestPayloadContract(t *testing.T) {
	d := testutil.Deployment(1, "r", time.Now().UTC(), nil)
	if _, err := protocol.Parse(d); err != nil {
		t.Fatal(err)
	}
	quoted := d
	quoted.Payload, _ = json.Marshal(string(d.Payload))
	if _, err := protocol.Parse(quoted); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{
		strings.Replace(string(d.Payload), `"schema_version":1`, `"schema_version":2`, 1),
		strings.Replace(string(d.Payload), `"application":"app"`, `"application":"app","command":"evil"`, 1),
		`null`, `[]`, string(d.Payload) + `{}`,
	} {
		bad := d
		bad.Payload = []byte(raw)
		if _, err := protocol.Parse(bad); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
}

func TestGitPayloadContract(t *testing.T) {
	good := protocol.Payload{SchemaVersion: 2, Source: "git", RequestID: "git", Application: "app", StartBefore: time.Now().UTC()}
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*protocol.Payload){
		func(p *protocol.Payload) { p.Source = "" },
		func(p *protocol.Payload) { p.Source = "ssh" },
		func(p *protocol.Payload) { p.SchemaVersion = 1 },
		func(p *protocol.Payload) { p.SchemaVersion = 3 },
		func(p *protocol.Payload) { p.ReleaseID = 1 },
		func(p *protocol.Payload) { p.AssetID = 1 },
		func(p *protocol.Payload) { p.SHA256 = strings.Repeat("a", 64) },
	} {
		bad := good
		change(&bad)
		if err := bad.Validate(); err == nil {
			t.Fatalf("accepted %+v", bad)
		}
	}
	d := testutil.Deployment(1, "git", time.Now().UTC(), nil)
	d.Payload, _ = json.Marshal(good)
	if _, err := protocol.Parse(d); err != nil {
		t.Fatal(err)
	}
	raw := strings.TrimSuffix(string(d.Payload), "}") + `,"script":"evil"}`
	d.Payload = []byte(raw)
	if _, err := protocol.Parse(d); err == nil {
		t.Fatal("accepted remote script selection")
	}
}

// This value was computed from the schema-1 JSON contract, before Source existed.
func TestExistingBundleFingerprintIsStable(t *testing.T) {
	d := testutil.Deployment(1, "r", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), nil)
	p, err := protocol.Parse(d)
	if err != nil {
		t.Fatal(err)
	}
	if got := protocol.Fingerprint(d, p); got != "a4a327b7095dd6f79ac780dd729efc48cb4a44ac43950a9c27a327245e6b6f3b" {
		t.Fatalf("existing ledger fingerprint changed: %s", got)
	}
}
