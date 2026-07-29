package worker

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"time"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/messaging"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/Usefused/engine/internal/shared/observability"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
)

func StartWebhookAnalyticsWorker(ctx context.Context, engineStore store.Store, natsClient *messaging.NATSClient) {
	if natsClient == nil || natsClient.JS == nil {
		return
	}

	// Create or update consumer
	_, err := natsClient.JS.AddConsumer("WEBHOOK_ANALYTICS_EVENTS", &nats.ConsumerConfig{
		Durable:       "webhook_analytics_worker",
		FilterSubject: "webhook.analytics.>",
		AckPolicy:     nats.AckExplicitPolicy,
		MaxAckPending: 2000,
	})
	if err != nil {
		slog.ErrorContext(ctx, "Failed to add durable consumer for webhook.analytics", slog.Any("error", err))
		return
	}

	sub, err := natsClient.JS.PullSubscribe("webhook.analytics.>", "webhook_analytics_worker")
	if err != nil {
		slog.ErrorContext(ctx, "Failed to subscribe to webhook analytics stream", slog.Any("error", err))
		return
	}

	slog.InfoContext(ctx, "Started Webhook Analytics Worker with JetStream micro-batching")

	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			default:
				msgs, err := sub.Fetch(100, nats.MaxWait(1*time.Second))
				if err != nil {
					if err == nats.ErrTimeout {
						continue
					}
					slog.ErrorContext(ctx, "Error fetching webhook analytics events from JetStream", slog.Any("error", err))
					time.Sleep(2 * time.Second) // backoff
					continue
				}

				if len(msgs) == 0 {
					continue
				}

				thread, _ := observability.Start(ctx, "Worker: Webhook Analytics", "", "worker:webhook_analytics")

				var events []models.WebhookEvent
				var validMsgs []*nats.Msg

				for _, msg := range msgs {
					parts := strings.Split(msg.Subject, ".")
					if len(parts) != 3 { // webhook.analytics.status
						msg.Ack()
						continue
					}

					var payload struct {
						MsgID              string     `json:"msg_id"`
						AccountID          string     `json:"account_id"`
						ServiceID          string     `json:"service_id"`
						EventName          string     `json:"event_name"`
						ErrorReason        string     `json:"error_reason"`
						Status             string     `json:"status"`
						VerificationStatus string     `json:"verification_status"`
						Environment        string     `json:"environment"`
						LatencyMs          int        `json:"latency_ms"`
						RetryCount         int        `json:"retry_count"`
						PayloadSize        int        `json:"payload_size"`
						Timestamp          time.Time  `json:"timestamp"`
						SDKRecordID        *uuid.UUID `json:"sdk_record_id,omitempty"`
					}

					if err := json.Unmarshal(msg.Data, &payload); err != nil {
						slog.ErrorContext(ctx, "Failed to unmarshal webhook analytics event", slog.Any("error", err))
						thread.Step("Failed to unmarshal webhook analytics event").Error(ctx, err)
						msg.Ack()
						continue
					}

					accountID, _ := uuid.Parse(payload.AccountID)
					serviceID, _ := uuid.Parse(payload.ServiceID)

					env := payload.Environment
					if env == "" {
						env = "prod"
					}
					event := models.WebhookEvent{
						ID:                 uuid.New(),
						AccountID:          accountID,
						ServiceID:          serviceID,
						MsgID:              payload.MsgID,
						EventType:          payload.EventName,
						VerificationStatus: payload.VerificationStatus,
						DeliveryStatus:     payload.Status,
						ErrorReason:        payload.ErrorReason,
						Environment:        env,
						LatencyMs:          payload.LatencyMs,
						RetryCount:         payload.RetryCount,
						PayloadSize:        payload.PayloadSize,
						CreatedAt:          payload.Timestamp,
						SDKRecordID:        payload.SDKRecordID,
					}
					events = append(events, event)
					validMsgs = append(validMsgs, msg)
				}

				if len(events) > 0 {
					if err := engineStore.BatchCreateWebhookEvents(ctx, events); err != nil {
						slog.ErrorContext(ctx, "Failed to batch insert webhook analytics events", slog.Any("error", err))
						thread.Step("Failed to batch insert webhook analytics events").Error(ctx, err)
						for _, msg := range validMsgs {
							msg.Nak()
						}
						thread.Complete(ctx, "Batch failed")
						continue
					}
				}

				for _, msg := range validMsgs {
					msg.Ack()
				}
				thread.Complete(ctx, "Batch processed")
			}
		}
	}()
}
