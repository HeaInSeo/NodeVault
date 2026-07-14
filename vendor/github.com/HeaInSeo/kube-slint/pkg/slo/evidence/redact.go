package evidence

import "regexp"

// secretKeyNames is shared across the key=value, JSON, and YAML/plain-colon
// redaction rules below.
const secretKeyNames = `token|password|passwd|secret|serviceaccounttoken|clientsecret`

// redactRule pairs a secret-matching regex with its replacement template.
// Each regex must capture the non-secret prefix as group 1 (and, for
// delimited values, the closing delimiter as group 2) so ReplaceAllString can
// splice in [REDACTED] without disturbing surrounding text.
type redactRule struct {
	re   *regexp.Regexp
	repl string
}

var redactRules = []redactRule{
	// "Bearer <token>" anywhere — covers both full header strings and map values.
	{regexp.MustCompile(`(?i)(Bearer\s+)\S+`), "${1}[REDACTED]"},

	// CLI flags carrying secret material: --token, --certificate-authority-data,
	// --client-key-data (accepts both "--flag value" and "--flag=value").
	{regexp.MustCompile(`(?i)(--(?:token|certificate-authority-data|client-key-data)[= ])\S+`), "${1}[REDACTED]"},

	// key=value form: token, password, passwd, secret, serviceAccountToken, clientSecret.
	{regexp.MustCompile(`(?i)(\b(?:` + secretKeyNames + `)\s*=\s*)\S+`), "${1}[REDACTED]"},

	// JSON-quoted form: "token": "value" (also password/passwd/secret/serviceAccountToken/clientSecret).
	// The closing quote is optional so a truncated/malformed response (e.g. an
	// unterminated string cut off mid-token) is still redacted up to EOF.
	{regexp.MustCompile(`(?i)("(?:` + secretKeyNames + `)"\s*:\s*")[^"]*("|$)`), "${1}[REDACTED]${2}"},

	// YAML/plain colon form: token: value, client-key-data: value, etc. — covers
	// secret material embedded in kubeconfig-shaped text.
	{regexp.MustCompile(`(?i)(\b(?:` + secretKeyNames + `|client-key-data|certificate-authority-data)\s*:\s*)\S+`), "${1}[REDACTED]"},
}

// RedactString replaces known secret patterns in s with [REDACTED].
// Safe to call on empty strings or strings with no secrets.
func RedactString(s string) string {
	for _, rule := range redactRules {
		s = rule.re.ReplaceAllString(s, rule.repl)
	}
	return s
}

// RedactMap returns a copy of m with all values passed through RedactString.
// Keys are not modified.
func RedactMap(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = RedactString(v)
	}
	return out
}
