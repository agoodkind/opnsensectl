package notify

import (
	"log/slog"
	"sort"
	"strings"
)

const (
	// errKey is the attribute that renders as the What line.
	errKey = "err"
	// stderrMarker introduces the command output an error carries, which the
	// What line moves onto its own line.
	stderrMarker = "stderr="
	// phaseKey is the attribute that renders as the Where line.
	phaseKey = "phase"
)

// buildBody renders the message, then a What line for the error, then a Where
// line for the phase, then every other attribute as a sorted "Key: value" line.
// send-email appends its host snapshot below this body.
func buildBody(message string, attrs []slog.Attr) string {
	values := make(map[string]string, len(attrs))
	for _, attr := range attrs {
		// An attribute with no key has no label to render, so it is skipped.
		if attr.Key == "" {
			continue
		}
		values[attr.Key] = attr.Value.String()
	}
	sections := []string{message}
	if what := buildWhat(values); what != "" {
		sections = append(sections, what)
	}
	if where := buildWhere(values); where != "" {
		sections = append(sections, where)
	}
	if extras := buildExtras(values); extras != "" {
		sections = append(sections, extras)
	}
	return strings.Join(sections, "\n\n")
}

func buildWhat(values map[string]string) string {
	errValue, ok := values[errKey]
	if !ok {
		return ""
	}
	delete(values, errKey)
	mainPart, stderrPart := splitStderr(errValue)
	if stderrPart == "" {
		return "What:    " + mainPart
	}
	return "What:    " + mainPart + "\n         stderr: " + stderrPart
}

// splitStderr separates a stderr="..." or unquoted stderr=... fragment from an
// error string. The main part loses the fragment and its trailing punctuation.
func splitStderr(errValue string) (string, string) {
	before, after, ok := strings.Cut(errValue, stderrMarker)
	if !ok {
		return errValue, ""
	}
	mainPart := strings.TrimRight(strings.TrimSpace(before), "(,: ")
	rest := strings.TrimSuffix(strings.TrimSpace(after), ")")
	if strings.HasPrefix(rest, "\"") {
		closing := strings.LastIndex(rest, "\"")
		if closing > 0 {
			return mainPart, strings.TrimSpace(rest[1:closing])
		}
	}
	return mainPart, rest
}

func buildWhere(values map[string]string) string {
	phase, ok := values[phaseKey]
	if !ok {
		return ""
	}
	delete(values, phaseKey)
	return "Where:   " + phaseKey + "=" + phase
}

func buildExtras(values map[string]string) string {
	if len(values) == 0 {
		return ""
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	lines := make([]string, 0, len(keys))
	for _, key := range keys {
		lines = append(lines, strings.ToUpper(key[:1])+key[1:]+": "+values[key])
	}
	return strings.Join(lines, "\n")
}
