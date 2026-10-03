package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/wim-web/nou10/internal/bundle"
	"github.com/wim-web/nou10/internal/config"
	"github.com/wim-web/nou10/internal/github"
	"github.com/wim-web/nou10/internal/host"
	"github.com/wim-web/nou10/internal/ledger"
)

type Agent struct {
	Config config.Config
	GitHub *github.Client
	Store  *ledger.Store
	Runner host.Executor
	Log    *slog.Logger
	Now    func() time.Time
}

func (a *Agent) now() time.Time {
	if a.Now != nil {
		return a.Now().UTC()
	}
	return time.Now().UTC()
}
func (a *Agent) log() *slog.Logger {
	if a.Log != nil {
		return a.Log
	}
	return slog.Default()
}

func (a *Agent) Run(ctx context.Context) error {
	if err := a.Store.Recover(a.now()); err != nil {
		return err
	}
	backoff := a.Config.PollInterval
	for {
		err := a.Step(ctx)
		var fatal *storageError
		if errors.As(err, &fatal) {
			return err
		}
		if ctx.Err() != nil {
			return nil
		}
		delay := max(a.Config.PollInterval, a.GitHub.PollInterval)
		if err != nil {
			delay = github.Delay(err, max(delay, backoff))
			backoff = min(max(backoff*2, time.Second), 15*time.Minute)
			a.log().Error("agent cycle failed", "error", err, "retry_after", delay)
		} else {
			backoff = a.Config.PollInterval
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
}

type storageError struct{ error }

func (a *Agent) update(fn func(*ledger.State) error) error {
	if err := a.Store.Update(fn); err != nil {
		return &storageError{err}
	}
	return nil
}

// Step performs one complete discovery/reporting cycle, then at most one execution.
// Callers hold the target OS lock for the entire agent lifetime.
func (a *Agent) Step(ctx context.Context) error {
	// Reporting is independent of discovery: a failed listing cannot starve results.
	if err := a.flush(ctx); err != nil {
		return err
	}
	ds, err := a.GitHub.ListDeployments(ctx, a.Config.Target())
	if err != nil {
		return err
	}
	before, err := a.Store.Read()
	if err != nil {
		return &storageError{err}
	}
	if err := a.Store.Ingest(ds, a.now()); err != nil {
		return &storageError{err}
	}
	if !before.Initialized {
		a.log().Info("initialized deployment baseline; ready for new requests")
	}
	if err := a.flush(ctx); err != nil {
		return err
	}
	st, err := a.Store.Read()
	if err != nil {
		return &storageError{err}
	}
	if st.Held {
		a.log().Warn("target held; operator recovery required", "reason", st.HoldReason)
		return nil
	}
	for _, e := range st.Pending() {
		if !a.now().Before(e.Payload.StartBefore) {
			if err := a.finish(e.Key, "done", "error", "start deadline expired"); err != nil {
				return err
			}
			continue
		}
		if err := a.execute(ctx, e); err != nil {
			return err
		}
		break
	}
	return a.flush(ctx)
}

func (a *Agent) flush(ctx context.Context) error {
	for {
		st, err := a.Store.Read()
		if err != nil {
			return &storageError{err}
		}
		if len(st.Outbox) == 0 {
			return nil
		}
		r := st.Outbox[0]
		if err := a.GitHub.Report(ctx, r.DeploymentID, r.Status); err != nil {
			return err
		}
		if err := a.update(func(s *ledger.State) error { s.Outbox = s.Outbox[1:]; return nil }); err != nil {
			return err
		}
	}
}

func (a *Agent) finish(key, phase, status, summary string) error {
	a.log().Info("deployment state", "request", key, "state", status, "summary", summary)
	return a.update(func(st *ledger.State) error { return st.Transition(key, phase, status, summary, a.now()) })
}

func (a *Agent) execute(ctx context.Context, e *ledger.Entry) error {
	gitSource := e.Payload.Source == "git"
	if gitSource != (a.Config.GitScript != "") {
		return a.finish(e.Key, "done", "error", "source does not match host configuration (git_script selects Git mode)")
	}
	jobDir := filepath.Join(a.Config.StateDir, "jobs", strconv.FormatInt(e.Deployment.ID, 10))
	filename := filepath.Join(jobDir, "bundle.tgz")
	if gitSource {
		filename = ""
	}
	if e.Phase != "starting" {
		if gitSource {
			if err := os.MkdirAll(jobDir, 0700); err != nil {
				return &storageError{err}
			}
		} else {
			if err := a.update(func(st *ledger.State) error { st.Entries[e.Key].Phase = "downloading"; return nil }); err != nil {
				return err
			}
			if err := a.prepare(ctx, e, jobDir, filename); err != nil {
				var invalid *invalidBundle
				if errors.As(err, &invalid) || github.Permanent(err) {
					return a.finish(e.Key, "done", "error", err.Error())
				}
				return err
			}
		}
		if !a.now().Before(e.Payload.StartBefore) {
			return a.finish(e.Key, "done", "error", "start deadline expired during preparation")
		}
		if err := a.update(func(st *ledger.State) error {
			st.Entries[e.Key].BundlePath = filename
			return st.Transition(e.Key, "starting", "in_progress", "starting local deployment", a.now())
		}); err != nil {
			return err
		}
	}
	// The persisted start intent is deliberately conservative: a crash here holds
	// the target, even if the child has not started yet.
	if err := a.flush(ctx); err != nil {
		return err
	}
	if !a.now().Before(e.Payload.StartBefore) {
		return a.finish(e.Key, "done", "error", "start deadline expired before execution")
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err := a.update(func(st *ledger.State) error { st.Entries[e.Key].Phase = "running"; return nil }); err != nil {
		return err
	}
	a.log().Info("running deployment", "deployment_id", e.Deployment.ID)
	st, err := a.Store.Read()
	if err != nil {
		return &storageError{err}
	}
	request := host.Request{Source: e.Payload.Source, BundlePath: filename, JobDir: jobDir, SourceSHA: e.Deployment.SHA, DeploymentID: e.Deployment.ID}
	if !gitSource && st.LastSuccess != "" {
		previous := st.Entries[st.LastSuccess]
		if previous == nil || previous.Status != "success" {
			return &storageError{errors.New("invalid last successful deployment reference")}
		}
		if previous.Payload.Source == "git" {
			return a.finish(e.Key, "done", "error", "cannot resume bundle installation history after a Git deployment")
		}
		if previous.BundlePath == "" {
			return &storageError{errors.New("missing last successful bundle path")}
		}
		request.PreviousJobDir = filepath.Dir(previous.BundlePath)
	}
	result := a.Runner.Run(ctx, request)
	phase := "done"
	if result.Unknown {
		phase = "unknown"
	}
	return a.finish(e.Key, phase, result.State, result.Summary)
}

type invalidBundle struct{ message string }

func (e *invalidBundle) Error() string { return e.message }

func (a *Agent) prepare(ctx context.Context, e *ledger.Entry, jobDir, filename string) error {
	asset, err := a.GitHub.FindAsset(ctx, e.Payload.ReleaseID, e.Payload.AssetID)
	if err != nil {
		return err
	}
	if asset.State != "uploaded" || !strings.HasSuffix(asset.Name, ".tgz") || asset.Size <= 0 || asset.Size > a.Config.MaxDownloadBytes {
		return &invalidBundle{"release asset must be an uploaded tgz within the download size limit"}
	}
	if err := os.MkdirAll(jobDir, 0700); err != nil {
		return &storageError{err}
	}
	f, err := os.CreateTemp(jobDir, "bundle-*.part")
	if err != nil {
		return &storageError{err}
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	defer f.Close()
	hash := sha256.New()
	sink := &writerPair{file: f, hash: hash}
	n, err := a.GitHub.Download(ctx, asset.ID, sink, a.Config.MaxDownloadBytes)
	if sink.err != nil {
		return sink.err
	}
	if err != nil {
		return err
	}
	if n != asset.Size {
		return &invalidBundle{"release asset size mismatch"}
	}
	if hex.EncodeToString(hash.Sum(nil)) != e.Payload.SHA256 {
		return &invalidBundle{"bundle SHA-256 mismatch"}
	}
	if err := f.Sync(); err != nil {
		return &storageError{err}
	}
	if err := f.Close(); err != nil {
		return &storageError{err}
	}
	if err := bundle.Validate(ctx, tmp, e.Deployment.SHA, bundle.Limits{ExpandedBytes: a.Config.MaxExpandedBytes, Files: a.Config.MaxFiles}); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return &invalidBundle{fmt.Sprintf("bundle rejected: %s", err)}
	}
	if err := os.Rename(tmp, filename); err != nil {
		return &storageError{err}
	}
	dir, err := os.Open(jobDir)
	if err != nil {
		return &storageError{err}
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil {
		return &storageError{err}
	}
	return nil
}

// Keep local write failures distinct from retryable network failures.
type byteWriter interface{ Write([]byte) (int, error) }
type writerPair struct {
	file byteWriter
	hash byteWriter
	err  error
}

func (w *writerPair) Write(p []byte) (int, error) {
	n, err := w.file.Write(p)
	if err == nil && n != len(p) {
		err = io.ErrShortWrite
	}
	if err != nil {
		w.err = &storageError{err}
		return n, w.err
	}
	_, err = w.hash.Write(p[:n])
	return n, err
}
