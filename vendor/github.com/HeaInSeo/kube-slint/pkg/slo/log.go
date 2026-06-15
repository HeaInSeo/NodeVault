package slo

// Logger 는 pkg/slo를 위한 최소한의 로깅 계약임.
type Logger interface {
	Logf(format string, args ...any)
}

type nopLogger struct{}

// Logf 는 Logger 인터페이스를 구현함.
func (nopLogger) Logf(string, ...any) {}

// NewLogger 는 안전한 Logger를 반환함. l이 nil이면 no-op을 반환함.
func NewLogger(l Logger) Logger {
	if l == nil {
		return nopLogger{}
	}
	return l
}

// NopLogger 는 원할 경우 사용할 수 있는 내보내진 NopLogger 싱글톤임.
var NopLogger Logger = nopLogger{}
