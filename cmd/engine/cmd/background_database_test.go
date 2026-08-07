package cmd

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
)

func TestBackgroundDatabaseGateSerializesMaintenanceOperations(t *testing.T) {
	gate := newBackgroundDatabaseGate()
	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- gate.run(t.Context(), func() error {
			close(firstEntered)
			<-releaseFirst
			return nil
		})
	}()
	<-firstEntered

	secondEntered := make(chan struct{})
	secondDone := make(chan error, 1)
	go func() {
		secondDone <- gate.run(t.Context(), func() error {
			close(secondEntered)
			return nil
		})
	}()

	select {
	case <-secondEntered:
		t.Fatal("second maintenance operation overlapped the first")
	case <-time.After(25 * time.Millisecond):
	}
	close(releaseFirst)
	assertBackgroundOperationDone(t, firstDone)
	select {
	case <-secondEntered:
	case <-time.After(time.Second):
		t.Fatal("second maintenance operation did not run after release")
	}
	assertBackgroundOperationDone(t, secondDone)
}

func TestBackgroundDatabaseGateHonorsCancellationWhileWaiting(t *testing.T) {
	gate := newBackgroundDatabaseGate()
	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	go func() {
		_ = gate.run(t.Context(), func() error {
			close(firstEntered)
			<-releaseFirst
			return nil
		})
	}()
	<-firstEntered

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	called := false
	err := gate.run(ctx, func() error {
		called = true
		return nil
	})
	close(releaseFirst)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("run error = %v, want context canceled", err)
	}
	if called {
		t.Fatal("canceled maintenance operation was called")
	}
}

func TestSerializedBackgroundStoreDoesNotGateMaintenanceWrites(t *testing.T) {
	gate := newBackgroundDatabaseGate()
	holderEntered := make(chan struct{})
	releaseHolder := make(chan struct{})
	holderDone := make(chan error, 1)
	go func() {
		holderDone <- gate.run(t.Context(), func() error {
			close(holderEntered)
			<-releaseHolder
			return nil
		})
	}()
	<-holderEntered

	writeCalled := make(chan struct{})
	usageStore := &backgroundUsageStoreFixture{writeCalled: writeCalled}
	backgroundStore := &serializedBackgroundStore{gate: gate, usageReports: usageStore}
	writeDone := make(chan error, 1)
	go func() {
		writeDone <- backgroundStore.MarkRuntimeUsageReportsFlushed(t.Context(), []uuid.UUID{uuid.New()}, time.Now())
	}()

	select {
	case <-writeCalled:
	case <-time.After(time.Second):
		t.Fatal("maintenance write waited for the scheduled probe gate")
	}
	assertBackgroundOperationDone(t, writeDone)
	close(releaseHolder)
	assertBackgroundOperationDone(t, holderDone)
}

type backgroundUsageStoreFixture struct {
	writeCalled chan struct{}
}

func (s *backgroundUsageStoreFixture) ListPendingRuntimeUsageReports(context.Context, int) ([]models.EngineUsageReport, error) {
	return nil, nil
}

func (s *backgroundUsageStoreFixture) MarkRuntimeUsageReportsFlushed(context.Context, []uuid.UUID, time.Time) error {
	close(s.writeCalled)
	return nil
}

func assertBackgroundOperationDone(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("maintenance operation failed: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("maintenance operation did not finish")
	}
}
