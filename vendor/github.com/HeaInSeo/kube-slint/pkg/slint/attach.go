package slint

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/onsi/ginkgo/v2"
)

// Attach registers BeforeEach/AfterEach hooks that call provider to fetch
// the current test's config (see SessionConfig in session.go for its full,
// authoritative field list) and manage the measurement session. Calling
// Attach() is itself the opt-in; disable it without code changes via
// SLINT_ENABLED=0/false (see isEnabledByEnv below).
func Attach(provider func() SessionConfig) (*Session, error) {
	session := &Session{} // placeholder; impl is set in BeforeEach

	// Harness-level toggle, not exposed to the operator's test code.
	enabled := isEnabledByEnv()

	ginkgo.BeforeEach(func() {
		if !enabled {
			session.reset(nil) // impl=nil, so End() is a safe no-op
			return
		}

		cfg := provider()
		validateSessionConfigOrFail(cfg)
		cfg = fillSessionDefaults(cfg)

		newSess := NewSession(cfg)

		session.reset(newSess)
		session.Start()
	})

	ginkgo.AfterEach(func() {
		if !enabled {
			return
		}
		if session == nil || session.impl == nil {
			_, _ = fmt.Fprintf(ginkgo.GinkgoWriter, "SLO: session not initialized (enabled=true)\n")
			return
		}
		if _, err := session.End(context.Background()); err != nil {
			_, _ = fmt.Fprintf(ginkgo.GinkgoWriter, "SLO: End failed (skip): %v\n", err)
		}
	})

	return session, nil
}

// isEnabledByEnv returns false only when SLINT_ENABLED is explicitly set to "0" or "false".
// Calling Attach() itself is the opt-in; this allows programmatic disable without code changes.
func isEnabledByEnv() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("SLINT_ENABLED")))
	return v != "0" && v != "false"
}

func validateSessionConfigOrFail(cfg SessionConfig) {
	if strings.TrimSpace(cfg.Namespace) == "" {
		ginkgo.Fail(fmt.Sprintf(
			"slint: invalid config: Namespace is required "+
				"(Suite=%q TestCase=%q RunID=%q MetricsServiceName=%q SA=%q ArtifactsDir=%q)",
			cfg.Suite, cfg.TestCase, cfg.RunID, cfg.MetricsServiceName, cfg.ServiceAccountName, cfg.ArtifactsDir,
		))
	}
	if strings.TrimSpace(cfg.MetricsServiceName) == "" {
		ginkgo.Fail(fmt.Sprintf(
			"slint: invalid config: MetricsServiceName is required "+
				"(Namespace=%q Suite=%q TestCase=%q RunID=%q SA=%q)",
			cfg.Namespace, cfg.Suite, cfg.TestCase, cfg.RunID, cfg.ServiceAccountName,
		))
	}
	// Note: Token is intentionally not required. The default curlpod fetcher
	// reads its bearer token from the pod's own mounted ServiceAccount token
	// file rather than from cfg.Token (see docs/post-rc-hardening-design.md).
	// cfg.Token remains available for callers supplying a custom Fetcher.
}

func fillSessionDefaults(cfg SessionConfig) SessionConfig {
	if strings.TrimSpace(cfg.TestCase) == "" {
		cfg.TestCase = ginkgo.CurrentSpecReport().LeafNodeText
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return cfg
}
