// Copyright 2017 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package gps

import (
	"bytes"
	"context"
	"encoding/xml"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"fmt"
	"github.com/Masterminds/vcs"
	"github.com/pkg/errors"
	"os/exec"
	"syscall"
)

type ctxRepo interface {
	vcs.Repo
	get(context.Context) error
	fetch(context.Context) error
	updateVersion(context.Context, string) error
	//ping(context.Context) (bool, error)
}

// ensureCleaner is an optional extension of ctxRepo.
type ensureCleaner interface {
	// ensureClean ensures a repository is clean and in working order,
	// or returns an error if the adaptive recovery attempts fail.
	ensureClean(context.Context) error
}

// original implementation of these methods come from
// https://github.com/Masterminds/vcs

type gitRepo struct {
	*vcs.GitRepo
	proxyURL string
}

const (
	proxyTypeHTTP  = "http.proxy"
	proxyTypeHTTPS = "https.proxy"
)

func (gr gitRepo) ensureProxy() (err error) {
	if gr.proxyURL == "" {
		if err = gr.unsetProxyInternal(proxyTypeHTTP); err != nil {
			return
		}
		if err = gr.unsetProxyInternal(proxyTypeHTTPS); err != nil {
			return
		}
		return
	}
	var proxyURL string
	if proxyURL, err = gr.getProxyInternal(proxyTypeHTTP); err != nil {
		return
	}
	if proxyURL != gr.proxyURL {
		if err = gr.setProxyInternal(proxyTypeHTTP); err != nil {
			return
		}
	}
	if proxyURL, err = gr.getProxyInternal(proxyTypeHTTPS); err != nil {
		return
	}
	if proxyURL != gr.proxyURL {
		if err = gr.setProxyInternal(proxyTypeHTTPS); err != nil {
			return
		}
	}
	return
}

func (gr gitRepo) setProxyInternal(proxyType string) (err error) {
	_, err = gr.GitRepo.RunFromDir("git", "config", proxyType, gr.proxyURL)
	return
}

func (gr gitRepo) unsetProxyInternal(proxyType string) (err error) {
	if _, err = gr.GitRepo.RunFromDir("git", "config", "--unset", proxyType); err != nil {
		if isExitWithCode(err, 5) {
			err = nil
		}
	}
	return
}

func (gr gitRepo) getProxyInternal(proxyType string) (proxyURL string, err error) {
	var out []byte
	if out, err = gr.GitRepo.RunFromDir("git", "config", "--get", "https.proxy"); err != nil {
		if isExitWithCode(err, 1) {
			err = nil
		}
	}
	if out != nil {
		proxyURL = strings.TrimSpace(string(out))
	}
	return
}

func isExitWithCode(err error, code int) (is bool) {
	var ee *exec.ExitError
	if ee, is = err.(*exec.ExitError); !is {
		return
	}
	is = code == ee.Sys().(syscall.WaitStatus).ExitStatus()
	return
}

func newVcsRemoteErrorOr(err error, args []string, out, msg string) error {
	if err == context.Canceled || err == context.DeadlineExceeded {
		return err
	}
	return vcs.NewRemoteError(msg, errors.Wrapf(err, "command failed: %v", args), out)
}

func newVcsLocalErrorOr(err error, args []string, out, msg string) error {
	if err == context.Canceled || err == context.DeadlineExceeded {
		return err
	}
	return vcs.NewLocalError(msg, errors.Wrapf(err, "command failed: %v", args), out)
}

func (r gitRepo) Ping() bool {
	if err := r.ensureProxy(); err != nil {
		return false
	}
	return r.GitRepo.Ping()
}

