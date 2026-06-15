package tags

// AutoTagsInput 은 자동 태그 필드를 정의함.
type AutoTagsInput struct {
	Suite     string
	TestCase  string
	Namespace string
	RunID     string
}

// AutoTags 는 자동 태그 맵을 반환함.
func AutoTags(input AutoTagsInput) map[string]string {
	return map[string]string{
		"suite":     input.Suite,
		"test_case": input.TestCase,
		"namespace": input.Namespace,
		"run_id":    input.RunID,
	}
}

// MergeTags 는 사용자 태그를 자동 태그 위에 병합함(사용자 재정의).
func MergeTags(userTags map[string]string, autoTags map[string]string) map[string]string {
	merged := map[string]string{}
	for key, value := range autoTags {
		if value != "" {
			merged[key] = value
		}
	}
	for key, value := range userTags {
		merged[key] = value
	}
	return merged
}
