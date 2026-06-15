package summary

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Writer 는 Summary 아티팩트를 대상 위치에 기록함.
type Writer interface {
	Write(path string, s Summary) error
}

// LoadFile reads a Summary from path, parses JSON, and validates the schema version.
// Returns an error if the file is missing, not valid JSON, or has an unsupported schemaVersion.
// External tools should use this instead of rolling their own JSON decode.
func LoadFile(path string) (Summary, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Summary{}, fmt.Errorf("summary.LoadFile: %w", err)
	}
	var s Summary
	if err := json.Unmarshal(data, &s); err != nil {
		return Summary{}, fmt.Errorf("summary.LoadFile: invalid JSON: %w", err)
	}
	if err := ValidateSchemaVersion(s); err != nil {
		return Summary{}, fmt.Errorf("summary.LoadFile: %w", err)
	}
	return s, nil
}

// WriteFile writes s to path as indented JSON using an atomic rename.
// It is a package-level convenience wrapper around JSONFileWriter.
func WriteFile(path string, s Summary) error {
	return NewJSONFileWriter().Write(path, s)
}

// JSONFileWriter 는 요약을 JSON 파일로 기록함.
type JSONFileWriter struct{}

// NewJSONFileWriter 는 새로운 JSONFileWriter를 생성함.
func NewJSONFileWriter() *JSONFileWriter { return &JSONFileWriter{} }

// Write는 원자적 쓰기 내구성(fsync)을 위해 sync=true를 사용함.
func (w *JSONFileWriter) Write(path string, s Summary) error {
	if path == "" {
		// 출력 경로가 구성되지 않아 생략함
		return nil
	}
	return writeJSONAtomic(path, s, 0o644, 0o755, true)
}

// writeJSONAtomic은 JSON을 동일한 디렉터리의 임시 파일에 쓴 다음 이름을 변경함.
// - 원자적 교체는 os.Rename(동일 파일 시스템)에 의해 보장됨.
// - doSync가 true이면, 더 강력한 내구성을 위해 닫기 전에 임시 파일을 fsync 갱신함.
func writeJSONAtomic(path string, s Summary, fileMode, dirMode os.FileMode, doSync bool) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return err
	}

	f, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()

	success := false
	defer func() {
		if !success {
			_ = f.Close()
			_ = os.Remove(tmp)
		}
	}()

	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	if err := enc.Encode(s); err != nil {
		return err
	}

	if doSync {
		if err := f.Sync(); err != nil {
			return err
		}
	}

	if err := f.Close(); err != nil {
		return err
	}

	if err := os.Chmod(tmp, fileMode); err != nil {
		return err
	}

	if err := os.Rename(tmp, path); err != nil {
		return err
	}

	success = true
	return nil
}