func (r *gitRepo) get(ctx context.Context) error {
	args := make([]string, 0, 8)
	if r.proxyURL != "" {
		args = append(args,
			"-c", fmt.Sprintf("http.proxy=%s", r.proxyURL),
			"-c", fmt.Sprintf("https.proxy=%s", r.proxyURL))
	}
	args = append(args,
		"clone",
		"--recursive",
		"-v",
		"--progress",
		r.Remote(),
		r.LocalPath())
	cmd := commandContext(ctx, "git", args...)
	// Ensure no prompting for PWs
	cmd.SetEnv(append([]string{"GIT_ASKPASS=", "GIT_TERMINAL_PROMPT=0"}, os.Environ()...))
	if out, err := cmd.CombinedOutput(); err != nil {
		return newVcsRemoteErrorOr(err, cmd.Args(), string(out),
			"unable to get repository")
	}

	return nil
}

func (r *gitRepo) fetch(ctx context.Context) error {
	if err := r.ensureProxy(); err != nil {
		return err
	}
	cmd := commandContext(
		ctx,
		"git",
		"fetch",
		"--tags",
		"--prune",
		r.RemoteLocation,
	)
	cmd.SetDir(r.LocalPath())
	// Ensure no prompting for PWs
	cmd.SetEnv(append([]string{"GIT_ASKPASS=", "GIT_TERMINAL_PROMPT=0"}, os.Environ()...))
	if out, err := cmd.CombinedOutput(); err != nil {
		return newVcsRemoteErrorOr(err, cmd.Args(), string(out),
			"unable to update repository")
	}
	return nil
}

func (r *gitRepo) updateVersion(ctx context.Context, v string) error {
	cmd := commandContext(ctx, "git", "checkout", v)
	cmd.SetDir(r.LocalPath())
	if out, err := cmd.CombinedOutput(); err != nil {
		return newVcsLocalErrorOr(err, cmd.Args(), string(out),
			"unable to update checked out version")
	}

	return r.defendAgainstSubmodules(ctx)
}

// defendAgainstSubmodules tries to keep repo state sane in the event of
// submodules. Or nested submodules. What a great idea, submodules.
func (r *gitRepo) defendAgainstSubmodules(ctx context.Context) error {
	if err := r.ensureProxy(); err != nil {
		return err
	}
	// First, update them to whatever they should be, if there should happen to be any.
	{
		cmd := commandContext(
			ctx,
			"git",
			"submodule",
			"update",
			"--init",
			"--recursive",
		)
		cmd.SetDir(r.LocalPath())
		// Ensure no prompting for PWs
		cmd.SetEnv(append([]string{"GIT_ASKPASS=", "GIT_TERMINAL_PROMPT=0"}, os.Environ()...))
		if out, err := cmd.CombinedOutput(); err != nil {
			return newVcsLocalErrorOr(err, cmd.Args(), string(out),
				"unexpected error while defensively updating submodules")
		}
	}

	// Now, do a special extra-aggressive clean in case changing versions caused
	// one or more submodules to go away.
	{
		cmd := commandContext(ctx, "git", "clean", "-x", "-d", "-f", "-f")
		cmd.SetDir(r.LocalPath())
		if out, err := cmd.CombinedOutput(); err != nil {
			return newVcsLocalErrorOr(err, cmd.Args(), string(out),
				"unexpected error while defensively cleaning up after possible derelict submodule directories")
		}
	}

	// Then, repeat just in case there are any nested submodules that went away.
	{
		cmd := commandContext(
			ctx,
			"git",
			"submodule",
			"foreach",
			"--recursive",
			"git",
			"clean", "-x", "-d", "-f", "-f",
		)
		cmd.SetDir(r.LocalPath())
		if out, err := cmd.CombinedOutput(); err != nil {
			return newVcsLocalErrorOr(err, cmd.Args(), string(out),
				"unexpected error while defensively cleaning up after possible derelict nested submodule directories")
		}
	}

	return nil
}

