package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"time"

	"github.com/wim-web/nou10/internal/agent"
	"github.com/wim-web/nou10/internal/config"
	"github.com/wim-web/nou10/internal/github"
	"github.com/wim-web/nou10/internal/host"
	"github.com/wim-web/nou10/internal/ledger"
	"github.com/wim-web/nou10/internal/protocol"
)

const usage = `nou10: pull-based deployments through GitHub

Usage:
  nou10 agent   --config /etc/nou10/config.yml
  nou10 deploy  --repository owner/repo --application app --environment production
                --sha COMMIT --source git [--wait]
  nou10 deploy  --repository owner/repo --application app --environment production
                --sha COMMIT --release-id ID --asset-id ID --sha256 HASH [--wait]
  nou10 status  --repository owner/repo --deployment-id ID [--wait]
  nou10 doctor  --config /etc/nou10/config.yml
  nou10 inspect --config /etc/nou10/config.yml
  nou10 recover --config /etc/nou10/config.yml --confirm-stopped
  nou10 version

deploy/status read GITHUB_TOKEN (or --token-file); host commands require token_file in config.
Run a command with --help for its flags.
`

func Run(ctx context.Context, args []string, out, errOut io.Writer, version string) error {
	if len(args) == 0 {
		_, err := io.WriteString(out, usage)
		return err
	}
	switch args[0] {
	case "help", "--help", "-h":
		_, err := io.WriteString(out, usage)
		return err
	case "version", "--version":
		_, err := fmt.Fprintln(out, "nou10", version)
		return err
	case "deploy":
		return deploy(ctx, args[1:], out, errOut)
	case "status":
		return status(ctx, args[1:], out, errOut)
	case "agent", "doctor", "inspect", "recover":
		return local(ctx, args[0], args[1:], out, errOut)
	default:
		return fmt.Errorf("unknown command %q; use nou10 --help", args[0])
	}
}

func flags(name string, errOut io.Writer) *flag.FlagSet {
	f := flag.NewFlagSet(name, flag.ContinueOnError)
	f.SetOutput(errOut)
	return f
}
func parse(f *flag.FlagSet, args []string) error {
	if err := f.Parse(args); err != nil {
		return err
	}
	if f.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	return nil
}

type remoteFlags struct{ repository, tokenFile string }

func (r *remoteFlags) bind(f *flag.FlagSet) {
	f.StringVar(&r.repository, "repository", os.Getenv("GITHUB_REPOSITORY"), "GitHub owner/repository")
	f.StringVar(&r.tokenFile, "token-file", "", "restricted token file; otherwise read GITHUB_TOKEN")
}
func (r remoteFlags) client() (*github.Client, error) {
	token := func() (string, error) {
		if r.tokenFile != "" {
			return config.ReadToken(r.tokenFile)
		}
		v := os.Getenv("GITHUB_TOKEN")
		if v == "" {
			return "", errors.New("GITHUB_TOKEN is required")
		}
		return v, nil
	}
	if _, err := token(); err != nil {
		return nil, err
	}
	return github.New(github.APIURL, r.repository, token, nil)
}

