package bot

import (
	"log"
	"runtime/debug"
	"strings"
	"sync"
)

// keyedSerialExecutor runs tasks sequentially per key, while allowing tasks of
// different keys to run concurrently.
//
// This is used to ensure messages within the same Feishu topic/thread are
// processed in order, so that context remains consistent.
type keyedSerialExecutor struct {
	mu     sync.Mutex
	queues map[string]*taskQueue
}

type taskQueue struct {
	mu      sync.Mutex
	tasks   []func()
	running bool
	gen     uint64
}

func newKeyedSerialExecutor() *keyedSerialExecutor {
	return &keyedSerialExecutor{
		queues: make(map[string]*taskQueue),
	}
}

func (e *keyedSerialExecutor) Enqueue(key string, task func()) {
	if task == nil {
		return
	}
	key = strings.TrimSpace(key)
	if key == "" {
		go safeRunTask(task)
		return
	}

	e.mu.Lock()
	q := e.queues[key]
	if q == nil {
		q = &taskQueue{}
		e.queues[key] = q
	}
	e.mu.Unlock()

	q.mu.Lock()
	q.tasks = append(q.tasks, task)
	if q.running {
		q.mu.Unlock()
		return
	}
	q.running = true
	q.gen++
	gen := q.gen
	q.mu.Unlock()

	go e.run(key, q, gen)
}

func (e *keyedSerialExecutor) run(key string, q *taskQueue, gen uint64) {
	for {
		q.mu.Lock()
		if len(q.tasks) == 0 {
			q.running = false
			q.tasks = nil
			q.mu.Unlock()

			// Cleanup to avoid leaking per-key goroutines/queues forever. We must
			// be careful: another goroutine may have enqueued a new task and
			// started a new worker between unlocking q.mu and acquiring e.mu.
			e.mu.Lock()
			q.mu.Lock()
			shouldDelete := !q.running && len(q.tasks) == 0 && q.gen == gen
			q.mu.Unlock()
			if shouldDelete {
				if e.queues[key] == q {
					delete(e.queues, key)
				}
			}
			e.mu.Unlock()
			return
		}

		task := q.tasks[0]
		q.tasks = q.tasks[1:]
		q.mu.Unlock()

		safeRunTask(task)
	}
}

func safeRunTask(task func()) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[ERROR] queued task panicked: %v\n%s", r, string(debug.Stack()))
		}
	}()
	task()
}
