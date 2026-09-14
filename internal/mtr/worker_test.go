package mtr

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestWorkerDeduplicatesPersistsAndAcceptsNewSelection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.json")
	var count atomic.Int32
	finished := make(chan Result, 8)
	run := func(ctx context.Context, r Request) Result {
		count.Add(1)
		return Result{Request: r, Status: "completed", Output: "test", FinishedAt: time.Now()}
	}
	w, err := NewWorker(path, run, func(r Result) { finished <- r })
	if err != nil {
		t.Fatal(err)
	}
	r := Request{ID: "first", AgentID: "a", IP: "1.1.1.1", Port: 443}
	w.Sync([]Request{r, r})
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("worker did not run")
	}
	if got := w.Sync([]Request{r}); len(got) != 1 {
		t.Fatal("cached result unavailable")
	}
	w.Close()
	w, err = NewWorker(path, run, func(r Result) { finished <- r })
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if got := w.Sync([]Request{r}); len(got) != 1 {
		t.Fatal("restart lost cached result")
	}
	r.ID = "new-selection"
	w.Sync([]Request{r})
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("new selection did not run")
	}
	if count.Load() != 2 {
		t.Fatalf("execution count=%d want 2", count.Load())
	}
}

func TestInterruptedAttemptIsNotRerun(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.json")
	r := Request{ID: "first", AgentID: "a", IP: "1.1.1.1", Port: 443}
	data, _ := json.Marshal(map[string]Result{r.ID: {Request: r, Status: "running"}})
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	var count atomic.Int32
	w, err := NewWorker(path, func(context.Context, Request) Result { count.Add(1); return Result{} }, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := w.Sync([]Request{r})
	w.Close()
	if len(got) != 1 || got[0].Status != "failed" || count.Load() != 0 {
		t.Fatal("interrupted trace was rerun")
	}
}

func TestBackgroundWorkerNeverBlocksSyncAndDiscardsQueuedOldTargets(t *testing.T) {
	started := make(chan Request, 3)
	release := make(chan struct{})
	w, err := NewWorker(filepath.Join(t.TempDir(), "cache.json"), func(ctx context.Context, r Request) Result {
		started <- r
		select {
		case <-release:
		case <-ctx.Done():
		}
		return Result{Request: r, Status: "completed", FinishedAt: time.Now()}
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	r := Request{ID: "a", AgentID: "a", IP: "1.1.1.1", Port: 443}
	old := r
	old.ID = "b"
	newTarget := r
	newTarget.ID = "c"
	w.Sync([]Request{r, old})
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("not started")
	}
	w.Sync([]Request{newTarget})
	close(release)
	select {
	case got := <-started:
		if got.ID != "c" {
			t.Fatal("queued obsolete target executed")
		}
	case <-time.After(time.Second):
		t.Fatal("new target not started")
	}
}