func deploy(ctx context.Context, args []string, out, errOut io.Writer) error {
	f := flags("deploy", errOut)
	var remote remoteFlags
	remote.bind(f)
	var app, env, task, sha, digest, requestID, startBefore, source string
	var releaseID, assetID int64
	var wait bool
	var waitTimeout, startWithin, poll time.Duration
	f.StringVar(&app, "application", "", "registered application")
	f.StringVar(&env, "environment", "", "exact target environment")
	f.StringVar(&task, "task", protocol.DefaultTask, "deployment task")
	f.StringVar(&sha, "sha", "", "full 40-character source commit SHA")
	f.StringVar(&source, "source", "bundle", "bundle (Release asset) or git (host checkout and configured git_script)")
	f.StringVar(&digest, "sha256", "", "tgz SHA-256 (lowercase hex)")
	f.StringVar(&requestID, "request-id", "", "idempotency key (defaults to GHA run/attempt/target)")
	f.StringVar(&startBefore, "start-before", "", "explicit RFC3339 UTC start deadline")
	f.DurationVar(&startWithin, "start-within", 15*time.Minute, "start deadline from now")
	f.Int64Var(&releaseID, "release-id", 0, "existing release ID in the same repository")
	f.Int64Var(&assetID, "asset-id", 0, "uploaded tgz asset ID belonging to the release")
	f.BoolVar(&wait, "wait", false, "wait for agent success; fail on every other terminal state")
	f.DurationVar(&waitTimeout, "wait-timeout", 30*time.Minute, "client wait limit; does not cancel the host")
	f.DurationVar(&poll, "poll-interval", 30*time.Second, "minimum status poll interval")
	if err := parse(f, args); err != nil {
		return err
	}
	if !protocol.ValidSHA(sha) || !protocol.ValidName(env) || task == "" || len(task) > 128 {
		return errors.New("valid --sha, --environment and --task are required")
	}
	if startWithin <= 0 || waitTimeout <= 0 || poll < time.Second {
		return errors.New("invalid timeout or poll interval")
	}
	target := protocol.Target{Repository: remote.repository, Application: app, Environment: env, Task: task}
	if requestID == "" {
		run, attempt := os.Getenv("GITHUB_RUN_ID"), os.Getenv("GITHUB_RUN_ATTEMPT")
		if _, err := strconv.ParseUint(run, 10, 64); err != nil {
			return errors.New("--request-id is required outside GitHub Actions")
		}
		if _, err := strconv.ParseUint(attempt, 10, 64); err != nil {
			return errors.New("GITHUB_RUN_ATTEMPT is required for an automatic request_id")
		}
		requestID = run + ":" + attempt + ":" + target.Key()
	}
	deadline := time.Now().UTC().Add(startWithin).Truncate(time.Second)
	if startBefore != "" {
		var err error
		deadline, err = time.Parse(time.RFC3339, startBefore)
		if err != nil {
			return errors.New("invalid --start-before")
		}
	}
	p := protocol.Payload{SchemaVersion: 1, RequestID: requestID, Application: app, ReleaseID: releaseID, AssetID: assetID, SHA256: digest, StartBefore: deadline}
	switch source {
	case "git":
		p.SchemaVersion, p.Source = 2, "git"
	case "bundle":
	default:
		return errors.New("--source must be bundle or git")
	}
	if err := p.Validate(); err != nil {
		return err
	}
	if !time.Now().Before(deadline) {
		return errors.New("start deadline is already expired")
	}
	c, err := remote.client()
	if err != nil {
		return err
	}
	if source == "bundle" {
		asset, err := c.FindAsset(ctx, releaseID, assetID)
		if err != nil {
			return err
		}
		if asset.State != "uploaded" || filepath.Ext(asset.Name) != ".tgz" || asset.Size <= 0 {
			return errors.New("asset must be a fully uploaded tgz in the specified release")
		}
	}
	d, err := c.CreateDeployment(ctx, protocol.CreateDeployment{Ref: sha, Environment: env, Task: task, AutoMerge: false, RequiredContexts: []string{}, Payload: p})
	if err != nil {
		return fmt.Errorf("create deployment: %w; request may have been accepted; inspect GitHub before retrying", err)
	}
	if err := json.NewEncoder(out).Encode(map[string]any{"deployment_id": d.ID, "request_id": requestID, "start_before": deadline}); err != nil {
		return err
	}
	if wait {
		return Wait(ctx, c, d.ID, waitTimeout, poll, errOut)
	}
	return nil
}

func status(ctx context.Context, args []string, out, errOut io.Writer) error {
	f := flags("status", errOut)
	var remote remoteFlags
	remote.bind(f)
	var id int64
	var wait bool
	var timeout, poll time.Duration
	f.Int64Var(&id, "deployment-id", 0, "Deployment ID to observe")
	f.BoolVar(&wait, "wait", false, "wait until success or failure")
	f.DurationVar(&timeout, "wait-timeout", 30*time.Minute, "wait limit; does not cancel the host")
	f.DurationVar(&poll, "poll-interval", 30*time.Second, "minimum poll interval")
	if err := parse(f, args); err != nil {
		return err
	}
	if id <= 0 || timeout <= 0 || poll < time.Second {
		return errors.New("positive deployment ID and valid durations are required")
	}
	c, err := remote.client()
	if err != nil {
		return err
	}
	if wait {
		return Wait(ctx, c, id, timeout, poll, out)
	}
	s, err := c.LatestStatus(ctx, id)
	if err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(s)
}