func (r *gitRepo) ensureClean(ctx context.Context) error {
	cmd := commandContext(
		ctx,
		"git",
		"status",
		"--porcelain",
	)
	cmd.SetDir(r.LocalPath())

	out, err := cmd.CombinedOutput()
	if err != nil {
		// An error on simple git status indicates some aggressive repository
		// corruption, outside of the purview that we can deal with here.
		return err
	}

	if len(bytes.TrimSpace(out)) == 0 {
		// No output from status indicates a clean tree, without any modified or
		// untracked files - we're in good shape.
		return nil
	}

	// We could be more parsimonious about this, but it's probably not worth it
	// - it's a rare case to have to do any cleanup anyway, so when we do, we
	// might as well just throw the kitchen sink at it.
	cmd = commandContext(
		ctx,
		"git",
		"reset",
		"--hard",
	)
	cmd.SetDir(r.LocalPath())
	_, err = cmd.CombinedOutput()
	if err != nil {
		return err
	}

	// We also need to git clean -df; just reuse defendAgainstSubmodules here,
	// even though it's a bit layer-breaky.
	err = r.defendAgainstSubmodules(ctx)
	if err != nil {
		return err
	}

	// Check status one last time. If it's still not clean, give up.
	cmd = commandContext(
		ctx,
		"git",
		"status",
		"--porcelain",
	)
	cmd.SetDir(r.LocalPath())

	out, err = cmd.CombinedOutput()
	if err != nil {
		return err
	}

	if len(bytes.TrimSpace(out)) != 0 {
		return errors.Errorf("failed to clean up git repository at %s - dirty? corrupted? status output: \n%s", r.LocalPath(), string(out))
	}

	return nil
}

type bzrRepo struct {
	*vcs.BzrRepo
}

func (r *bzrRepo) get(ctx context.Context) error {
	basePath := filepath.Dir(filepath.FromSlash(r.LocalPath()))
	if _, err := os.Stat(basePath); os.IsNotExist(err) {
		err = os.MkdirAll(basePath, 0755)
		if err != nil {
			return newVcsLocalErrorOr(err, nil, "", "unable to create directory")
		}
	}

	cmd := commandContext(ctx, "bzr", "branch", r.Remote(), r.LocalPath())
	if out, err := cmd.CombinedOutput(); err != nil {
		return newVcsRemoteErrorOr(err, cmd.Args(), string(out),
			"unable to get repository")
	}

	return nil
}

func (r *bzrRepo) fetch(ctx context.Context) error {
	cmd := commandContext(ctx, "bzr", "pull")
	cmd.SetDir(r.LocalPath())
	if out, err := cmd.CombinedOutput(); err != nil {
		return newVcsRemoteErrorOr(err, cmd.Args(), string(out),
			"unable to update repository")
	}
	return nil
}

func (r *bzrRepo) updateVersion(ctx context.Context, version string) error {
	cmd := commandContext(ctx, "bzr", "update", "-r", version)
	cmd.SetDir(r.LocalPath())
	if out, err := cmd.CombinedOutput(); err != nil {
		return newVcsLocalErrorOr(err, cmd.Args(), string(out),
			"unable to update checked out version")
	}
	return nil
}

type hgRepo struct {
	*vcs.HgRepo
}

func (r *hgRepo) get(ctx context.Context) error {
	cmd := commandContext(ctx, "hg", "clone", r.Remote(), r.LocalPath())
	if out, err := cmd.CombinedOutput(); err != nil {
		return newVcsRemoteErrorOr(err, cmd.Args(), string(out),
			"unable to get repository")
	}

	return nil
}

func (r *hgRepo) fetch(ctx context.Context) error {
	cmd := commandContext(ctx, "hg", "pull")
	cmd.SetDir(r.LocalPath())
	if out, err := cmd.CombinedOutput(); err != nil {
		return newVcsRemoteErrorOr(err, cmd.Args(), string(out),
			"unable to fetch latest changes")
	}
	return nil
}

func (r *hgRepo) updateVersion(ctx context.Context, version string) error {
	cmd := commandContext(ctx, "hg", "update", version)
	cmd.SetDir(r.LocalPath())
	if out, err := cmd.CombinedOutput(); err != nil {
		return newVcsRemoteErrorOr(err, cmd.Args(), string(out),
			"unable to update checked out version")
	}

	return nil
}

