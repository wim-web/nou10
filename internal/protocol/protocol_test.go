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
