package promtext

import (
	"bufio"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/HeaInSeo/kube-slint/pkg/slo/common/promkey"
)

// ParseTextToMap 은 Prometheus 노출 형식(텍스트)을 플랫 맵으로 파싱함.
// 키 형식 예시:
//
//	metric_name{a="b",c="d"}
//
// 레이블이 없는 경우:
//
//	metric_name
//
// v3: 일반적인 경우(카운터/게이지)를 위한 최소한의 파서임.
func ParseTextToMap(r io.Reader) (map[string]float64, error) {
	out := map[string]float64{}
	sc := bufio.NewScanner(r)

	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// "key value" 분리
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		rawKey := fields[0]
		key, err := promkey.Canonicalize(rawKey)
		if err != nil {
			// v3 정책: 잘못된 형식의 메트릭 라인 건너뛰기 (최선의 파서)
			continue
		}
		valStr := fields[1]
		v, err := strconv.ParseFloat(valStr, 64)
		if err != nil {
			return nil, fmt.Errorf("parse float: %q: %w", line, err)
		}

		out[key] = v
	}

	if err := sc.Err(); err != nil {
		return nil, err
	}

	return out, nil
}
