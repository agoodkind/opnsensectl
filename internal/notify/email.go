package notify

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"goodkind.io/send-email/mailer"
)

const (
	// sendAttemptTimeout bounds one delivery attempt, so a stalled route leaves
	// time for the attempt through the bind interface.
	sendAttemptTimeout = 15 * time.Second
	// defaultFromDomain is the sender domain send-email uses when From is empty.
	defaultFromDomain = "goodkind.io"
)

// emailSink mails one alert to the configured recipient. It drops records
// below minLevel, and when the default route fails it retries once over the
// SMTP2GO HTTP API bound to bindIface.
type emailSink struct {
	newMailer     MailerFactory
	apiKey        string
	from          string
	bindIface     string
	caller        string
	to            string
	subjectPrefix string
	minLevel      slog.Level
}

func (s *emailSink) send(ctx context.Context, level slog.Level, message string, attrs []slog.Attr) error {
	if level < s.minLevel {
		return nil
	}
	outgoing := mailer.Message{
		To:      s.to,
		Subject: buildSubject(s.subjectPrefix, level, message),
		Body:    buildBody(message, attrs),
		From:    s.from,
		Name:    "",
		Caller:  s.caller,
	}
	primaryErr := s.sendVia(ctx, "", outgoing)
	if primaryErr == nil {
		return nil
	}
	if s.bindIface == "" {
		return primaryErr
	}
	slog.WarnContext(ctx, "notify: alert mail failed via the default route, retrying via the bind interface",
		"bind_iface", s.bindIface, "err", primaryErr)
	return s.sendVia(ctx, s.bindIface, outgoing)
}

func (s *emailSink) sendVia(ctx context.Context, bindIface string, outgoing mailer.Message) error {
	transport := mailer.MethodAuto
	if bindIface != "" {
		transport = mailer.MethodHTTP
	}
	attemptMailer := s.newMailer(mailer.Config{
		SMTP2GOAPIKey:     s.apiKey,
		MsmtprcPath:       "",
		DefaultFromDomain: defaultFromDomain,
		BindInterface:     bindIface,
		Transport:         transport,
		Now:               nil,
	})
	attemptCtx, cancel := context.WithTimeout(ctx, sendAttemptTimeout)
	defer cancel()
	if err := attemptMailer.Send(attemptCtx, outgoing); err != nil {
		wrapped := fmt.Errorf("notify: send alert mail via %q: %w", bindIface, err)
		slog.ErrorContext(ctx, "notify: send alert mail failed",
			"err", wrapped, "bind_iface", bindIface, "to", s.to)
		return wrapped
	}
	return nil
}

// buildSubject composes "[prefix] LEVEL: message", or "LEVEL: message" when no
// prefix is configured.
func buildSubject(prefix string, level slog.Level, message string) string {
	levelName := strings.ToUpper(level.String())
	trimmedPrefix := strings.TrimSpace(prefix)
	if trimmedPrefix == "" {
		return levelName + ": " + message
	}
	return trimmedPrefix + " " + levelName + ": " + message
}
