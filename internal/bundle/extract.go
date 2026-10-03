package bundle

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
)

// Extract first validates the entire archive, then creates a new private tree.
// It never extracts over an existing tree, preserves only ordinary file modes,
// and confines every write with os.Root. Callers keep the source bundle private.
func Extract(ctx context.Context, filename, sourceSHA, destination string, limits Limits) error {
	if err := Validate(ctx, filename, sourceSHA, limits); err != nil {
		return err
	}
	if err := os.Mkdir(destination, 0700); err != nil {
		return err
	}
	root, err := os.OpenRoot(destination)
	if err != nil {
		return err
	}
	defer root.Close()
	f, err := os.Open(filename)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(contextReader{ctx, f})
	if err != nil {
		return err
	}
	defer gz.Close()
	lr := &io.LimitedReader{R: gz, N: limits.ExpandedBytes + 1}
	tr := tar.NewReader(lr)
	for count := 0; ; count++ {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if count >= limits.Files {
			return errors.New("archive entry limit exceeded")
		}
		name, err := archivePath(h.Name)
		if err != nil {
			return err
		}
		if h.Typeflag == tar.TypeDir {
			if err := root.MkdirAll(name, 0700); err != nil {
				return err
			}
			continue
		}
		if h.Typeflag != tar.TypeReg || h.Mode&06000 != 0 {
			return errors.New("unsafe archive entry")
		}
		if err := root.MkdirAll(path.Dir(name), 0700); err != nil {
			return err
		}
		out, err := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(out, tr)
		if copyErr == nil {
			copyErr = out.Chmod(os.FileMode(h.Mode) & 0777)
		}
		if copyErr == nil {
			copyErr = out.Sync()
		}
		closeErr := out.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
		if lr.N <= 0 {
			return errors.New("expanded size limit exceeded")
		}
	}
	// Persist directory entries too, because this tree contains the stop hook
	// used by the next deployment after a successful ledger commit.
	return filepath.WalkDir(destination, func(name string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		f, err := os.Open(name)
		if err != nil {
			return err
		}
		defer f.Close()
		return f.Sync()
	})
}
