//go:build linux || darwin

package host

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/wim-web/nou10/internal/appspec"
	"github.com/wim-web/nou10/internal/config"
	"github.com/wim-web/nou10/internal/protocol"
)

func (r Runner) runGit(ctx context.Context, request Request, root *os.Root, log *os.File) Result {
	fail := func(err error) Result {
		fmt.Fprintln(log, err)
		if ctx.Err() != nil {
			return interrupted()
		}
		return Result{State: "error", Summary: "Git source preparation failed; inspect deploy.log"}
	}
	if r.Config.GitScript == "" || !protocol.ValidSHA(request.SourceSHA) || !protocol.ValidRepository(r.Config.Repository) {
		return fail(errors.New("Git source requires git_script, repository and a full commit SHA"))
	}
	info, err := root.Lstat(".git")
	if errors.Is(err, os.ErrNotExist) {
		// init + fetch also works when the operator has already placed .env here.
		if _, err := r.git(ctx, log, false, "init", "--template=", "."); err != nil {
			return fail(err)
		}
	} else if err != nil || !info.IsDir() {
		return fail(errors.New(".git must be a real directory; linked worktrees are unsupported"))
	}
	top, err := r.git(ctx, log, false, "rev-parse", "--show-toplevel")
	if err != nil || top != r.Config.InstallRoot {
		return fail(errors.New("install_root must be the Git working tree root"))
	}
	dirty, err := r.git(ctx, log, false, "status", "--porcelain", "--untracked-files=no")
	if err != nil {
		return fail(err)
	}
	if dirty != "" {
		return fail(errors.New("checkout has tracked local changes; refusing to overwrite them"))
	}
	url, err := r.git(ctx, log, false, "ls-remote", "--get-url", r.gitURL())
	if err != nil || url != r.gitURL() {
		return fail(errors.New("Git URL rewriting must not change the configured repository"))
	}
	// Fetch the requested SHA from the configured repository, not a mutable branch
	// or the checkout's origin. A queued request must report the revision it ran.
	if _, err := r.git(ctx, log, true, "fetch", "--no-tags", "--depth=1", "--", r.gitURL(), request.SourceSHA); err != nil {
		return fail(err)
	}
	fetched, err := r.git(ctx, log, false, "rev-parse", "--verify", "FETCH_HEAD^{commit}")
	if err != nil || fetched != request.SourceSHA {
		return fail(errors.New("fetched commit does not match deployment SHA"))
	}
	// Git normally overwrites ignored files on checkout; .env must be preserved.
	if _, err := r.git(ctx, log, false, "checkout", "--detach", "--no-overwrite-ignore", request.SourceSHA, "--"); err != nil {
		return fail(err)
	}
	if _, err := r.git(ctx, log, false, "ls-files", "--error-unmatch", "--", r.Config.GitScript); err != nil {
		return fail(errors.New("git_script must be tracked in the requested commit"))
	}
	hook := appspec.Hook{Location: r.Config.GitScript}
	if err := checkHooks(r.Config.InstallRoot, []appspec.Hook{hook}); err != nil {
		return fail(err)
	}
	if result := r.runEvent(ctx, request, r.Config.InstallRoot, "GitDeploy", []appspec.Hook{hook}, log); result != nil {
		return *result
	}
	if ctx.Err() != nil {
		return interrupted()
	}
	return Result{State: "success", Summary: "Git checkout and deployment script completed"}
}

func (r Runner) gitURL() string { return "https://github.com/" + r.Config.Repository + ".git" }

func (r Runner) git(ctx context.Context, log io.Writer, authenticated bool, args ...string) (string, error) {
	options := []string{"--literal-pathspecs", "-c", "core.hooksPath=/dev/null", "-c", "credential.helper=",
		"-c", "http.extraHeader=", "-c", "http.followRedirects=false",
		"-c", "protocol.allow=never", "-c", "protocol.https.allow=always"}
	env := []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C.UTF-8",
		"GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_NO_REPLACE_OBJECTS=1"}
	if authenticated {
		token, err := config.ReadToken(r.Config.TokenFile)
		if err != nil {
			return "", err
		}
		// Keep credentials out of argv, .git/config and the deployment script's env.
		options = append(options, "--config-env=http."+r.gitURL()+".extraHeader=NOU10_GIT_AUTHORIZATION")
		env = append(env, "NOU10_GIT_AUTHORIZATION=Authorization: Basic "+base64.StdEncoding.EncodeToString([]byte("x-access-token:"+token)))
	}
	cmd := exec.CommandContext(ctx, "git", append(options, args...)...)
	cmd.Dir = r.Config.InstallRoot
	cmd.Env = env
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = log
	processGroup(cmd)
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w", args[0], err)
	}
	return strings.TrimSpace(output.String()), nil
}
