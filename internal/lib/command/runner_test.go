package command

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRunnerTimeoutKillsProcessGroup(t *testing.T) {
	r := NewRunner(1, 300*time.Millisecond)

	start := time.Now()
	// дочерний sleep держит stderr открытым: если убить только sh, Run ждал бы WaitDelay (10s)
	err := r.Run(context.Background(), "sh", "-c", "sleep 30 & wait")
	if err == nil {
		t.Fatal("expected error on timeout")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("process group was not killed in time: %s", elapsed)
	}
}

func TestRunnerCancel(t *testing.T) {
	r := NewRunner(1, time.Minute)

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(200*time.Millisecond, cancel)

	start := time.Now()
	if err := r.Run(ctx, "sleep", "30"); err == nil {
		t.Fatal("expected error on cancel")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("process was not killed on cancel: %s", elapsed)
	}
}

func TestRunnerSemaphoreLimitsConcurrency(t *testing.T) {
	r := NewRunner(1, time.Minute)

	start := time.Now()
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := r.Run(context.Background(), "sleep", "0.3"); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()

	if elapsed := time.Since(start); elapsed < 600*time.Millisecond {
		t.Fatalf("processes ran concurrently despite limit 1: %s", elapsed)
	}
}

func TestRunnerErrorContainsStderr(t *testing.T) {
	r := NewRunner(1, time.Minute)

	err := r.Run(context.Background(), "sh", "-c", "echo broken input >&2; exit 1")
	if err == nil {
		t.Fatal("expected error")
	}
	if got := err.Error(); !strings.Contains(got, "broken input") {
		t.Fatalf("error does not contain stderr: %s", got)
	}
}
