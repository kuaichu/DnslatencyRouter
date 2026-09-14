package mtr

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Worker serializes trace executions and records an attempt before launching
// NextTrace. Repeated jobs and process restarts cannot rerun the same selection.
type Worker struct {
	mu      sync.Mutex
	path    string
	results map[string]Result
	desired map[string]Request
	wake    chan struct{}
	ctx     context.Context
	cancel  context.CancelFunc
	done    chan struct{}
	run     func(context.Context, Request) Result
	onDone  func(Result)
}

func NewWorker(path string, run func(context.Context, Request) Result, onDone func(Result)) (*Worker, error) {
	w := &Worker{path: path, results: make(map[string]Result), desired: make(map[string]Request), wake: make(chan struct{}, 1), done: make(chan struct{}), run: run, onDone: onDone}
	if data, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(data, &w.results); err != nil {
			return nil, fmt.Errorf("read MTR cache: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	if w.results == nil {
		w.results = make(map[string]Result)
	}
	for id, result := range w.results {
		if result.Status == "running" {
			result.Status, result.Error, result.FinishedAt = "failed", "Previous MTR attempt was interrupted; IP unchanged, not retried", time.Now()
			w.results[id] = result
		}
	}
	w.ctx, w.cancel = context.WithCancel(context.Background())
	go w.loop()
	return w, nil
}

func (w *Worker) Close() { w.cancel(); <-w.done }

func (w *Worker) Sync(requests []Request) []Result {
	w.mu.Lock()
	w.desired = make(map[string]Request)
	var ready []Result
	for _, request := range requests {
		if request.Validate() != nil {
			continue
		}
		w.desired[request.ID] = request
		if result, ok := w.results[request.ID]; ok && result.Request == request && result.Status != "running" {
			ready = append(ready, result)
		}
	}
	w.mu.Unlock()
	select {
	case w.wake <- struct{}{}:
	default:
	}
	return ready
}

func (w *Worker) saveLocked() error {
	if err := os.MkdirAll(filepath.Dir(w.path), 0700); err != nil {
		return err
	}
	data, err := json.Marshal(w.results)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(w.path), ".mtr-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), w.path)
}

func (w *Worker) next() (Request, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	ids := make([]string, 0, len(w.desired))
	for id := range w.desired {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if _, done := w.results[id]; done {
			continue
		}
		request := w.desired[id]
		w.results[id] = Result{Request: request, Status: "running", StartedAt: time.Now()}
		if err := w.saveLocked(); err != nil {
			w.results[id] = Result{Request: request, Status: "failed", Error: "Cannot persist MTR attempt: " + err.Error(), FinishedAt: time.Now()}
			continue
		}
		return request, true
	}
	return Request{}, false
}

func (w *Worker) loop() {
	defer close(w.done)
	for {
		select {
		case <-w.ctx.Done():
			return
		case <-w.wake:
		}
		for {
			if w.ctx.Err() != nil {
				return
			}
			request, ok := w.next()
			if !ok {
				break
			}
			result := w.run(w.ctx, request)
			result.Request = request
			w.mu.Lock()
			w.results[request.ID] = result
			// Only the current selections need retry-safe local history.
			if len(w.results) > 256 {
				for id := range w.results {
					if _, keep := w.desired[id]; !keep {
						delete(w.results, id)
					}
				}
			}
			_ = w.saveLocked()
			w.mu.Unlock()
			if w.onDone != nil {
				w.onDone(result)
			}
		}
	}
}
