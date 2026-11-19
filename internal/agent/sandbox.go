package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"syscall"
	"time"
)

type cappedOutput struct {
	f       *os.File
	preview bytes.Buffer
	total   int
}

func (w *cappedOutput) Write(p []byte) (int, error) {
	n := len(p)
	keep := min(n, max((1<<20)-w.total, 0))
	if keep > 0 {
		if _, e := w.f.Write(p[:keep]); e != nil {
			return 0, e
		}
		w.total += keep
	}
	take := min(n, max(8192-w.preview.Len(), 0))
	w.preview.Write(p[:take])
	return n, nil
}
func (s *Service) runShell(parent context.Context, key, command string) (string, error) {
	ctx, cancel := context.WithTimeout(parent, time.Duration(s.cfg.Limits.ToolSeconds)*time.Second)
	defer cancel()
	scratch, e := os.MkdirTemp(filepath.Join(s.root, "workspace/scratch"), "run-")
	if e != nil {
		return "", e
	}
	workspace := filepath.Join(s.root, "workspace")
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		bin, e := exec.LookPath("sandbox-exec")
		if e != nil {
			return "", errors.New("bash denied: sandbox-exec unavailable")
		}
		profile := `(version 1)(deny default)(allow process-exec)(allow process-fork)(allow signal (target same-sandbox))(allow sysctl-read)` +
			`(allow file-read-metadata)(allow file-read* (literal "/") (subpath "/System") (subpath "/usr") (subpath "/bin") (subpath "/sbin") (subpath "/Library/Apple") (subpath "/private/var/db/dyld") (literal "/dev/null") (subpath ` + strconv.Quote(workspace) + `))` +
			`(allow file-write* (subpath ` + strconv.Quote(scratch) + `) (literal "/dev/null"))`
		cmd = exec.CommandContext(ctx, bin, "-p", profile, "/bin/sh", "-c", command)
	case "linux":
		bin, e := exec.LookPath("bwrap")
		if e != nil {
			return "", errors.New("bash denied: install bubblewrap or disable shell; no unsafe fallback")
		}
		args := []string{"--die-with-parent", "--unshare-all", "--new-session", "--proc", "/proc", "--dev", "/dev", "--tmpfs", "/tmp"}
		for _, dir := range []string{"/usr", "/bin", "/sbin", "/lib", "/lib64"} {
			if fileExists(dir) {
				args = append(args, "--ro-bind", dir, dir)
			}
		}
		args = append(args, "--ro-bind", workspace, workspace, "--bind", scratch, scratch, "--chdir", scratch, "/bin/sh", "-c", command)
		cmd = exec.CommandContext(ctx, bin, args...)
	default:
		return "", errors.New("bash isolation is supported only on macOS/Linux")
	}
	cmd.Dir = scratch
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + scratch, "TMPDIR=" + scratch, "LANG=en_US.UTF-8"}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		e := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(e, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return e
	}
	cmd.WaitDelay = 2 * time.Second
	ref := stableID(key) + "-" + randomID() + ".txt"
	f, e := os.OpenFile(filepath.Join(s.root, "data/outputs", ref), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if e != nil {
		return "", e
	}
	output := &cappedOutput{f: f}
	cmd.Stdout = output
	cmd.Stderr = output
	e = cmd.Run()
	// Kill background descendants even when the foreground shell exited normally.
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	syncErr := f.Sync()
	closeErr := f.Close()
	e = errors.Join(e, syncErr, closeErr)
	text := s.bounded(output.preview.String()) + "\nOutput file: outputs/" + ref + " (retained up to 1 MiB)"
	if ctx.Err() != nil {
		return text, ctx.Err()
	}
	if e != nil {
		return text, fmt.Errorf("sandbox command failed: %w; %s", e, text)
	}
	return text, nil
}
