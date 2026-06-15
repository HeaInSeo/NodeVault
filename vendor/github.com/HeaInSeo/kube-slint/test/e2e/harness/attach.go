package harness

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/onsi/ginkgo/v2"
)

// Config는 SLO 측정 세션에 대한 입력을 정의함.
// type Config struct {
// 	Enabled bool	// false면 계측 훅 완전 스킵
// 	Namespace          string
// 	MetricsServiceName string
// 	TestCase           string
// 	Suite              string
// 	RunID              string
// 	ServiceAccountName string
// 	Token              string
// 	ArtifactsDir string
// 	Tags         map[string]string
// 	// 계측 코드에서 선택할 수 있도록 해줌.
// 	Method engine.Method
//     Fetcher fetch.MetricsFetcher
// }

// Attach 는 제공자 함수를 호출하는 BeforeEach/AfterEach 훅을 등록하여
// 현재 테스트의 구성을 가져오고 측정 세션을 관리함.
func Attach(provider func() SessionConfig) (*Session, error) {
	session := &Session{} // 자리 표시자 (impl은 BeforeEach에서 설정됨)

	// 하네스 레벨 토글 (오퍼레이터의 테스트 코드에 노출되지 않음)
	enabled := isEnabledByEnv()

	ginkgo.BeforeEach(func() {
		if !enabled {
			session.reset(nil) // impl=nil이므로 End()는 아무 작업도 수행하지 않음/안전함
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
			"harness: invalid config: Namespace is required "+
				"(Suite=%q TestCase=%q RunID=%q MetricsServiceName=%q SA=%q ArtifactsDir=%q)",
			cfg.Suite, cfg.TestCase, cfg.RunID, cfg.MetricsServiceName, cfg.ServiceAccountName, cfg.ArtifactsDir,
		))
	}
	if strings.TrimSpace(cfg.MetricsServiceName) == "" {
		ginkgo.Fail(fmt.Sprintf(
			"harness: invalid config: MetricsServiceName is required "+
				"(Namespace=%q Suite=%q TestCase=%q RunID=%q SA=%q)",
			cfg.Namespace, cfg.Suite, cfg.TestCase, cfg.RunID, cfg.ServiceAccountName,
		))
	}
	if strings.TrimSpace(cfg.Token) == "" {
		ginkgo.Fail(fmt.Sprintf(
			"harness: invalid config: Token is empty "+
				"(Namespace=%q MetricsServiceName=%q Suite=%q TestCase=%q RunID=%q SA=%q)",
			cfg.Namespace, cfg.MetricsServiceName, cfg.Suite, cfg.TestCase, cfg.RunID, cfg.ServiceAccountName,
		))
	}
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
