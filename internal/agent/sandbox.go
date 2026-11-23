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

// sandboxed builds a command that runs without network, can read only system
// directories and readable, and can write only writable, its working
// directory. There is no unsafe fallback: without a sandbox it refuses.
func sandboxed(ctx context.Context, argv, readable []string, writable string) (*exec.Cmd, error) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		bin, e := exec.LookPath("sandbox-exec")
		if e != nil {
			return nil, errors.New("sandbox-exec unavailable")
		}
		reads := ""
		for _, dir := range readable {
			reads += ` (subpath ` + strconv.Quote(dir) + `)`
		}
		profile := `(version 1)(deny default)(allow process-exec)(allow process-fork)(allow signal (target same-sandbox))(allow sysctl-read)` +
			`(allow file-read-metadata)(allow file-read* (literal "/") (subpath "/System") (subpath "/usr") (subpath "/bin") (subpath "/sbin") (subpath "/Library/Apple") (subpath "/private/var/db/dyld") (literal "/dev/null")` + reads + `)` +
			`(allow file-write* (subpath ` + strconv.Quote(writable) + `) (literal "/dev/null"))`
		cmd = exec.CommandContext(ctx, bin, append([]string{"-p", profile}, argv...)...)
	case "linux":
		bin, e := exec.LookPath("bwrap")
		if e != nil {
			return nil, errors.New("install bubblewrap; there is no unsafe fallback")
		}
		args := []string{"--die-with-parent", "--unshare-all", "--new-session", "--proc", "/proc", "--dev", "/dev", "--tmpfs", "/tmp"}
		for _, dir := range []string{"/usr", "/bin", "/sbin", "/lib", "/lib64"} {
			if fileExists(dir) {
				args = append(args, "--ro-bind", dir, dir)
			}
		}
		for _, dir := range readable {
			args = append(args, "--ro-bind", dir, dir)
		}
		args = append(args, "--bind", writable, writable, "--chdir", writable)
		cmd = exec.CommandContext(ctx, bin, append(args, argv...)...)
	default:
		return nil, errors.New("isolation is supported only on macOS/Linux")
	}
	cmd.Dir = writable
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + writable, "TMPDIR=" + writable, "LANG=en_US.UTF-8"}
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
	return cmd, nil
}

func (s *Service) runShell(parent context.Context, key, command string) (string, error) {
	ctx, cancel := context.WithTimeout(parent, time.Duration(s.cfg.Limits.ToolSeconds)*time.Second)
	defer cancel()
	scratch, e := os.MkdirTemp(filepath.Join(s.root, "workspace/scratch"), "run-")
	if e != nil {
		return "", e
	}
	cmd, e := sandboxed(ctx, []string{"/bin/sh", "-c", command}, []string{filepath.Join(s.root, "workspace")}, scratch)
	if e != nil {
		return "", fmt.Errorf("bash denied: %w", e)
	}
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
