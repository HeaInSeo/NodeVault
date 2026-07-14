package curlpod

import (
	"fmt"
	"net/url"
	"regexp"

	"github.com/HeaInSeo/kube-slint/pkg/kubeutil"
)

// dnsLabelRe matches a valid DNS-1123 label (the same shape Kubernetes
// requires for Service/Namespace names).
var dnsLabelRe = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

func isValidDNSLabel(s string) bool {
	return len(s) > 0 && len(s) <= 63 && dnsLabelRe.MatchString(s)
}

// isDangerousNamespace reports whether ns is a cluster-critical namespace.
// Delegates to kubeutil.IsDangerousNamespace so every kubectl-issuing path
// in this module shares one definition instead of drifting independently.
func isDangerousNamespace(ns string) bool {
	return kubeutil.IsDangerousNamespace(ns)
}

// ValidateMetricsURL builds the metrics scrape URL from a ServiceURLFormat
// template plus service/namespace values, and validates that the result is a
// safe cluster-local address before any curl pod is created.
//
// service and namespace must each be a valid DNS-1123 label — this closes a
// template-injection path where an attacker-controlled service/namespace
// value (e.g. "x.evil.example") could otherwise smuggle a different host
// into the interpolated URL even when ServiceURLFormat itself is the safe
// default template.
//
// The resulting URL's scheme must be http or https, and its host must be
// exactly "<service>.<namespace>.svc" or
// "<service>.<namespace>.svc.cluster.local" (any port), unless allowExternal
// is true (the caller's explicit dangerous opt-in).
func ValidateMetricsURL(format, service, namespace string, allowExternal bool) (string, error) {
	if !isValidDNSLabel(service) {
		return "", fmt.Errorf("invalid metrics service name %q: must be a valid DNS label", service)
	}
	if !isValidDNSLabel(namespace) {
		return "", fmt.Errorf("invalid namespace %q: must be a valid DNS label", namespace)
	}

	raw := fmt.Sprintf(format, service, namespace)
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("invalid metrics URL: %w", err)
	}

	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("unsupported metrics URL scheme %q (only http/https allowed)", u.Scheme)
	}

	if allowExternal {
		return raw, nil
	}

	host := u.Hostname()
	wantSVC := service + "." + namespace + ".svc"
	wantSVCFQDN := wantSVC + ".cluster.local"
	if host != wantSVC && host != wantSVCFQDN {
		return "", fmt.Errorf(
			"metrics URL host %q is not a cluster-local service address (expected %q or %q); "+
				"set DangerouslyAllowExternalMetricsURL to override",
			host, wantSVC, wantSVCFQDN,
		)
	}

	return raw, nil
}
