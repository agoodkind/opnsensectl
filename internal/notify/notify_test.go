package notify_test

import (
	"context"
	"log/slog"
	"sync"
	"testing"

	"goodkind.io/send-email/mailer"

	"goodkind.io/opnsensectl/internal/config"
	"goodkind.io/opnsensectl/internal/notify"
)

type recordingMailer struct {
	mu       sync.Mutex
	messages []mailer.Message
}

// Send stands in for the SMTP2GO HTTP API call inside send-email, which a test
// can neither reach nor redirect.
func (m *recordingMailer) Send(_ context.Context, message mailer.Message) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.messages = append(m.messages, message)
	return nil
}

// TestNotifyMailsAnAlertWhoseFieldsIncludeAnEmptyKey raises an alert carrying a
// field with no key and checks that the alert is still mailed, with the keyless
// field left out of the body.
func TestNotifyMailsAnAlertWhoseFieldsIncludeAnEmptyKey(t *testing.T) {
	recorder := &recordingMailer{}
	section := config.EmailSection{
		SMTP2GOAPIKey: "key",
		AlertEmail:    "opnsense-alerts@example.test",
		From:          "opnsense-upgrade@example.test",
		SubjectPrefix: "[OPNsense-test]",
		BindIface:     "",
		MinLevel:      "ERROR",
	}
	manager, err := notify.NewWithMailer(section, slog.New(slog.DiscardHandler), "mwan-opnsense-upgrade",
		func(mailer.Config) notify.Mailer { return recorder })
	if err != nil {
		t.Fatalf("NewWithMailer: %v", err)
	}

	manager.Notify(context.Background(), notify.Event{
		Level:   slog.LevelError,
		Kind:    "opnsense-upgrade-execute",
		Key:     "101",
		Message: "opnsense-upgrade execute: apply update failed",
		Fields:  []slog.Attr{{Key: "", Value: slog.StringValue("orphan")}, slog.String("vmid", "101")},
	})

	if len(recorder.messages) != 1 {
		t.Fatalf("mailed %d messages, want 1", len(recorder.messages))
	}
	wantBody := "opnsense-upgrade execute: apply update failed\n\n" +
		"Alert_key: 101\n" +
		"Alert_kind: opnsense-upgrade-execute\n" +
		"Transition: true\n" +
		"Vmid: 101"
	if recorder.messages[0].Body != wantBody {
		t.Errorf("body:\n--- got ---\n%s\n--- want ---\n%s", recorder.messages[0].Body, wantBody)
	}
}
