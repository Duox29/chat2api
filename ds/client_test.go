package ds

import (
	"context"
	"strings"
	"testing"
	"time"
)

// A runLogin that arrives while another refresh is in flight must skip the
// browser launch once the in-flight refresh completes (generation check),
// instead of launching N sequential browsers for N concurrent 401s.
func TestRunLoginDedupesConcurrentRefresh(t *testing.T) {
	c := New(t.TempDir())
	c.loginMu.Lock()
	done := make(chan error, 1)
	go func() { done <- c.runLogin(context.Background()) }()
	// The goroutine captures loginSeq first, then blocks on loginMu (held
	// here). Simulate the in-flight refresh completing while it waits.
	time.Sleep(time.Second)
	c.mu.Lock()
	c.loginSeq++
	c.mu.Unlock()
	c.loginMu.Unlock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("waiter must skip browser after refresh, got: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("runLogin did not return")
	}
}

// A failed login must not bump the generation (so the next waiter retries)
// and must surface the browser error.
func TestRunLoginFailureDoesNotBumpSeq(t *testing.T) {
	c := New(t.TempDir()) // no scripts/login.js here: browser step must fail
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := c.runLogin(ctx)
	if err == nil || !strings.Contains(err.Error(), "browser login failed") {
		t.Fatalf("expected browser login failure, got: %v", err)
	}
	c.mu.Lock()
	seq := c.loginSeq
	c.mu.Unlock()
	if seq != 0 {
		t.Fatalf("failed login bumped loginSeq to %d", seq)
	}
}
