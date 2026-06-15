package fetch

import "context"

// InsideSnapshotFetch 는 내부 CurlPod 전용 경계를 사용하여 메트릭을 가져옴.
func InsideSnapshotFetch(ctx context.Context, fetchFunc func(context.Context) (string, error)) (string, []string) {
	body, err := fetchFunc(ctx)
	if err != nil {
		return "", []string{err.Error()}
	}
	return body, nil
}
