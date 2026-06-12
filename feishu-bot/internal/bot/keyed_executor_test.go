package bot

import (
	"testing"
	"time"
)

func TestKeyedSerialExecutor_SerialPerKey(t *testing.T) {
	exec := newKeyedSerialExecutor()

	started1 := make(chan struct{})
	unblock1 := make(chan struct{})
	started2 := make(chan struct{})
	finished2 := make(chan struct{})

	exec.Enqueue("k", func() {
		close(started1)
		<-unblock1
	})
	exec.Enqueue("k", func() {
		close(started2)
		close(finished2)
	})

	select {
	case <-started1:
	case <-time.After(2 * time.Second):
		t.Fatalf("task1 did not start")
	}

	select {
	case <-started2:
		t.Fatalf("task2 started before task1 finished")
	case <-time.After(150 * time.Millisecond):
		// ok
	}

	close(unblock1)

	select {
	case <-finished2:
	case <-time.After(2 * time.Second):
		t.Fatalf("task2 did not finish")
	}
}

func TestKeyedSerialExecutor_ParallelAcrossKeys(t *testing.T) {
	exec := newKeyedSerialExecutor()

	startedA := make(chan struct{})
	unblockA := make(chan struct{})
	startedB := make(chan struct{})
	finishedB := make(chan struct{})

	exec.Enqueue("a", func() {
		close(startedA)
		<-unblockA
	})

	select {
	case <-startedA:
	case <-time.After(2 * time.Second):
		t.Fatalf("taskA did not start")
	}

	exec.Enqueue("b", func() {
		close(startedB)
		close(finishedB)
	})

	select {
	case <-startedB:
		// ok: key "b" can run while key "a" is blocked
	case <-time.After(2 * time.Second):
		t.Fatalf("taskB did not start while taskA is blocked")
	}

	close(unblockA)

	select {
	case <-finishedB:
	case <-time.After(2 * time.Second):
		t.Fatalf("taskB did not finish")
	}
}

func TestKeyedSerialExecutor_PanicDoesNotStopQueue(t *testing.T) {
	exec := newKeyedSerialExecutor()

	done := make(chan struct{})
	exec.Enqueue("k", func() {
		panic("boom")
	})
	exec.Enqueue("k", func() {
		close(done)
	})

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("queue stopped after panic")
	}
}
