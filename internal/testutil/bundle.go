// Package testutil contains deterministic fixtures; nothing here contacts GitHub.
package testutil

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/wim-web/nou10/internal/protocol"
)

const SHA = "1111111111111111111111111111111111111111"

type File struct {
	Name, Data string
	Type       byte
	Mode       int64
	Link       string
}

func Archive(t *testing.T, files []File) []byte {
	t.Helper()
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	tw := tar.NewWriter(gz)
	for _, f := range files {
		kind := f.Type
		if kind == 0 {
			kind = tar.TypeReg
		}
		mode := f.Mode
		if mode == 0 {
			mode = 0755
		}
		h := &tar.Header{Name: f.Name, Typeflag: kind, Mode: mode, Size: int64(len(f.Data)), Linkname: f.Link}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(f.Data)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}
func Files() []File {
	return []File{
		{Name: "appspec.yml", Data: "version: 0.0\nos: linux\nhooks:\n  ValidateService:\n    - location: scripts/validate.sh\n      timeout: 30\n"},
		{Name: "manifest.json", Data: `{"source_sha":"` + SHA + `"}`},
		{Name: "scripts/validate.sh", Data: "#!/bin/sh\nexit 0\n"},
	}
}
func Bundle(t *testing.T) []byte { return Archive(t, Files()) }
func Digest(b []byte) string     { return fmt.Sprintf("%x", sha256.Sum256(b)) }
func Target() protocol.Target {
	return protocol.Target{Repository: "owner/repo", Application: "app", Environment: "production", Task: protocol.DefaultTask}
}
func Deployment(id int64, request string, now time.Time, bundle []byte) protocol.Deployment {
	p := protocol.Payload{SchemaVersion: 1, RequestID: request, Application: "app", ReleaseID: 10, AssetID: 20, SHA256: Digest(bundle), StartBefore: now.Add(15 * time.Minute)}
	raw, _ := json.Marshal(p)
	return protocol.Deployment{ID: id, SHA: SHA, Ref: SHA, Environment: "production", Task: protocol.DefaultTask, Payload: raw, CreatedAt: now}
}
