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
	sc.Buffer(make([]byte, 1024), 1024*1024)

	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// "key rest" 분리. rawKey는 label 블록({...}) 전체를 포함해야 하므로
		// 단순 strings.Fields로는 label 값에 공백이 있는 경우 깨짐 (F4) —
		// splitMetricLine이 label 블록의 끝(따옴표 안이 아닌 '}')을 직접 찾음.
		rawKey, rest, ok := splitMetricLine(line)
		if !ok {
			continue
		}
		key, err := promkey.Canonicalize(rawKey)
		if err != nil {
			// v3 정책: 잘못된 형식의 메트릭 라인 건너뛰기 (최선의 파서)
			continue
		}
		valueFields := strings.Fields(rest)
		if len(valueFields) < 1 {
			continue
		}
		valStr := valueFields[0]
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

// splitMetricLine splits an exposition-format line into its key token
// (metric name, plus the full "{...}" label block if present) and the
// remaining "value [timestamp]" text.
//
// A naive strings.Fields(line) split breaks when a label value contains
// whitespace (e.g. `metric{path="/foo bar"} 1`), since it would split the
// label block itself into multiple tokens. This instead locates the '{'
// opening the label block (if any) and scans forward to its matching
// unquoted '}', so whitespace inside a quoted label value never confuses
// the boundary between the key and the value.
func splitMetricLine(line string) (key, rest string, ok bool) {
	brace := strings.IndexByte(line, '{')
	if brace < 0 {
		i := strings.IndexAny(line, " \t")
		if i < 0 {
			return "", "", false
		}
		return line[:i], strings.TrimSpace(line[i:]), true
	}
	end := findLabelBlockEnd(line, brace)
	if end < 0 {
		return "", "", false
	}
	return line[:end+1], strings.TrimSpace(line[end+1:]), true
}

// findLabelBlockEnd returns the index of the '}' that closes the label
// block opened at line[openBrace], skipping over any '}' (or whitespace)
// that appears inside a quoted label value. Returns -1 if no such
// closing brace exists (e.g. an unterminated quoted value).
func findLabelBlockEnd(line string, openBrace int) int {
	inQuotes := false
	for i := openBrace + 1; i < len(line); i++ {
		switch line[i] {
		case '\\':
			if inQuotes {
				i++ // skip the escaped character
			}
		case '"':
			inQuotes = !inQuotes
		case '}':
			if !inQuotes {
				return i
			}
		}
	}
	return -1
}
