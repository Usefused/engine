package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"
	_ "time/tzdata" // Self-hosted images must understand IANA zones without OS timezone packages.

	"github.com/google/uuid"
	"github.com/robfig/cron/v3"
)

var ErrScheduleConflict = errors.New("schedule changed, is running, or no longer exists; reload before retrying")

// UnifiedAppSchedule is workspace-owned automation; private input is returned only to app managers.
type UnifiedAppSchedule struct {
	ID              uuid.UUID       `json:"id"`
	AccountID       uuid.UUID       `json:"-"`
	FamilyID        uuid.UUID       `json:"app_family_id"`
	Name            string          `json:"name"`
	Input           json.RawMessage `json:"input"`
	Cron            string          `json:"cron"`
	Timezone        string          `json:"timezone"`
	RunAt           *time.Time      `json:"run_at"`
	Enabled         bool            `json:"enabled"`
	Revision        int64           `json:"revision"`
	NextRunAt       *time.Time      `json:"next_run_at"`
	LastExecutionID *uuid.UUID      `json:"last_execution_id"`
	LastAppID       *uuid.UUID      `json:"last_app_id"`
	LastStatus      string          `json:"last_status"`
	LastStartedAt   *time.Time      `json:"last_started_at"`
	CreatedBy       uuid.UUID       `json:"created_by"`
	CreatedAt       time.Time       `json:"created_at"`
}

// UnifiedAppScheduleStore owns configuration and atomically advances an occurrence with its execution reservation.
type UnifiedAppScheduleStore interface {
	ListUnifiedAppSchedules(context.Context, uuid.UUID, uuid.UUID, []byte) ([]UnifiedAppSchedule, error)
	SaveUnifiedAppSchedule(context.Context, UnifiedAppSchedule, []byte) error
	DeleteUnifiedAppSchedule(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, int64) error
	DueUnifiedAppSchedules(context.Context, []byte) ([]UnifiedAppSchedule, error)
	ReserveScheduledExecution(context.Context, UnifiedAppSchedule, ExecutionResult) (bool, error)
	FinishScheduledExecution(context.Context, uuid.UUID, uuid.UUID, string) error
	RecoverScheduledExecutions(context.Context) ([]ExecutionResult, error)
}

// NextScheduleTime uses local wall-clock cron semantics, with no seconds field or embedded timezone overrides.
func NextScheduleTime(expression, zone string, after time.Time) (time.Time, error) {
	// The timezone is a separate reviewed field, never an injected cron directive.
	if len(strings.Fields(expression)) != 5 || len(expression) > 256 {
		return time.Time{}, errors.New("cron must have five fields: minute hour day month weekday")
	}
	location, err := time.LoadLocation(zone)
	// Empty and host-local zones would make replicas disagree on the next occurrence.
	if err != nil || zone == "" || zone == "Local" {
		return time.Time{}, errors.New("timezone must be an IANA name, such as Europe/London or UTC")
	}
	parsed, err := cron.ParseStandard(expression)
	// Invalid or impossible expressions must not silently disable a requested schedule.
	if err != nil {
		return time.Time{}, errors.New("invalid cron expression")
	}
	next := parsed.Next(after.In(location))
	// The parser bounds its search; a zero time represents an impossible calendar combination.
	if next.IsZero() {
		return time.Time{}, errors.New("cron has no upcoming occurrence")
	}
	return next.UTC(), nil
}

// ValidateSchedule bounds persisted inputs and computes one future occurrence without executing app code.
func ValidateSchedule(schedule *UnifiedAppSchedule, now time.Time) error {
	schedule.Name = strings.TrimSpace(schedule.Name)
	// Identity validation is independent of calendar and input validation.
	if err := validateScheduleIdentity(*schedule); err != nil {
		return err
	}
	input, err := canonicalExecutionJSON(schedule.Input, true)
	// Inputs share the existing execution size and JSON constraints.
	if err != nil {
		return err
	}
	schedule.Input = input
	return validateScheduleTiming(schedule, now)
}

// validateScheduleIdentity bounds ownership and labels before private input is persisted.
func validateScheduleIdentity(schedule UnifiedAppSchedule) error {
	// Complete ownership and a small label are mandatory before persistence.
	if schedule.ID == uuid.Nil || schedule.AccountID == uuid.Nil || schedule.FamilyID == uuid.Nil || schedule.Name == "" || len(schedule.Name) > 128 || schedule.Revision < 0 {
		return errors.New("schedule identity or name is invalid")
	}
	return nil
}

// validateScheduleTiming rejects ambiguous calendars and computes the next reviewed occurrence.
func validateScheduleTiming(schedule *UnifiedAppSchedule, now time.Time) error {
	// Exactly one timing mode avoids ambiguous precedence between a date and recurrence.
	if (schedule.RunAt == nil) == (schedule.Cron == "") {
		return errors.New("provide either run_at or cron")
	}
	_, err := time.LoadLocation(schedule.Timezone)
	// The displayed zone must be portable even for one-time schedules.
	if err != nil || schedule.Timezone == "" || schedule.Timezone == "Local" {
		return errors.New("timezone must be an IANA name")
	}
	next := schedule.RunAt
	// Recurrence is recomputed from now so resuming does not replay a backlog.
	if schedule.Cron != "" {
		value, err := NextScheduleTime(schedule.Cron, schedule.Timezone, now)
		// Invalid calendars cannot be saved as successful automation.
		if err != nil {
			return err
		}
		next = &value
	} else if schedule.Enabled && !schedule.RunAt.After(now) {
		// Creating or resuming a one-time run requires a deliberate future time.
		return errors.New("run_at must be in the future")
	}
	schedule.NextRunAt = next
	return nil
}
