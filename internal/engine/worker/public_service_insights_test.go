package worker

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Usefused/engine/internal/shared/models"
)

type publicInsightStoreStub struct {
	candidates       []uuid.UUID
	projected        []uuid.UUID
	reports          []models.PublicServiceInsightReport
	results          []models.PublicServiceInsightReportResult
	deliveryFailures []uuid.UUID
}

func (s *publicInsightStoreStub) ListUnprojectedPublicInsightServiceIDs(context.Context, time.Time, int) ([]uuid.UUID, error) {
	return s.candidates, nil
}
func (s *publicInsightStoreStub) ProjectPublicServiceInsightReports(_ context.Context, serviceIDs []uuid.UUID, _ time.Time, _ int) (int64, error) {
	s.projected = append([]uuid.UUID(nil), serviceIDs...)
	return int64(len(serviceIDs)), nil
}
func (s *publicInsightStoreStub) ListPendingPublicServiceInsightReports(context.Context, int, time.Time) ([]models.PublicServiceInsightReport, error) {
	return s.reports, nil
}
func (s *publicInsightStoreStub) MarkPublicServiceInsightReportResults(_ context.Context, results []models.PublicServiceInsightReportResult, _ time.Time) error {
	s.results = append([]models.PublicServiceInsightReportResult(nil), results...)
	return nil
}
func (s *publicInsightStoreStub) MarkPublicServiceInsightReportDeliveryFailure(_ context.Context, reportIDs []uuid.UUID, _ string, _ time.Time) error {
	s.deliveryFailures = append([]uuid.UUID(nil), reportIDs...)
	return nil
}

type publicInsightClientStub struct {
	eligibility map[uuid.UUID]bool
	results     []models.PublicServiceInsightReportResult
	sendErr     error
	sent        []models.PublicServiceInsightReport
}

func (c *publicInsightClientStub) FetchPublicServiceInsightEligibility(context.Context, []uuid.UUID) (map[uuid.UUID]bool, error) {
	return c.eligibility, nil
}
func (c *publicInsightClientStub) SendPublicServiceInsightReports(_ context.Context, _, _ string, reports []models.PublicServiceInsightReport, _ time.Time) ([]models.PublicServiceInsightReportResult, error) {
	c.sent = append([]models.PublicServiceInsightReport(nil), reports...)
	return c.results, c.sendErr
}

func TestPublicInsightWorkerProjectsOnlyRegistryEligibleServices(t *testing.T) {
	publicService, privateService, reportID := uuid.New(), uuid.New(), uuid.New()
	store := &publicInsightStoreStub{
		candidates: []uuid.UUID{publicService, privateService},
		reports:    []models.PublicServiceInsightReport{{ReportID: reportID}},
	}
	client := &publicInsightClientStub{
		eligibility: map[uuid.UUID]bool{publicService: true, privateService: false},
		results:     []models.PublicServiceInsightReportResult{{ReportID: reportID, Accepted: true}},
	}
	worker := NewPublicInsightWorker(store, client, PublicInsightOptions{})

	worker.flush(context.Background())

	if len(store.projected) != 1 || store.projected[0] != publicService {
		t.Fatalf("projected services = %v, want only %s", store.projected, publicService)
	}
	if len(client.sent) != 1 || len(store.results) != 1 || !store.results[0].Accepted {
		t.Fatalf("delivery = sent %#v results %#v", client.sent, store.results)
	}
}

func TestPublicInsightWorkerRetainsReportsWhenRegistryUnavailable(t *testing.T) {
	reportID := uuid.New()
	store := &publicInsightStoreStub{reports: []models.PublicServiceInsightReport{{ReportID: reportID}}}
	client := &publicInsightClientStub{sendErr: errors.New("registry unavailable")}
	worker := NewPublicInsightWorker(store, client, PublicInsightOptions{})

	worker.flush(context.Background())

	if len(store.deliveryFailures) != 1 || store.deliveryFailures[0] != reportID {
		t.Fatalf("delivery failures = %v, want %s", store.deliveryFailures, reportID)
	}
	if len(store.results) != 0 {
		t.Fatalf("results marked during outage: %#v", store.results)
	}
}
