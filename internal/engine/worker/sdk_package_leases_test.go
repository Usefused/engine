package worker

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
)

type sdkPackageLeaseStoreStub struct {
	mu       sync.Mutex
	apps     []models.SDKPackageLeaseRenewal
	cursors  []uuid.UUID
	started  chan struct{}
	startOne sync.Once
}

func (s *sdkPackageLeaseStoreStub) ListSDKPackageLeaseRenewals(_ context.Context, after uuid.UUID, limit int) ([]models.SDKPackageLeaseRenewal, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cursors = append(s.cursors, after)
	if s.started != nil {
		s.startOne.Do(func() { close(s.started) })
	}
	start := 0
	for start < len(s.apps) && s.apps[start].AppID.String() <= after.String() {
		start++
	}
	end := min(start+limit, len(s.apps))
	return append([]models.SDKPackageLeaseRenewal(nil), s.apps[start:end]...), nil
}

type sdkPackageLeaseClientStub struct {
	mu         sync.Mutex
	batches    [][]models.SDKPackageLeaseRenewal
	failOnCall int
}

func (c *sdkPackageLeaseClientStub) RenewSDKPackageLeases(_ context.Context, apps []models.SDKPackageLeaseRenewal) (int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.batches = append(c.batches, append([]models.SDKPackageLeaseRenewal(nil), apps...))
	if c.failOnCall == len(c.batches) {
		return 0, errors.New("registry unavailable")
	}
	return int64(len(apps)), nil
}

func TestSDKPackageLeaseWorkerRenewsBoundedPages(t *testing.T) {
	apps := orderedPackageLeaseApps(1201)
	store := &sdkPackageLeaseStoreStub{apps: apps}
	client := &sdkPackageLeaseClientStub{}
	worker := NewSDKPackageLeaseWorker(store, client, SDKPackageLeaseOptions{BatchSize: 500})

	requested, renewed, batches, err := worker.renewPages(context.Background())
	if err != nil {
		t.Fatalf("renewPages() error = %v", err)
	}
	if requested != len(apps) || renewed != int64(len(apps)) || batches != 3 {
		t.Fatalf("renewPages() = requested %d renewed %d batches %d", requested, renewed, batches)
	}
	if worker.resumeAt != uuid.Nil {
		t.Fatalf("completed scan retained cursor %s", worker.resumeAt)
	}
	for index, batch := range client.batches {
		if len(batch) > models.SDKPackageLeaseBatchLimit {
			t.Fatalf("batch %d has %d apps", index, len(batch))
		}
	}
}

func TestSDKPackageLeaseWorkerResumesAfterFailedBatch(t *testing.T) {
	apps := orderedPackageLeaseApps(5)
	store := &sdkPackageLeaseStoreStub{apps: apps}
	client := &sdkPackageLeaseClientStub{failOnCall: 2}
	worker := NewSDKPackageLeaseWorker(store, client, SDKPackageLeaseOptions{BatchSize: 2})

	worker.renewPages(context.Background())
	if worker.resumeAt != apps[1].AppID {
		t.Fatalf("failure cursor = %s, want %s", worker.resumeAt, apps[1].AppID)
	}
	client.failOnCall = 0
	requested, renewed, batches, err := worker.renewPages(context.Background())
	if err != nil {
		t.Fatalf("resumed renewPages() error = %v", err)
	}
	if requested != 3 || renewed != 3 || batches != 2 {
		t.Fatalf("resumed renewal = requested %d renewed %d batches %d", requested, renewed, batches)
	}
	if got := client.batches[2][0].AppID; got != apps[2].AppID {
		t.Fatalf("resumed at app %s, want %s", got, apps[2].AppID)
	}
}

func TestSDKPackageLeaseWorkerStartsWithImmediateRenewal(t *testing.T) {
	started := make(chan struct{})
	store := &sdkPackageLeaseStoreStub{started: started}
	client := &sdkPackageLeaseClientStub{}
	worker := NewSDKPackageLeaseWorker(store, client, SDKPackageLeaseOptions{Interval: time.Hour})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	worker.Start(ctx)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("startup renewal did not begin immediately")
	}
	stopCtx, stopCancel := context.WithTimeout(context.Background(), time.Second)
	defer stopCancel()
	worker.Stop(stopCtx)
}

func TestSDKPackageLeaseWorkerJitterIsBounded(t *testing.T) {
	worker := NewSDKPackageLeaseWorker(nil, nil, SDKPackageLeaseOptions{
		Interval: time.Hour, MaxJitter: 10 * time.Minute,
	})
	for range 100 {
		delay := worker.nextInterval()
		if delay < 50*time.Minute || delay > 70*time.Minute {
			t.Fatalf("jittered delay %s is outside bounds", delay)
		}
	}
}

func orderedPackageLeaseApps(count int) []models.SDKPackageLeaseRenewal {
	apps := make([]models.SDKPackageLeaseRenewal, count)
	familyID := uuid.New()
	for index := range apps {
		apps[index] = models.SDKPackageLeaseRenewal{
			AppID: uuid.MustParse(formatOrderedUUID(index + 1)), AppFamilyID: familyID,
		}
	}
	return apps
}

func formatOrderedUUID(value int) string {
	const hex = "0123456789abcdef"
	result := []byte("00000000-0000-0000-0000-000000000000")
	for position := len(result) - 1; value > 0; position-- {
		if result[position] == '-' {
			position--
		}
		result[position] = hex[value%16]
		value /= 16
	}
	return string(result)
}
