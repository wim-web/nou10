//go:build linux || darwin

package host

import (
	"testing"

	"github.com/wim-web/nou10/internal/testutil"
)

func TestTargetLock(t *testing.T) {
	dir := t.TempDir()
	target := testutil.Target()
	lock, err := Acquire(dir, target)
	if err != nil {
		t.Fatal(err)
	}
	otherTask := target
	otherTask.Task = "different"
	if second, err := Acquire(dir, otherTask); err == nil {
		second.Close()
		t.Fatal("second process acquired same target")
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := Acquire(dir, target)
	if err != nil {
		t.Fatal(err)
	}
	second.Close()
}
