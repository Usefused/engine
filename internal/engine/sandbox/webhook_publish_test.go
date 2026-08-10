package sandbox

import (
	"net/http/httptest"
	"testing"

	"github.com/nats-io/nats.go"
)

// TestSubjectSafeLabel_ReplacesDots guards the NATS subject's fixed segment
// positions: "." is the subject delimiter, and a kind: webhook artifact's
// name (the label) has no character restriction today, so a literal "." in
// a name must never reach the subject as-is.
func TestSubjectSafeLabel_ReplacesDots(t *testing.T) {
	got := subjectSafeLabel("team.x.webhooks")
	want := "team-x-webhooks"
	if got != want {
		t.Fatalf("subjectSafeLabel(%q) = %q, want %q", "team.x.webhooks", got, want)
	}
}

func TestSubjectSafeLabel_LeavesOrdinaryNameUnchanged(t *testing.T) {
	got := subjectSafeLabel("team-x-webhooks")
	want := "team-x-webhooks"
	if got != want {
		t.Fatalf("subjectSafeLabel(%q) = %q, want %q", "team-x-webhooks", got, want)
	}
}

// TestPublishWebhookEvent_SubjectIncludesLabel is the isolation-bug
// regression test: the subject must carry the registration's label as its
// own segment (webhooks.<account>.<service>.<label>.<event>), not just
// account+service+event -- see plans/plan-webhook-kind.md.
// Two registrations for the same service producing the same event name must
// land on different subjects when their labels differ.
func TestPublishWebhookEvent_SubjectIncludesLabel(t *testing.T) {
	var gotSubject string
	origPublish := webhookPublishFunc
	webhookPublishFunc = func(msg *nats.Msg) error {
		gotSubject = msg.Subject
		return nil
	}
	defer func() { webhookPublishFunc = origPublish }()

	config := &webhookConfig{AccountID: "acct-1", ServiceID: "svc-1", Label: "team-x-webhooks"}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/webhook/abc-svc", nil)

	publishWebhookEvent(w, r, []byte(`{}`), "abc", "issue.created", config)

	want := "webhooks.acct-1.svc-1.team-x-webhooks.issue.created"
	if gotSubject != want {
		t.Fatalf("subject = %q, want %q", gotSubject, want)
	}
}

// TestPublishWebhookEvent_DifferentLabelsProduceDifferentSubjects is the
// direct isolation assertion: same account+service+event, different labels
// -> different subjects, so a subscriber scoped to one registration's
// label can never receive the other's deliveries.
func TestPublishWebhookEvent_DifferentLabelsProduceDifferentSubjects(t *testing.T) {
	var subjects []string
	origPublish := webhookPublishFunc
	webhookPublishFunc = func(msg *nats.Msg) error {
		subjects = append(subjects, msg.Subject)
		return nil
	}
	defer func() { webhookPublishFunc = origPublish }()

	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/webhook/abc-svc", nil)

	publishWebhookEvent(w, r, []byte(`{}`), "abc", "push", &webhookConfig{AccountID: "acct-1", ServiceID: "svc-1", Label: "team-x-webhooks"})
	publishWebhookEvent(w, r, []byte(`{}`), "def", "push", &webhookConfig{AccountID: "acct-1", ServiceID: "svc-1", Label: "team-y-webhooks"})

	if len(subjects) != 2 || subjects[0] == subjects[1] {
		t.Fatalf("expected two distinct subjects for two different labels, got %#v", subjects)
	}
}
