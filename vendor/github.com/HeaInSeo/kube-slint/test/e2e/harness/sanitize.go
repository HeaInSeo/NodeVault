package harness

import "strings"

// SanitizeFilename 은 문자열을 파일 이름으로 사용하기 안전하게 변환함.
// 구성이 파일에서 로드될 때 아티팩트 명명용으로 안정적이게 사용함.
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
	// 길이를 적절히 유지함
	if len(t) > 120 {
		t = t[:120]
	}
	return t
}
