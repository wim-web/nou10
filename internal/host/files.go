//go:build linux || darwin

package host

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"github.com/wim-web/nou10/internal/appspec"
	"github.com/wim-web/nou10/internal/config"
)

type filePlan struct {
	Source, Destination string
	Mode                os.FileMode
	Retain              bool
}
type installation struct {
	Version int      `json:"version"`
	Root    string   `json:"root"`
	Files   []string `json:"files"`
}

// Open the operator-owned root once; all file operations stay relative to this
// descriptor even if a path is subsequently renamed. No symlinks are accepted.
func openInstallRoot(name string) (*os.Root, error) {
	resolved, err := filepath.EvalSymlinks(name)
	if err != nil {
		return nil, err
	}
	if resolved != name {
		return nil, errors.New("install_root and its ancestors must not contain symlinks; use the canonical path")
	}
	return os.OpenRoot(name)
}
func cleanPath(root *os.Root, name string) error {
	if name == "." {
		return nil
	}
	if !filepath.IsLocal(name) {
		return errors.New("path escapes install_root")
	}
	current := ""
	parts := strings.Split(name, string(filepath.Separator))
	for i, part := range parts {
		current = filepath.Join(current, part)
		info, err := root.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return errors.New("symlink in destination path")
		}
		if i < len(parts)-1 && !info.IsDir() {
			return errors.New("destination parent is not a directory")
		}
	}
	return nil
}
func relativeDestination(installRoot, name string) (string, error) {
	if !appspec.Absolute(name) || !config.Contains(installRoot, name) {
		return "", errors.New("AppSpec destination is outside install_root")
	}
	return filepath.Rel(installRoot, name)
}

func planFiles(ctx context.Context, archive string, root *os.Root, spec *appspec.Spec, previous installation) ([]filePlan, []string, error) {
	sourceRoot, err := os.OpenRoot(archive)
	if err != nil {
		return nil, nil, err
	}
	defer sourceRoot.Close()
	owned := map[string]bool{}
	for _, p := range previous.Files {
		owned[p] = true
	}
	byDestination := map[string]filePlan{}
	dirs := map[string]bool{}
	for _, mapping := range spec.Files {
		source, _ := appspec.Source(mapping.Source)
		if err := cleanPath(sourceRoot, source); err != nil {
			return nil, nil, err
		}
		base, err := relativeDestination(root.Name(), mapping.Destination)
		if err != nil {
			return nil, nil, err
		}
		absoluteSource := filepath.Join(archive, source)
		info, err := os.Lstat(absoluteSource)
		if err != nil {
			return nil, nil, fmt.Errorf("missing files.source: %w", err)
		}
		if err := filepath.WalkDir(absoluteSource, func(filename string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			entry, err := d.Info()
			if err != nil {
				return err
			}
			if !entry.IsDir() && !entry.Mode().IsRegular() {
				return errors.New("files.source contains a link or special file")
			}
			rel, err := filepath.Rel(absoluteSource, filename)
			if err != nil {
				return err
			}
			if !info.IsDir() {
				rel = filepath.Base(source)
			}
			dest := filepath.Join(base, rel)
			if d.IsDir() {
				dirs[dest] = true
				return nil
			}
			if _, exists := byDestination[dest]; exists {
				return errors.New("multiple sources map to the same destination file")
			}
			byDestination[dest] = filePlan{Source: filename, Destination: dest, Mode: entry.Mode().Perm()}
			for p := filepath.Dir(dest); p != "."; p = filepath.Dir(p) {
				dirs[p] = true
			}
			return nil
		}); err != nil {
			return nil, nil, err
		}
	}
	var plans []filePlan
	for name, p := range byDestination {
		if dirs[name] {
			return nil, nil, errors.New("file and directory mappings overlap")
		}
		if err := cleanPath(root, name); err != nil {
			return nil, nil, err
		}
		info, err := root.Lstat(name)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, nil, err
		}
		if err == nil {
			if !info.Mode().IsRegular() {
				return nil, nil, errors.New("destination file is not a regular file")
			}
			if !owned[name] {
				switch spec.FileExistsBehavior {
				case "DISALLOW":
					return nil, nil, fmt.Errorf("unmanaged destination already exists: %s", name)
				case "RETAIN":
					p.Retain = true
				}
			}
		}
		plans = append(plans, p)
	}
	var directories []string
	for name := range dirs {
		if err := cleanPath(root, name); err != nil {
			return nil, nil, err
		}
		if info, err := root.Lstat(name); err == nil && !info.IsDir() {
			return nil, nil, errors.New("destination directory is not a directory")
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, nil, err
		}
		directories = append(directories, name)
	}
	sort.Slice(plans, func(i, j int) bool { return plans[i].Destination < plans[j].Destination })
	sort.Strings(directories)
	return plans, directories, nil
}

