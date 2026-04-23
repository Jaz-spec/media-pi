// Package execsh wraps os/exec with project conventions: working directory
// is the repo root, output is streamed to a log file, exit codes are
// returned explicitly.
package execsh

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// Execer is the interface the orchestrator and worker depend on. Having an
// interface here lets tests swap in fakes rather than spawning real processes.
type Execer interface {
	RunScript(ctx context.Context, script string, args ...string) (RunResult, error)
	StreamScript(ctx context.Context, logPath, script string, args ...string) (int, error)
}

// RunResult captures stdout, stderr, and exit code of a finished process.
type RunResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// Real is the production Execer. RepoRoot is the working directory every
// script is run from; it must be an absolute path.
type Real struct {
	RepoRoot string
}

// New returns a Real Execer rooted at repoRoot. If repoRoot is empty, the
// current working directory is used.
func New(repoRoot string) (*Real, error) {
	if repoRoot == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return nil, fmt.Errorf("getwd: %w", err)
		}
		repoRoot = cwd
	}
	abs, err := filepath.Abs(repoRoot)
	if err != nil {
		return nil, fmt.Errorf("abs repo root: %w", err)
	}
	return &Real{RepoRoot: abs}, nil
}

// RunScript runs the given script to completion and returns its buffered
// stdout/stderr. Use for short scripts whose output is small enough to hold
// in memory (e.g. record.sh status). For long-running scripts with large
// logs, use StreamScript.
func (r *Real) RunScript(ctx context.Context, script string, args ...string) (RunResult, error) {
	cmd := exec.CommandContext(ctx, script, args...)
	cmd.Dir = r.RepoRoot

	var stdout, stderr outBuf
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	res := RunResult{
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		ExitCode: cmd.ProcessState.ExitCode(),
	}
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			// Non-zero exit is not a fatal error — the caller decides what
			// each exit code means. Return the result plus the error so the
			// caller can choose.
			return res, nil
		}
		return res, fmt.Errorf("run %s: %w", script, err)
	}
	return res, nil
}

// StreamScript runs the given script and tees its combined stdout+stderr to
// the log file at logPath (appended). Blocks until the script exits; returns
// the final exit code. The log file is created if it doesn't exist.
func (r *Real) StreamScript(ctx context.Context, logPath, script string, args ...string) (int, error) {
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return -1, fmt.Errorf("mkdir log dir: %w", err)
	}
	logFile, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return -1, fmt.Errorf("open log %s: %w", logPath, err)
	}
	defer logFile.Close()

	cmd := exec.CommandContext(ctx, script, args...)
	cmd.Dir = r.RepoRoot
	cmd.Stdout = logFile
	cmd.Stderr = logFile

	err = cmd.Run()
	exit := cmd.ProcessState.ExitCode()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return exit, nil
		}
		return exit, fmt.Errorf("stream %s: %w", script, err)
	}
	return exit, nil
}

// outBuf is a tiny string builder satisfying io.Writer. We avoid importing
// bytes.Buffer just to keep imports minimal.
type outBuf struct {
	b []byte
}

func (o *outBuf) Write(p []byte) (int, error) {
	o.b = append(o.b, p...)
	return len(p), nil
}

func (o *outBuf) String() string { return string(o.b) }
