//go:build linux || darwin

package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/wim-web/nou10/internal/appspec"
	"github.com/wim-web/nou10/internal/bundle"
	"github.com/wim-web/nou10/internal/config"
)

type Result struct {
	State, Summary string
	Unknown        bool
}
type Request struct {
	BundlePath, JobDir, SourceSHA string
	DeploymentID                  int64
	PreviousJobDir                string
}
type Executor interface {
	Run(context.Context, Request) Result
}
type Runner struct{ Config config.Config }

func Check(_ context.Context, c config.Config) error {
	root, err := openInstallRoot(c.InstallRoot)
	if err != nil {
		return fmt.Errorf("open install_root: %w", err)
	}
	defer root.Close()
	for _, name := range []string{c.StateDir, c.LockDir, c.TokenFile} {
		resolved, err := filepath.EvalSymlinks(name)
		if err != nil {
			return err
		}
		if config.Contains(c.InstallRoot, resolved) || config.Contains(resolved, c.InstallRoot) {
			return errors.New("install_root overlaps a protected path after resolving symlinks")
		}
	}
	// Prove directory writes and synchronization work without touching application files.
	f, err := os.CreateTemp(c.InstallRoot, ".nou10-doctor-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func (r Runner) Run(parent context.Context, request Request) Result {
	log, err := os.OpenFile(filepath.Join(request.JobDir, "deploy.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return Result{State: "error", Summary: "cannot open deployment log"}
	}
	defer log.Close()
	ctx, cancel := context.WithTimeout(parent, r.Config.ExecutionTimeout)
	defer cancel()
	fail := func(summary string, err error) Result {
		fmt.Fprintf(log, "%s: %v\n", summary, err)
		if ctx.Err() != nil {
			return interrupted()
		}
		return Result{State: "error", Summary: summary + "; inspect deploy.log"}
	}
	root, err := openInstallRoot(r.Config.InstallRoot)
	if err != nil {
		return fail("cannot open install_root", err)
	}
	defer root.Close()
	archive := filepath.Join(request.JobDir, "archive")
	limits := bundle.Limits{ExpandedBytes: r.Config.MaxExpandedBytes, Files: r.Config.MaxFiles}
	if err := bundle.Extract(ctx, request.BundlePath, request.SourceSHA, archive, limits); err != nil {
		return fail("bundle extraction failed", err)
	}
	spec, err := readSpec(archive)
	if err != nil {
		return fail("invalid AppSpec", err)
	}
	previous := installation{Version: 1, Root: r.Config.InstallRoot}
	var previousSpec *appspec.Spec
	previousArchive := ""
	if request.PreviousJobDir != "" {
		previous, err = readInstallation(request.PreviousJobDir, r.Config.InstallRoot)
		if err != nil {
			return fail("cannot load previous successful installation", err)
		}
		previousArchive = filepath.Join(request.PreviousJobDir, "archive")
		previousSpec, err = readSpec(previousArchive)
		if err != nil {
			return fail("cannot load previous AppSpec", err)
		}
		if err := checkHooks(previousArchive, previousSpec.Hooks["ApplicationStop"]); err != nil {
			return fail("invalid previous stop hook", err)
		}
	}
	for _, hooks := range spec.Hooks {
		if err := checkHooks(archive, hooks); err != nil {
			return fail("invalid hook", err)
		}
	}
	if _, _, err := planFiles(ctx, archive, root, spec, previous); err != nil {
		return fail("invalid file installation plan", err)
	}
	permissions, err := planPermissions(root, spec)
	if err != nil {
		return fail("invalid permissions plan", err)
	}
	if previousSpec != nil {
		if result := r.runEvent(ctx, request, previousArchive, "ApplicationStop", previousSpec.Hooks["ApplicationStop"], log); result != nil {
			return *result
		}
	}
	if result := r.runEvent(ctx, request, archive, "BeforeInstall", spec.Hooks["BeforeInstall"], log); result != nil {
		return *result
	}
	// Hooks may have changed destination files; enforce conflict policy again.
	plans, dirs, err := planFiles(ctx, archive, root, spec, previous)
	if err != nil {
		return fail("invalid file installation plan after hooks", err)
	}
	fmt.Fprintln(log, "Install")
	owned, err := installFiles(ctx, root, plans, dirs, previous)
	if err != nil {
		return fail("file installation failed", err)
	}
	if err := applyPermissions(ctx, root, permissions); err != nil {
		return fail("permission changes failed", err)
	}
	for _, event := range []string{"AfterInstall", "ApplicationStart", "ValidateService"} {
		if result := r.runEvent(ctx, request, archive, event, spec.Hooks[event], log); result != nil {
			return *result
		}
	}
	if ctx.Err() != nil {
		return interrupted()
	}
	// The ledger publishes this revision as successful only after this record is durable.
	if err := writeInstallation(request.JobDir, installation{Version: 1, Root: r.Config.InstallRoot, Files: owned}); err != nil {
		fmt.Fprintf(log, "cannot persist installation history: %v\n", err)
		return Result{State: "error", Unknown: true, Summary: "installation history persistence failed; operator recovery required"}
	}
	return Result{State: "success", Summary: "deployment and ValidateService completed"}
}

func readSpec(archive string) (*appspec.Spec, error) {
	f, err := os.Open(filepath.Join(archive, "appspec.yml"))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	if err != nil {
		return nil, err
	}
	return appspec.Parse(b)
}
func readInstallation(dir, root string) (installation, error) {
	var out installation
	f, err := os.Open(filepath.Join(dir, "installation.json"))
	if err != nil {
		return out, err
	}
	defer f.Close()
	dec := json.NewDecoder(io.LimitReader(f, 32<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&out); err != nil {
		return out, err
	}
	if dec.Decode(new(any)) != io.EOF || out.Version != 1 || out.Root != root {
		return out, errors.New("installation history version or root mismatch")
	}
	for _, name := range out.Files {
		if !filepath.IsLocal(name) || filepath.Clean(name) != name || name == "." {
			return out, errors.New("unsafe path in installation history")
		}
	}
	return out, nil
}
func writeInstallation(dir string, record installation) error {
	f, err := os.CreateTemp(dir, "installation-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if err := json.NewEncoder(f).Encode(record); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), filepath.Join(dir, "installation.json")); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func checkHooks(archive string, hooks []appspec.Hook) error {
	root, err := os.OpenRoot(archive)
	if err != nil {
		return err
	}
	defer root.Close()
	for _, h := range hooks {
		name, err := appspec.Relative(h.Location)
		if err != nil {
			return err
		}
		if err := cleanPath(root, name); err != nil {
			return err
		}
		info, err := root.Lstat(name)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
			return errors.New("hook must be an executable regular file")
		}
		if h.RunAs != "" {
			uid, err := userID(h.RunAs)
			if err != nil {
				return err
			}
			if uid != os.Geteuid() {
				return errors.New("runas may only name the agent's current user; user switching is unsupported")
			}
		}
	}
	return nil
}
func interrupted() Result {
	return Result{State: "error", Unknown: true, Summary: "execution interrupted or timed out; verify processes and recover target"}
}

func (r Runner) runEvent(ctx context.Context, request Request, archive, event string, hooks []appspec.Hook, log *os.File) *Result {
	for _, h := range hooks {
		fmt.Fprintf(log, "%s: %s\n", event, h.Location)
		if err := checkHooks(archive, []appspec.Hook{h}); err != nil {
			fmt.Fprintln(log, err)
			result := Result{State: "error", Summary: "hook validation failed; inspect deploy.log"}
			return &result
		}
		childCtx, cancel := context.WithTimeout(ctx, time.Duration(h.Seconds())*time.Second)
		cmd := exec.CommandContext(childCtx, filepath.Join(archive, h.Location))
		cmd.Dir = archive
		cmd.Stdout = log
		cmd.Stderr = log
		cmd.Env = []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C.UTF-8",
			"APPLICATION_NAME=" + r.Config.Application, "DEPLOYMENT_ID=" + strconv.FormatInt(request.DeploymentID, 10),
			"DEPLOYMENT_GROUP_ID=" + r.Config.Target().Key(), "DEPLOYMENT_GROUP_NAME=" + r.Config.Application + "-" + r.Config.Environment,
			"LIFECYCLE_EVENT=" + event, "NOU10_SOURCE_SHA=" + request.SourceSHA}
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		cmd.Cancel = func() error {
			err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			if errors.Is(err, syscall.ESRCH) {
				return os.ErrProcessDone
			}
			return err
		}
		cmd.WaitDelay = 5 * time.Second
		err := cmd.Run()
		interruptedRun := childCtx.Err() != nil
		cancel()
		if interruptedRun {
			result := interrupted()
			return &result
		}
		if err != nil {
			fmt.Fprintln(log, err)
			var exit *exec.ExitError
			if errors.As(err, &exit) {
				if status, ok := exit.Sys().(syscall.WaitStatus); ok && status.Signaled() {
					result := Result{State: "error", Unknown: true, Summary: "hook terminated by a signal; operator recovery required"}
					return &result
				}
				result := Result{State: "failure", Summary: fmt.Sprintf("%s hook exited with code %d; inspect deploy.log", event, exit.ExitCode())}
				return &result
			}
			result := Result{State: "error", Summary: "cannot start hook; inspect deploy.log"}
			return &result
		}
	}
	return nil
}
