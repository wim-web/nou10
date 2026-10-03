// Package ledger stores the entire small MVP ledger in a synchronous bbolt transaction.
package ledger

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"

	"github.com/wim-web/nou10/internal/protocol"
	bolt "go.etcd.io/bbolt"
)

type Entry struct {
	Key           string              `json:"key"`
	Deployment    protocol.Deployment `json:"deployment"`
	Payload       protocol.Payload    `json:"payload"`
	Fingerprint   string              `json:"fingerprint"`
	DeploymentIDs []int64             `json:"deployment_ids"`
	Phase         string              `json:"phase"`
	Status        string              `json:"status"`
	Summary       string              `json:"summary"`
	StartedAt     *time.Time          `json:"started_at,omitempty"`
	FinishedAt    *time.Time          `json:"finished_at,omitempty"`
	BundlePath    string              `json:"bundle_path,omitempty"`
}

type Report struct {
	DeploymentID int64           `json:"deployment_id"`
	Status       protocol.Status `json:"status"`
}

type State struct {
	Version     int               `json:"version"`
	Target      protocol.Target   `json:"target"`
	Initialized bool              `json:"initialized"`
	Baseline    int64             `json:"baseline"`
	Held        bool              `json:"held"`
	HoldReason  string            `json:"hold_reason,omitempty"`
	LastSuccess string            `json:"last_success,omitempty"`
	Entries     map[string]*Entry `json:"entries"`
	Seen        map[int64]string  `json:"seen"`
	Outbox      []Report          `json:"outbox"`
}

type Store struct{ db *bolt.DB }

var bucket = []byte("ledger")
var key = []byte("state")

func Open(dir string, t protocol.Target) (*Store, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("state_dir must be a real directory with mode 0700")
	}
	path := filepath.Join(dir, "ledger.db")
	if info, err := os.Lstat(path); err == nil && (!info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0) {
		return nil, errors.New("ledger.db must be a regular file with mode 0600")
	}
	db, err := bolt.Open(path, 0600, &bolt.Options{Timeout: time.Second})
	if err != nil {
		return nil, fmt.Errorf("open ledger (another process may hold the lock): %w", err)
	}
	s := &Store{db: db}
	err = db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists(bucket)
		if err != nil {
			return err
		}
		if b.Get(key) == nil {
			v := State{Version: 1, Target: t, Entries: map[string]*Entry{}, Seen: map[int64]string{}}
			data, _ := json.Marshal(v)
			return b.Put(key, data)
		}
		return nil
	})
	if err == nil {
		var st *State
		st, err = s.Read()
		if err == nil && (st.Version != 1 || st.Target != t) {
			err = errors.New("ledger version or target does not match configuration; do not reuse state_dir")
		}
	}
	if err != nil {
		db.Close()
		return nil, err
	}
	// Persist the database directory entry before accepting deployments.
	directory, err := os.Open(dir)
	if err == nil {
		err = directory.Sync()
		_ = directory.Close()
	}
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }
func (s *Store) Read() (*State, error) {
	var st State
	err := s.db.View(func(tx *bolt.Tx) error { return json.Unmarshal(tx.Bucket(bucket).Get(key), &st) })
	return &st, err
}
func (s *Store) Update(fn func(*State) error) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		var st State
		b := tx.Bucket(bucket)
		if err := json.Unmarshal(b.Get(key), &st); err != nil {
			return err
		}
		if err := fn(&st); err != nil {
			return err
		}
		data, err := json.Marshal(st)
		if err != nil {
			return err
		}
		return b.Put(key, data)
	})
}

func (s *State) Enqueue(e *Entry) {
	for _, id := range e.DeploymentIDs {
		s.Report(id, e.Status, e.Summary)
	}
}
func (s *State) Report(id int64, status, summary string) {
	if text := []rune(summary); len(text) > 140 {
		summary = string(text[:137]) + "..."
	}
	s.Outbox = append(s.Outbox, Report{DeploymentID: id, Status: protocol.Status{State: status, Description: summary, AutoInactive: false}})
}
func (s *State) Transition(key, phase, status, summary string, now time.Time) error {
	e := s.Entries[key]
	if e == nil {
		return errors.New("missing ledger entry")
	}
	e.Phase = phase
	e.Status = status
	e.Summary = summary
	if phase == "starting" {
		e.StartedAt = &now
	}
	if phase == "done" || phase == "unknown" {
		e.FinishedAt = &now
	}
	if status == "success" {
		s.LastSuccess = key
	}
	if phase == "unknown" {
		s.Held = true
		s.HoldReason = summary
	}
	s.Enqueue(e)
	return nil
}

// Recover never replays an execution for which a start intent was persisted.
func (s *Store) Recover(now time.Time) error {
	return s.Update(func(st *State) error {
		for key, e := range st.Entries {
			if e.Phase == "starting" || e.Phase == "running" {
				if err := st.Transition(key, "unknown", "error", "execution outcome unknown after restart; operator recovery required", now); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

func (s *Store) Ingest(ds []protocol.Deployment, now time.Time) error {
	sort.Slice(ds, func(i, j int) bool {
		if ds[i].CreatedAt.Equal(ds[j].CreatedAt) {
			return ds[i].ID < ds[j].ID
		}
		return ds[i].CreatedAt.Before(ds[j].CreatedAt)
	})
	return s.Update(func(st *State) error {
		if !st.Initialized {
			for _, d := range ds {
				st.Baseline = max(st.Baseline, d.ID)
			}
			st.Initialized = true
			return nil
		}
		for _, d := range ds {
			if d.ID <= st.Baseline || st.Seen[d.ID] != "" || d.Environment != st.Target.Environment || d.Task != st.Target.Task || protocol.Application(d.Payload) != st.Target.Application {
				continue
			}
			p, err := protocol.Parse(d)
			entryKey := "request:" + p.RequestID
			fp := protocol.Fingerprint(d, p)
			if err == nil {
				if old := st.Entries[entryKey]; old != nil {
					if old.Fingerprint == fp {
						old.DeploymentIDs = append(old.DeploymentIDs, d.ID)
						st.Seen[d.ID] = entryKey
						st.Report(d.ID, old.Status, old.Summary)
						continue
					}
					err = errors.New("request_id was reused with different contents")
				}
			}
			if err != nil {
				entryKey = "rejected:" + strconv.FormatInt(d.ID, 10)
			}
			e := &Entry{Key: entryKey, Deployment: d, Payload: p, Fingerprint: fp, DeploymentIDs: []int64{d.ID}, Phase: "queued", Status: "queued", Summary: "accepted; waiting to start"}
			st.Entries[entryKey] = e
			st.Seen[d.ID] = entryKey
			if err != nil {
				if e := st.Transition(entryKey, "done", "error", err.Error(), now); e != nil {
					return e
				}
			} else {
				st.Enqueue(e)
			}
		}
		return nil
	})
}

func (s *State) Pending() []*Entry {
	var out []*Entry
	for _, e := range s.Entries {
		if e.Phase != "done" && e.Phase != "unknown" {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i].Deployment, out[j].Deployment
		if a.CreatedAt.Equal(b.CreatedAt) {
			return a.ID < b.ID
		}
		return a.CreatedAt.Before(b.CreatedAt)
	})
	return out
}
