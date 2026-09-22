package usage

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"
)

// recordingRepository is a Repository that keeps what it was given.
type recordingRepository struct {
	events []Event
	fail   error
	counts int
}

func (r *recordingRepository) Insert(_ context.Context, e Event) error {
	if r.fail != nil {
		return r.fail
	}
	r.events = append(r.events, e)
	return nil
}

func (r *recordingRepository) Count(context.Context) (int, error) {
	if r.fail != nil {
		return 0, r.fail
	}
	if r.counts != 0 {
		return r.counts, nil
	}
	return len(r.events), nil
}

// newTestService builds a service over a recording repository and a fixed
// clock, and returns the log it writes to so a test can assert on it.
func newTestService(t *testing.T, repo Repository) (*Service, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	service, err := NewService(Options{
		Repository: repo,
		Logger:     slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Now:        func() time.Time { return time.Date(2026, time.September, 22, 9, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatalf("NewService returned an error: %v", err)
	}
	return service, &buf
}

// TestTheVocabularyIsClosed is the package's central promise stated as a test.
//
// The five are the whole of what can be recorded, and the adversarial half
// matters more than the happy half: every string below is something a person
// might have written, and every one of them must be refused rather than
// stored. A filter looking for "password" would pass all but one of these.
func TestTheVocabularyIsClosed(t *testing.T) {
	for _, eventType := range EventTypes {
		if !eventType.Valid() {
			t.Errorf("%q is declared but Valid refuses it", eventType)
		}
	}

	refused := []EventType{
		"",
		"dashboard",
		"DASHBOARD.OPEN",
		"dashboard.open ",
		"dashboard.open\n",
		// The shapes that would make this table a place content could live.
		"/home/dbroot/.claude/projects/-app/transcript.jsonl",
		"prompt: summarise the deploy script",
		"tool_input: rm -rf /srv",
		"claude.output",
		"terminal.connect: 120x30",
		"controller.request from 192.168.1.4",
	}
	for _, eventType := range refused {
		if eventType.Valid() {
			t.Errorf("Valid accepted %q, which is not one of the five", string(eventType))
		}
	}

	// EventTypes is a reading copy; Valid is the authority. The two must agree
	// or the list is a second vocabulary that can drift.
	if len(EventTypes) != 5 {
		t.Errorf("EventTypes has %d entries, want 5", len(EventTypes))
	}
	for _, eventType := range EventTypes {
		if len(string(eventType)) > maxEventTypeLen {
			t.Errorf("%q is longer than the %d-character bound", string(eventType), maxEventTypeLen)
		}
	}
}

// TestTheEventHasNowhereToPutPayload is the structural half of the claim.
//
// The vocabulary test above stops a string being used as an event type. This
// one stops the struct growing a field for content, which is the way the
// promise would actually be broken: not by a caller passing a prompt as a type,
// but by somebody adding a Detail field for a reason that seemed good at the
// time. A reflection assertion fails that build rather than the promise.
func TestTheEventHasNowhereToPutPayload(t *testing.T) {
	eventType := reflect.TypeOf(Event{})
	var fields []string
	for i := 0; i < eventType.NumField(); i++ {
		if field := eventType.Field(i); field.IsExported() {
			fields = append(fields, field.Name)
		}
	}
	want := []string{"ID", "Type", "CreatedAt"}
	if !reflect.DeepEqual(fields, want) {
		t.Errorf("Event has exported fields %v, want exactly %v", fields, want)
	}
}

// TestRecordingWritesOneRow pins what actually reaches storage.
func TestRecordingWritesOneRow(t *testing.T) {
	repo := &recordingRepository{}
	service, _ := newTestService(t, repo)
	ctx := context.Background()

	service.Record(ctx, EventTerminalConnect)

	if len(repo.events) != 1 {
		t.Fatalf("recorded %d events, want 1", len(repo.events))
	}
	got := repo.events[0]
	if got.Type != EventTerminalConnect {
		t.Errorf("Type = %q, want %q", got.Type, EventTerminalConnect)
	}
	if !strings.HasPrefix(got.ID, "use_") {
		t.Errorf("ID = %q, want the use_ prefix", got.ID)
	}
	if !eventIDSpec.Valid(got.ID) {
		t.Errorf("ID = %q, which does not match its own spec", got.ID)
	}
	if got.CreatedAt.Location() != time.UTC {
		t.Errorf("CreatedAt is in %v, want UTC", got.CreatedAt.Location())
	}
	if got.CreatedAt.IsZero() {
		t.Error("CreatedAt is zero")
	}

	// A second event is a second row, with a different identifier: this table
	// counts occurrences, so two presses of the same button are two rows.
	service.Record(ctx, EventTerminalConnect)
	if len(repo.events) != 2 {
		t.Fatalf("recorded %d events, want 2", len(repo.events))
	}
	if repo.events[0].ID == repo.events[1].ID {
		t.Error("two events share an identifier")
	}
}

// TestAnUnknownTypeIsRefusedAndRecordedNowhere is the runtime half of the
// vocabulary claim: Valid is not decoration, the service consults it, and a
// value that fails it never reaches storage.
func TestAnUnknownTypeIsRefusedAndRecordedNowhere(t *testing.T) {
	repo := &recordingRepository{}
	service, logs := newTestService(t, repo)

	service.Record(context.Background(), EventType("prompt: what is in /etc/shadow"))

	if len(repo.events) != 0 {
		t.Fatalf("recorded %d events for a type outside the vocabulary, want 0", len(repo.events))
	}
	if !strings.Contains(logs.String(), "refused to record") {
		t.Errorf("the refusal was not logged; log = %q", logs.String())
	}
	// The warning names the type it refused, which is the whole diagnostic
	// value of it - a caller has to be able to see what it passed.
	if !strings.Contains(logs.String(), "prompt: what is in /etc/shadow") {
		t.Errorf("the refusal did not name the offending type; log = %q", logs.String())
	}
}

// TestRecordingNeverFailsItsCaller pins the contract that makes this safe to
// call from a page handler, a socket and a lease.
//
// A failing store must not produce a panic, an error, or a retry: the thing
// being recorded has already happened, and a page that 500s because a count
// could not be written would be a count taking down the feature it was counting.
func TestRecordingNeverFailsItsCaller(t *testing.T) {
	repo := &recordingRepository{fail: errors.New("the disk is full")}
	service, logs := newTestService(t, repo)
	ctx := context.Background()

	for _, eventType := range EventTypes {
		service.Record(ctx, eventType)
	}

	if !strings.Contains(logs.String(), "could not record a usage event") {
		t.Errorf("a failed write was not logged; log = %q", logs.String())
	}
	if !strings.Contains(logs.String(), "the disk is full") {
		t.Errorf("the failure did not carry its cause; log = %q", logs.String())
	}
}

// TestANilRecorderIsANoOp covers the wiring mistake rather than the ordinary
// state: a nil *Service stored in a Recorder is not a nil interface, so a
// consumer's own nil check does not catch it.
func TestANilRecorderIsANoOp(t *testing.T) {
	var service *Service
	var recorder Recorder = service

	// Neither call may panic.
	recorder.Record(context.Background(), EventDashboardOpen)
	if count, err := service.Count(context.Background()); err != nil || count != 0 {
		t.Errorf("Count on a nil service = (%d, %v), want (0, nil)", count, err)
	}
}

// TestAServiceNeedsARepository pins the construction failure. It is an error
// rather than a no-op because a recorder wired to nothing would silently
// discard every event, and a beta that collected nothing would look exactly
// like a beta nobody used.
func TestAServiceNeedsARepository(t *testing.T) {
	if _, err := NewService(Options{}); err == nil {
		t.Fatal("NewService accepted no repository")
	} else if !IsCode(err, CodeInvalidEvent) {
		t.Errorf("NewService returned %v, want a %s error", err, CodeInvalidEvent)
	}
}
