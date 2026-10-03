package ledger

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/wim-web/nou10/internal/protocol"
	"github.com/wim-web/nou10/internal/testutil"
)

func TestBaselineDedupConflictAndRestart(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	now := time.Now().UTC().Truncate(time.Second)
	bundle := testutil.Bundle(t)
	s, err := Open(dir, testutil.Target())
	if err != nil {
		t.Fatal(err)
	}
	old := testutil.Deployment(1, "old", now, bundle)
	if err := s.Ingest([]protocol.Deployment{old}, now); err != nil {
		t.Fatal(err)
	}
	d := testutil.Deployment(2, "r", now, bundle)
	dup := d
	dup.ID = 3
	conflict := d
	conflict.ID = 4
	p, _ := protocol.Parse(conflict)
	p.AssetID++
	conflict.Payload, _ = json.Marshal(p)
	if err := s.Ingest([]protocol.Deployment{conflict, dup, d, old}, now); err != nil {
		t.Fatal(err)
	}
	st, _ := s.Read()
	if st.Baseline != 1 || len(st.Entries) != 2 || len(st.Entries["request:r"].DeploymentIDs) != 2 {
		t.Fatalf("bad state: %+v", st)
	}
	if st.Entries["rejected:4"].Status != "error" {
		t.Fatal("conflict accepted")
	}
	if err := s.Update(func(st *State) error { return st.Transition("request:r", "done", "success", "done", now) }); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(dir, testutil.Target())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Recover(now); err != nil {
		t.Fatal(err)
	}
	if err := s.Ingest([]protocol.Deployment{d, dup, conflict}, now); err != nil {
		t.Fatal(err)
	}
	st, _ = s.Read()
	if len(st.Pending()) != 0 || st.Held || st.LastSuccess != "request:r" {
		t.Fatalf("completed request replayed: %+v", st)
	}
	late := d
	late.ID = 5
	if err := s.Ingest([]protocol.Deployment{late}, now); err != nil {
		t.Fatal(err)
	}
	st, _ = s.Read()
	last := st.Outbox[len(st.Outbox)-1]
	if last.DeploymentID != 5 || last.Status.State != "success" {
		t.Fatalf("late duplicate got %+v", last)
	}
}

func TestCrashHoldsAndRollbackIsAtomic(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "state"), testutil.Target())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now().UTC()
	if err := s.Ingest(nil, now); err != nil {
		t.Fatal(err)
	}
	d := testutil.Deployment(1, "r", now, testutil.Bundle(t))
	if err := s.Ingest([]protocol.Deployment{d}, now); err != nil {
		t.Fatal(err)
	}
	if err := s.Update(func(st *State) error { return st.Transition("request:r", "starting", "in_progress", "starting", now) }); err != nil {
		t.Fatal(err)
	}
	if err := s.Recover(now); err != nil {
		t.Fatal(err)
	}
	st, _ := s.Read()
	if !st.Held || st.Entries["request:r"].Phase != "unknown" || len(st.Pending()) != 0 {
		t.Fatalf("did not hold: %+v", st)
	}
	if err := s.Update(func(st *State) error { st.Held = false; return assertError{} }); err == nil {
		t.Fatal("transaction unexpectedly succeeded")
	}
	st, _ = s.Read()
	if !st.Held {
		t.Fatal("failed transaction changed durable state")
	}
}

type assertError struct{}

func (assertError) Error() string { return "rollback" }

func TestTargetMismatch(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	s, err := Open(dir, testutil.Target())
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	target := testutil.Target()
	target.Application = "other"
	if s, err := Open(dir, target); err == nil {
		s.Close()
		t.Fatal("reused another target's ledger")
	}
}