func Wait(ctx context.Context, c *github.Client, id int64, timeout, poll time.Duration, out io.Writer) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	backoff := poll
	last := ""
	for {
		s, err := c.LatestStatus(ctx, id)
		if ctx.Err() != nil {
			return errors.New("deployment wait ended; host execution is not cancelled")
		}
		delay := max(poll, c.PollInterval)
		if err == nil {
			backoff = poll
			if s.State != last {
				fmt.Fprintf(out, "deployment %d: %s\n", id, s.State)
				last = s.State
			}
			if s.State == "success" {
				return nil
			}
			if protocol.Terminal(s.State) {
				return fmt.Errorf("deployment %d ended with %s", id, s.State)
			}
		} else {
			var api *github.Error
			if errors.As(err, &api) && (api.Kind == "authentication" || api.Kind == "permanent") {
				return err
			}
			delay = github.Delay(err, max(delay, backoff))
			backoff = min(max(backoff*2, time.Second), 5*time.Minute)
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return errors.New("deployment wait ended; host execution is not cancelled")
		case <-timer.C:
		}
	}
}

func local(ctx context.Context, command string, args []string, out, errOut io.Writer) error {
	f := flags(command, errOut)
	var filename string
	var confirmed bool
	f.StringVar(&filename, "config", "/etc/nou10/config.yml", "host configuration")
	if command == "recover" {
		f.BoolVar(&confirmed, "confirm-stopped", false, "confirm you checked host state and stopped remaining deployment processes")
	}
	if err := parse(f, args); err != nil {
		return err
	}
	c, err := config.Load(filename)
	if err != nil {
		return err
	}
	if command == "recover" && !confirmed {
		return errors.New("recovery requires --confirm-stopped after checking host state and remaining processes")
	}
	if command == "agent" && runtime.GOOS != "linux" {
		return errors.New("the deployment agent supports Linux only")
	}
	lock, err := host.Acquire(c.LockDir, c.Target())
	if err != nil {
		return err
	}
	defer lock.Close()
	store, err := ledger.Open(c.StateDir, c.Target())
	if err != nil {
		return err
	}
	defer store.Close()
	if command == "inspect" {
		st, err := store.Read()
		if err != nil {
			return err
		}
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(st)
	}
	if command == "recover" {
		// Also detects a crash when recovery is run before the next agent startup.
		if err := store.Recover(time.Now().UTC()); err != nil {
			return err
		}
		err := store.Update(func(st *ledger.State) error {
			if !st.Held {
				return errors.New("target is not held")
			}
			for _, e := range st.Pending() {
				if err := st.Transition(e.Key, "done", "error", "discarded during recovery; submit a new request", time.Now().UTC()); err != nil {
					return err
				}
			}
			st.Held = false
			st.HoldReason = ""
			st.Initialized = false
			return nil
		})
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(out, "hold cleared; start the agent, wait for baseline initialization, then create a new deployment")
		return err
	}
	if _, err := config.ReadToken(c.TokenFile); err != nil {
		return err
	}
	client, err := github.New(github.APIURL, c.Repository, func() (string, error) { return config.ReadToken(c.TokenFile) }, nil)
	if err != nil {
		return err
	}
	if err := host.Check(ctx, c); err != nil {
		return err
	}
	if command == "doctor" {
		if err := client.Probe(ctx, c.Target()); err != nil {
			return err
		}
		_, err = fmt.Fprintln(out, "OK: config, token file, local locks/state, writable install_root, repository/contents/deployments reads\nDeployment status write permission requires Deployments: write; read-only doctor cannot prove it.\nReal deployments must still be validated on the target OS.")
		return err
	}
	a := agent.Agent{Config: c, GitHub: client, Store: store, Runner: host.Runner{Config: c}}
	return a.Run(ctx)
}