type svnRepo struct {
	*vcs.SvnRepo
}

func (r *svnRepo) get(ctx context.Context) error {
	remote := r.Remote()
	if strings.HasPrefix(remote, "/") {
		remote = "file://" + remote
	} else if runtime.GOOS == "windows" && filepath.VolumeName(remote) != "" {
		remote = "file:///" + remote
	}

	cmd := commandContext(ctx, "svn", "checkout", remote, r.LocalPath())
	if out, err := cmd.CombinedOutput(); err != nil {
		return newVcsRemoteErrorOr(err, cmd.Args(), string(out),
			"unable to get repository")
	}

	return nil
}

func (r *svnRepo) fetch(ctx context.Context) error {
	cmd := commandContext(ctx, "svn", "update")
	cmd.SetDir(r.LocalPath())
	if out, err := cmd.CombinedOutput(); err != nil {
		return newVcsRemoteErrorOr(err, cmd.Args(), string(out),
			"unable to update repository")
	}

	return nil
}

func (r *svnRepo) updateVersion(ctx context.Context, version string) error {
	cmd := commandContext(ctx, "svn", "update", "-r", version)
	cmd.SetDir(r.LocalPath())
	if out, err := cmd.CombinedOutput(); err != nil {
		return newVcsRemoteErrorOr(err, cmd.Args(), string(out),
			"unable to update checked out version")
	}

	return nil
}

func (r *svnRepo) CommitInfo(id string) (*vcs.CommitInfo, error) {
	ctx := context.TODO()
	// There are cases where Svn log doesn't return anything for HEAD or BASE.
	// svn info does provide details for these but does not have elements like
	// the commit message.
	if id == "HEAD" || id == "BASE" {
		type commit struct {
			Revision string `xml:"revision,attr"`
		}

		type info struct {
			Commit commit `xml:"entry>commit"`
		}

		cmd := commandContext(ctx, "svn", "info", "-r", id, "--xml")
		cmd.SetDir(r.LocalPath())
		out, err := cmd.CombinedOutput()
		if err != nil {
			return nil, newVcsLocalErrorOr(err, cmd.Args(), string(out),
				"unable to retrieve commit information")
		}

		infos := new(info)
		if err := xml.Unmarshal(out, &infos); err != nil {
			return nil, newVcsLocalErrorOr(err, cmd.Args(), string(out),
				"unable to retrieve commit information")
		}

		id = infos.Commit.Revision
		if id == "" {
			return nil, vcs.ErrRevisionUnavailable
		}
	}

	cmd := commandContext(ctx, "svn", "log", "-r", id, "--xml")
	cmd.SetDir(r.LocalPath())
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, newVcsRemoteErrorOr(err, cmd.Args(), string(out),
			"unable to retrieve commit information")
	}

	type logentry struct {
		Author string `xml:"author"`
		Date   string `xml:"date"`
		Msg    string `xml:"msg"`
	}

	type log struct {
		XMLName xml.Name   `xml:"log"`
		Logs    []logentry `xml:"logentry"`
	}

	logs := new(log)
	if err := xml.Unmarshal(out, &logs); err != nil {
		return nil, newVcsLocalErrorOr(err, cmd.Args(), string(out),
			"unable to retrieve commit information")
	}

	if len(logs.Logs) == 0 {
		return nil, vcs.ErrRevisionUnavailable
	}

	ci := &vcs.CommitInfo{
		Commit:  id,
		Author:  logs.Logs[0].Author,
		Message: logs.Logs[0].Msg,
	}

	if len(logs.Logs[0].Date) > 0 {
		ci.Date, err = time.Parse(time.RFC3339Nano, logs.Logs[0].Date)
		if err != nil {
			return nil, newVcsLocalErrorOr(err, cmd.Args(), string(out),
				"unable to retrieve commit information")
		}
	}

	return ci, nil
}
