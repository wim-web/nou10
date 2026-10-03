//go:build linux || darwin

// Package host provides the local process boundary and OS target lock.
package host

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"

	"github.com/wim-web/nou10/internal/protocol"
)

type Lock struct{ file *os.File }

func Acquire(dir string, t protocol.Target) (*Lock, error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, err
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0022 != 0 {
		return nil, errors.New("lock_dir must be a real directory without group/other write permission")
	}
	fd, err := syscall.Open(filepath.Join(dir, t.Key()+".lock"), syscall.O_CREAT|syscall.O_RDWR|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0600)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), "target lock")
	if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, errors.New("target is already locked by another process")
	}
	return &Lock{file: f}, nil
}
func (l *Lock) Close() error { return l.file.Close() } // Keep the inode: unlinking permits split locks.