func installFiles(ctx context.Context, root *os.Root, plans []filePlan, dirs []string, previous installation) ([]string, error) {
	wanted := map[string]bool{}
	for _, p := range plans {
		wanted[p.Destination] = true
	}
	// Remove only files recorded by this target's last successful deployment.
	// Never recursively remove directories or files retained from outside nou10.
	for _, old := range previous.Files {
		if wanted[old] {
			continue
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := cleanPath(root, old); err != nil {
			return nil, err
		}
		info, err := root.Lstat(old)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() {
			return nil, errors.New("previously managed path is no longer a regular file")
		}
		if err := root.Remove(old); err != nil {
			return nil, err
		}
		if err := syncDirectory(root, filepath.Dir(old)); err != nil {
			return nil, err
		}
	}
	for _, dir := range dirs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := cleanPath(root, dir); err != nil {
			return nil, err
		}
		if err := root.MkdirAll(dir, 0755); err != nil {
			return nil, err
		}
	}
	var owned []string
	for _, p := range plans {
		if p.Retain {
			continue
		}
		if err := cleanPath(root, p.Destination); err != nil {
			return nil, err
		}
		if err := copyFile(ctx, root, p); err != nil {
			return nil, err
		}
		owned = append(owned, p.Destination)
	}
	// File fsyncs do not persist newly created ancestor directory entries.
	for _, dir := range dirs {
		if err := syncDirectory(root, dir); err != nil {
			return nil, err
		}
	}
	if err := syncDirectory(root, "."); err != nil {
		return nil, err
	}
	return owned, nil
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}
func copyFile(ctx context.Context, root *os.Root, p filePlan) error {
	source, err := os.Open(p.Source)
	if err != nil {
		return err
	}
	defer source.Close()
	info, err := source.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("source is not a regular file")
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return err
	}
	temp := filepath.Join(filepath.Dir(p.Destination), ".nou10-"+hex.EncodeToString(random[:])+".tmp")
	out, err := root.OpenFile(temp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer root.Remove(temp)
	defer out.Close()
	if _, err := io.Copy(out, contextReader{ctx, source}); err != nil {
		return err
	}
	if err := out.Chmod(p.Mode); err != nil {
		return err
	}
	if err := out.Sync(); err != nil {
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	if err := root.Rename(temp, p.Destination); err != nil {
		return err
	}
	return syncDirectory(root, filepath.Dir(p.Destination))
}
func syncDirectory(root *os.Root, name string) error {
	f, err := root.Open(name)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func userID(name string) (int, error) {
	u, err := user.Lookup(name)
	if err != nil {
		return -1, fmt.Errorf("unknown user %q", name)
	}
	return strconv.Atoi(u.Uid)
}
func groupID(name string) (int, error) {
	g, err := user.LookupGroup(name)
	if err != nil {
		return -1, fmt.Errorf("unknown group %q", name)
	}
	return strconv.Atoi(g.Gid)
}

type permissionPlan struct {
	Spec     appspec.Permission
	Name     string
	UID, GID int
	Mode     os.FileMode
}

func planPermissions(root *os.Root, spec *appspec.Spec) ([]permissionPlan, error) {
	var plans []permissionPlan
	for _, p := range spec.Permissions {
		name, err := relativeDestination(root.Name(), p.Object)
		if err != nil {
			return nil, err
		}
		if err := cleanPath(root, name); err != nil {
			return nil, err
		}
		plan := permissionPlan{Spec: p, Name: name, UID: -1, GID: -1}
		if p.Owner != "" {
			plan.UID, err = userID(p.Owner)
			if err != nil {
				return nil, err
			}
		}
		if p.Group != "" {
			plan.GID, err = groupID(p.Group)
			if err != nil {
				return nil, err
			}
		}
		mode, _ := p.FileMode()
		plan.Mode = os.FileMode(mode)
		plans = append(plans, plan)
	}
	return plans, nil
}
func applyPermissions(ctx context.Context, root *os.Root, plans []permissionPlan) error {
	for _, p := range plans {
		info, err := root.Lstat(p.Name)
		if err != nil {
			return err
		}
		if len(p.Spec.Type) > 0 && !info.IsDir() {
			return errors.New("permissions.type requires a directory object")
		}
		var selected []string
		err = fs.WalkDir(root.FS(), p.Name, func(name string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if !d.IsDir() && !d.Type().IsRegular() {
				return errors.New("permissions object contains a link or special file")
			}
			include := len(p.Spec.Type) == 0
			for _, kind := range p.Spec.Type {
				if kind == "directory" && d.IsDir() && name != p.Name {
					include = true
				}
				if kind == "file" && !d.IsDir() && filepath.Dir(name) == p.Name {
					include = true
				}
			}
			if include {
				selected = append(selected, name)
			}
			return nil
		})
		if err != nil {
			return err
		}
		// Change descendants before their parents, so a restrictive directory
		// mode does not prevent us from applying the remaining permissions.
		sort.Slice(selected, func(i, j int) bool {
			return strings.Count(selected[i], "/") > strings.Count(selected[j], "/") || strings.Count(selected[i], "/") == strings.Count(selected[j], "/") && selected[i] > selected[j]
		})
		for _, name := range selected {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := changePermission(root, name, p); err != nil {
				return err
			}
		}
	}
	return nil
}
func changePermission(root *os.Root, name string, p permissionPlan) error {
	if err := cleanPath(root, name); err != nil {
		return err
	}
	f, err := root.Open(name)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if info.Mode().IsRegular() {
		if stat, ok := info.Sys().(*syscall.Stat_t); ok && stat.Nlink > 1 {
			return errors.New("permissions on hard-linked files are unsupported")
		}
	} else if !info.IsDir() {
		return errors.New("permissions require a regular file or directory")
	}
	if p.UID != -1 || p.GID != -1 {
		if err := f.Chown(p.UID, p.GID); err != nil {
			return err
		}
	}
	if p.Spec.Mode != nil {
		if err := f.Chmod(p.Mode); err != nil {
			return err
		}
	}
	return f.Sync()
}
