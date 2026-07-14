package slint

import (
	"strings"
	"unicode"
)

// SanitizeFilename converts a string into something safe to use as a
// filename, so it stays stable for artifact naming when config is loaded
// from a file.
func SanitizeFilename(s string) string {
	t := strings.TrimSpace(s)
	if t == "" {
		return "unknown"
	}
	repl := strings.NewReplacer(
		"/", "_", "\\", "_", " ", "_", ":", "_", ";", "_",
		"\"", "_", "'", "_", "\n", "_", "\r", "_", "\t", "_",
	)
	t = repl.Replace(t)
	// Keep the length reasonable.
	if len(t) > 120 {
		t = t[:120]
	}
	return t
}

// SanitizeKubernetesLabelValue returns a value that is safe for Kubernetes label
// values and label selectors. The original RunID remains available in summaries.
func SanitizeKubernetesLabelValue(s string) string {
	t := strings.TrimSpace(s)
	if t == "" {
		return "unknown"
	}

	var b strings.Builder
	for _, r := range t {
		switch {
		case r == '-' || r == '_' || r == '.':
			b.WriteRune(r)
		case r <= unicode.MaxASCII && (unicode.IsLetter(r) || unicode.IsDigit(r)):
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	t = b.String()
	t = strings.Trim(t, "-_.")
	if t == "" {
		t = "unknown"
	}
	if len(t) > 63 {
		t = t[:63]
		t = strings.Trim(t, "-_.")
		if t == "" {
			t = "unknown"
		}
	}
	return t
}
