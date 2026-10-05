package command

import (
	"bytes"
	"context"
	"fmt"
	"golang.org/x/sync/semaphore"
	"os/exec"
	"runtime"
	"strings"
	"syscall"
	"time"
)

const (
	waitDelay      = 10 * time.Second
	maxStderrInErr = 1024
)

// Runner запускает внешние программы (libreoffice, ffmpeg, convert).
// Семафор ограничивает количество одновременно запущенных процессов, timeout - время работы одного процесса.
type Runner struct {
	sem     *semaphore.Weighted
	timeout time.Duration
}

func NewRunner(maxProcesses int, timeout time.Duration) *Runner {
	if maxProcesses <= 0 {
		maxProcesses = runtime.NumCPU()
	}
	return &Runner{
		sem:     semaphore.NewWeighted(int64(maxProcesses)),
		timeout: timeout,
	}
}

func (r *Runner) Run(ctx context.Context, name string, args ...string) error {
	if err := r.sem.Acquire(ctx, 1); err != nil {
		return fmt.Errorf("wait for free process slot: [%w]", err)
	}
	defer r.sem.Release(1)

	if r.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, r.timeout)
		defer cancel()
	}

	var stderr bytes.Buffer

	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stderr = &stderr
	// отдельная группа процессов: libreoffice запускает дочерний soffice.bin,
	// и при отмене нужно убить всю группу, а не только родителя
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = waitDelay

	if err := cmd.Run(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			err = fmt.Errorf("%w (%w)", err, ctxErr)
		}
		return fmt.Errorf("error %s command: [%w] %s", name, err, stderrTail(stderr.String()))
	}
	return nil
}

func stderrTail(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > maxStderrInErr {
		s = "..." + s[len(s)-maxStderrInErr:]
	}
	return s
}
