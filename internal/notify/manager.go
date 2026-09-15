package notify

import (
	"context"
	"log/slog"
	"strings"
	"sync"
)

// recoveredPrefix opens the message of every resolve.
const recoveredPrefix = "RECOVERED:"

// Manager is the Notifier the upgrade verb uses. It is safe for concurrent
// Notify and Resolve calls.
type Manager struct {
	log  *slog.Logger
	sink *emailSink

	mu    sync.Mutex
	state map[string]alertState
}

// alertState records whether an alert is raised and the level it was raised
// at, so its resolve mails at that level and crosses the same threshold.
type alertState struct {
	active    bool
	lastLevel slog.Level
}

// Notify raises an alert that is not already raised. A raise of an alert that
// is still active is dropped, so a phase that fails repeatedly mails once.
func (m *Manager) Notify(ctx context.Context, event Event) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id := alertID(event.Kind, event.Key)
	current, exists := m.state[id]
	if exists && current.active {
		return
	}
	m.state[id] = alertState{active: true, lastLevel: event.Level}
	attrs := alertAttrs(event.Kind, event.Key, slog.Bool("transition", true), event.Fields)
	m.emit(ctx, event.Level, event.Kind, event.Key, event.Message, attrs)
}

// Resolve clears a raised alert and reports the recovery at the level the
// alert was raised at. Resolving an alert that is not raised does nothing.
func (m *Manager) Resolve(ctx context.Context, kind, key, message string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id := alertID(kind, key)
	current, exists := m.state[id]
	if !exists || !current.active {
		return
	}
	m.state[id] = alertState{active: false, lastLevel: current.lastLevel}
	level := current.lastLevel
	// slog.LevelInfo is the zero Level, so an alert raised at INFO resolves at
	// WARN, which is also how the gateway notifier resolves one.
	if level == slog.LevelInfo {
		level = slog.LevelWarn
	}
	if !strings.HasPrefix(message, recoveredPrefix) {
		message = recoveredPrefix + " " + message
	}
	attrs := alertAttrs(kind, key, slog.Bool("resolved", true), nil)
	m.emit(ctx, level, kind, key, message, attrs)
}

// emit logs the record and hands it to the mail sink when one is configured.
// A failed delivery is logged and does not fail the phase that raised it.
func (m *Manager) emit(ctx context.Context, level slog.Level, kind, key, message string, attrs []slog.Attr) {
	m.log.LogAttrs(ctx, level, message, attrs...)
	if m.sink == nil {
		return
	}
	if err := m.sink.send(ctx, level, message, attrs); err != nil {
		m.log.WarnContext(ctx, "notify: alert mail not delivered",
			"err", err, "alert_kind", kind, "alert_key", key)
	}
}

func alertID(kind, key string) string {
	return kind + "|" + key
}

func alertAttrs(kind, key string, state slog.Attr, fields []slog.Attr) []slog.Attr {
	attrs := make([]slog.Attr, 0, len(fields)+3)
	attrs = append(attrs, slog.String("alert_kind", kind), slog.String("alert_key", key), state)
	return append(attrs, fields...)
}
