// Package notify raises and resolves the OPNsense upgrade alerts. An alert is
// raised once per (kind, key) until it is resolved, every raise and resolve is
// logged, and those at or above the configured level are mailed through
// goodkind.io/send-email using the [email] section of the OPNsense tooling
// configuration.
package notify

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"

	"goodkind.io/gklog"
	"goodkind.io/send-email/mailer"

	"goodkind.io/opnsensectl/internal/config"
)

// Event is one alert raise. Key separates alerts of the same Kind, such as one
// upgrade run per guest.
type Event struct {
	Level   slog.Level
	Kind    string
	Key     string
	Message string
	Fields  []slog.Attr
}

// Notifier is the alert surface the upgrade phases call.
type Notifier interface {
	Notify(ctx context.Context, event Event)
	Resolve(ctx context.Context, kind, key, message string)
}

// Mailer is the one send-email call the notifier makes. *mailer.Mailer
// satisfies it.
type Mailer interface {
	Send(ctx context.Context, message mailer.Message) error
}

// MailerFactory builds the Mailer for one delivery attempt from that attempt's
// send-email configuration.
type MailerFactory func(mailerConfig mailer.Config) Mailer

// errLogRequired reports a nil logger, since every raise and resolve is logged
// whether or not it is mailed.
var errLogRequired = errors.New("notify: log is required")

// New builds a Manager that mails through goodkind.io/send-email. serviceName
// is the caller name send-email prints on each message.
func New(section config.EmailSection, log *slog.Logger, serviceName string) (*Manager, error) {
	return NewWithMailer(section, log, serviceName, newSendEmailMailer)
}

// NewWithMailer builds a Manager whose delivery attempts use newMailer. When
// the section names no API key or no recipient, the Manager logs alerts and
// mails none.
func NewWithMailer(
	section config.EmailSection,
	log *slog.Logger,
	serviceName string,
	newMailer MailerFactory,
) (*Manager, error) {
	if log == nil {
		slog.Error("notify: construct manager failed", "err", errLogRequired, "service", serviceName)
		return nil, errLogRequired
	}
	manager := &Manager{
		log:   log.With("component", "notify"),
		sink:  nil,
		mu:    sync.Mutex{},
		state: map[string]alertState{},
	}
	apiKey := strings.TrimSpace(section.SMTP2GOAPIKey)
	recipient := strings.TrimSpace(section.AlertEmail)
	if apiKey == "" || recipient == "" {
		log.Warn("notify: [email] has no smtp2go_api_key or alert_email, so alerts are logged and not mailed",
			"service", serviceName)
		return manager, nil
	}
	manager.sink = &emailSink{
		newMailer:     newMailer,
		apiKey:        apiKey,
		from:          section.From,
		bindIface:     section.BindIface,
		caller:        serviceName,
		to:            recipient,
		subjectPrefix: section.SubjectPrefix,
		minLevel:      gklog.ParseLevel(section.MinLevel),
	}
	return manager, nil
}

func newSendEmailMailer(mailerConfig mailer.Config) Mailer {
	return mailer.New(mailerConfig)
}
