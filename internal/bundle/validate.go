// Package bundle validates tgz archives without extracting or executing their contents.
package bundle

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path"
	"strings"

	"github.com/wim-web/nou10/internal/appspec"
)

type Limits struct {
	ExpandedBytes int64
	Files         int
}
type Manifest struct {
	SourceSHA string `json:"source_sha"`
}
type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(b []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(b)
}

func archivePath(name string) (string, error) {
	if name == "" || strings.ContainsAny(name, "\\:\x00") || strings.HasPrefix(name, "/") {
		return "", errors.New("unsafe archive path")
	}
	for _, p := range strings.Split(name, "/") {
		if p == ".." {
			return "", errors.New("parent traversal in archive")
		}
	}
	n := path.Clean(name)
	if len(n) > 4096 {
		return "", errors.New("archive path is too long")
	}
	return n, nil
}

func Validate(ctx context.Context, filename, sourceSHA string, limits Limits) error {
	f, err := os.Open(filename)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(contextReader{ctx, f})
	if err != nil {
		return errors.New("invalid gzip archive")
	}
	defer gz.Close()
	// Count all expanded bytes, including tar headers, padding and PAX metadata.
	lr := &io.LimitedReader{R: gz, N: limits.ExpandedBytes + 1}
	tr := tar.NewReader(lr)
	types := map[string]byte{}
	modes := map[string]int64{}
	var manifest, appspec []byte
	count := 0
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return errors.New("invalid tar archive or expanded size limit exceeded")
		}
		count++
		if count > limits.Files {
			return errors.New("archive entry limit exceeded")
		}
		name, err := archivePath(h.Name)
		if err != nil {
			return err
		}
		if _, exists := types[name]; exists {
			return errors.New("duplicate archive path")
		}
		if h.Typeflag != tar.TypeReg && h.Typeflag != tar.TypeDir {
			return errors.New("links, devices and special archive entries are unsupported")
		}
		if name == "." && h.Typeflag != tar.TypeDir {
			return errors.New("invalid archive root")
		}
		if h.Mode&06000 != 0 || len(h.Xattrs) > 0 {
			return errors.New("privileged archive metadata is unsupported")
		}
		for key := range h.PAXRecords {
			if strings.Contains(strings.ToLower(key), "sparse") {
				return errors.New("sparse archives are unsupported")
			}
		}
		if h.Size < 0 || h.Size > limits.ExpandedBytes {
			return errors.New("expanded size limit exceeded")
		}
		if h.Typeflag == tar.TypeDir && h.Size != 0 {
			return errors.New("directory contains unexpected data")
		}
		types[name] = h.Typeflag
		modes[name] = h.Mode
		if name == "manifest.json" || name == "appspec.yml" {
			if h.Typeflag != tar.TypeReg || h.Size > 1<<20 {
				return errors.New("invalid or oversized manifest/AppSpec")
			}
			b, err := io.ReadAll(tr)
			if err != nil {
				return errors.New("cannot read manifest/AppSpec")
			}
			if name == "manifest.json" {
				manifest = b
			} else {
				appspec = b
			}
		} else if _, err := io.Copy(io.Discard, tr); err != nil {
			return errors.New("truncated archive entry")
		}
		if lr.N <= 0 {
			return errors.New("expanded size limit exceeded")
		}
	}
	// Force gzip checksum verification and reject concealed concatenated tar content.
	buf := make([]byte, 32768)
	for {
		n, err := lr.Read(buf)
		for _, b := range buf[:n] {
			if b != 0 {
				return errors.New("unexpected trailing archive data")
			}
		}
		if lr.N <= 0 {
			return errors.New("expanded size limit exceeded")
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return errors.New("invalid gzip checksum or truncated archive")
		}
	}
	for name := range types {
		for parent := path.Dir(name); parent != "."; parent = path.Dir(parent) {
			if kind, ok := types[parent]; ok && kind != tar.TypeDir {
				return errors.New("archive parent is not a directory")
			}
		}
	}
	var m Manifest
	if len(manifest) == 0 || json.Unmarshal(manifest, &m) != nil || m.SourceSHA != sourceSHA {
		return errors.New("manifest source_sha does not match deployment SHA")
	}
	if len(appspec) == 0 {
		return errors.New("appspec.yml is required at archive root")
	}
	return validateAppSpec(appspec, types, modes)
}

func validateAppSpec(data []byte, types map[string]byte, modes map[string]int64) error {
	spec, err := appspec.Parse(data)
	if err != nil {
		return err
	}
	for _, hooks := range spec.Hooks {
		for _, hook := range hooks {
			name, err := archivePath(hook.Location)
			kind, exists := types[name]
			if err != nil || !exists || kind != tar.TypeReg || modes[name]&0111 == 0 {
				return errors.New("AppSpec hook must reference an executable regular file in the bundle")
			}
		}
	}
	return nil
}
