package podbridge5

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/HeaInSeo/utils"
)

// GenerateExecutor path 생성될 executor.sh 의 path, fileName "executor.sh", userScriptPath 컨테이너내에서 executor.sh 가 실행 할 user_script.sh 의 위치
func GenerateExecutor(path, fileName, userScriptPath string) (*os.File, *string, error) {
	if utils.IsEmptyString(path) || utils.IsEmptyString(fileName) {
		return nil, nil, fmt.Errorf("path or file name is empty")
	}

	// Ensure the directory exists.
	if _, err := os.Stat(path); os.IsNotExist(err) {
		if err := os.MkdirAll(path, 0o755); err != nil {
			return nil, nil, fmt.Errorf("failed to create directory: %w", err)
		}
	}

	executorPath := filepath.Join(path, fileName)
	tmpFilePath := filepath.Join(path, fileName+".tmp")

	// Create temp file
	tmpFile, err := os.Create(tmpFilePath)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create temporary file: %w", err)
	}

	// Write the new executor script
	scriptContent := fmt.Sprintf(`#!/usr/bin/env bash
set -euo pipefail

RESULT_LOG="%s"
STATUS_LOG="%s"
LOCK_FILE="${STATUS_LOG}.lock"

# 로그 초기화
: > "$RESULT_LOG"

# 상태 기록 함수 (실패 시 바로 호출)
record_failure() {
    local code=$1
    echo "exit_code:${code}" > "${STATUS_LOG}.tmp"
    {
        flock -x 200
        mv "${STATUS_LOG}.tmp" "${STATUS_LOG}"
    } 200> "$LOCK_FILE"
    echo "Syntax error detected, exit_code=${code}" | tee -a "$RESULT_LOG"
    exit "${code}"
}

# 1) %s 존재 및 문법 검사
if [[ ! -f "%s" ]]; then
    echo "Error: %s not found" | tee -a "$RESULT_LOG"
    record_failure 1
fi

if ! bash -n "%s"; then
    echo "Syntax error in %s" | tee -a "$RESULT_LOG"
    record_failure 1
fi

# 2) 실제 실행 및 정상 흐름
bash "%s" 2>&1 | tee -a "$RESULT_LOG"
EXIT_CODE=${PIPESTATUS[0]}

# 3) 상태 기록 (정상/실패 공통)
echo "exit_code:${EXIT_CODE}" > "${STATUS_LOG}.tmp"
{
    flock -x 200
    mv "${STATUS_LOG}.tmp" "${STATUS_LOG}"
} 200> "$LOCK_FILE"

# 4) 최종 로그
if (( EXIT_CODE != 0 )); then
    echo "Task failed with exit code ${EXIT_CODE}" | tee -a "$RESULT_LOG"
else
    echo "Task completed successfully" | tee -a "$RESULT_LOG"
fi

exit "${EXIT_CODE}"`, ContainerResultLogPath, ContainerExitCodeLogPath, userScriptPath, userScriptPath, userScriptPath, userScriptPath, userScriptPath, userScriptPath)

	if _, writeErr := tmpFile.WriteString(scriptContent); writeErr != nil {
		if closeErr := tmpFile.Close(); closeErr != nil {
			Log.Errorf("failed to close temporary file after write error: %v", closeErr)
		}
		return nil, nil, fmt.Errorf("failed to write script content to temporary file: %w", writeErr)
	}

	if syncErr := tmpFile.Sync(); syncErr != nil {
		if closeErr := tmpFile.Close(); closeErr != nil {
			Log.Errorf("failed to close temporary file after sync error: %v", closeErr)
		}
		return nil, nil, fmt.Errorf("failed to sync temporary file: %w", syncErr)
	}
	if closeErr := tmpFile.Close(); closeErr != nil {
		return nil, nil, fmt.Errorf("failed to close temporary file: %w", closeErr)
	}

	// Compare with existing file if present
	exists, _, existsErr := utils.FileExists(executorPath)
	if existsErr != nil {
		return nil, nil, fmt.Errorf("failed to check if original file exists: %w", existsErr)
	}
	if exists {
		same, compareErr := compareFiles(executorPath, tmpFilePath)
		if compareErr != nil {
			return nil, nil, fmt.Errorf("failed to compare files: %w", compareErr)
		}
		if same {
			if removeErr := os.Remove(tmpFilePath); removeErr != nil {
				Log.Errorf("failed to remove temporary file %s: %v", tmpFilePath, removeErr)
			}
			return nil, &executorPath, nil
		}
	}

	// Rename temp to final
	if err = os.Rename(tmpFilePath, executorPath); err != nil {
		return nil, nil, fmt.Errorf("failed to rename temporary file to final file: %w", err)
	}

	// Set execute permissions
	if err = os.Chmod(executorPath, 0o755); err != nil {
		return nil, nil, fmt.Errorf("failed to set file permissions: %w", err)
	}

	// Open final file for return
	finalFile, err := os.Open(executorPath)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to open final executor file: %w", err)
	}

	return finalFile, &executorPath, nil
}

// compareFiles 두 파일의 내용을 비교하는 함수
func compareFiles(file1, file2 string) (bool, error) {
	f1, err := os.Open(file1)
	if err != nil {
		return false, fmt.Errorf("open %q: %w", file1, err)
	}
	defer func() {
		if closeErr := f1.Close(); closeErr != nil {
			Log.Errorf("Failed to close file %s: %v", file1, closeErr)
		}
	}()

	f2, err := os.Open(file2)
	if err != nil {
		return false, fmt.Errorf("open %q: %w", file2, err)
	}
	defer func() {
		if closeErr := f2.Close(); closeErr != nil {
			Log.Errorf("Failed to close file %s: %v", file2, closeErr)
		}
	}()

	f1Stat, err := f1.Stat()
	if err != nil {
		return false, fmt.Errorf("stat %q: %w", file1, err)
	}

	f2Stat, err := f2.Stat()
	if err != nil {
		return false, fmt.Errorf("stat %q: %w", file2, err)
	}

	// 파일 크기가 다르면 내용이 다르다고 간주
	if f1Stat.Size() != f2Stat.Size() {
		return false, nil
	}

	// 1024바이트 단위로 읽어 파일 내용을 비교
	buf1 := make([]byte, 1024)
	buf2 := make([]byte, 1024)

	for {
		n1, err1 := f1.Read(buf1)
		n2, err2 := f2.Read(buf2)

		if err1 != nil && err1 != io.EOF {
			return false, fmt.Errorf("read %q: %w", file1, err1)
		}
		if err2 != nil && err2 != io.EOF {
			return false, fmt.Errorf("read %q: %w", file2, err2)
		}

		if n1 != n2 || !bytes.Equal(buf1[:n1], buf2[:n1]) {
			return false, nil
		}

		if err1 == io.EOF && err2 == io.EOF {
			break
		}
	}

	return true, nil
}

// ProcessScript use_script.sh 만들어 주는 메서드
func ProcessScript(scriptContent, path string) (string, error) {
	checkedPath, checkErr := utils.CheckPath(path)
	if checkErr != nil {
		return "", fmt.Errorf("invalid path: %w", checkErr)
	}
	path = checkedPath
	// 디렉토리가 존재하지 않으면 생성
	if _, err := os.Stat(path); os.IsNotExist(err) {
		if err := os.MkdirAll(path, 0o755); err != nil {
			return "", fmt.Errorf("failed to create directory: %w", err)
		}
	}

	// 스크립트 내용의 앞뒤 공백 및 들여쓰기 제거
	// scriptContent = ensureShebang(scriptContent)

	// 받은 스크립트를 텍스트 파일로 저장 (보관용)
	txtFilePath := filepath.Join(path, "user_script.txt")
	if err := os.WriteFile(txtFilePath, []byte(scriptContent), 0o644); err != nil {
		return "", fmt.Errorf("failed to write script content to txt file: %w", err)
	}

	// 임시 파일은 목적지와 같은 디렉터리에 생성해야 os.Rename이 EXDEV 없이 작동
	tmpFile, err := os.CreateTemp(path, "user_script_*.sh")
	if err != nil {
		return "", fmt.Errorf("failed to create temp file: %w", err)
	}

	defer func() {
		if removeErr := os.Remove(tmpFile.Name()); removeErr != nil && !os.IsNotExist(removeErr) {
			Log.Errorf("Failed to remove temporary file %s: %v", tmpFile.Name(), removeErr)
		}
	}()

	// 쉘 스크립트 내용을 임시 파일에 씀
	if _, err = tmpFile.WriteString(scriptContent); err != nil {
		return "", fmt.Errorf("failed to write script content to temp file: %w", err)
	}

	// 파일을 닫고 저장
	if err = tmpFile.Close(); err != nil {
		return "", fmt.Errorf("failed to close temp file: %w", err)
	}

	// 문법 검사 수행
	if err = exec.CommandContext(context.Background(), "bash", "-n", tmpFile.Name()).Run(); err != nil {
		// 문법 오류가 있으면 .sh 파일을 남기지 않고 에러 반환
		return "", fmt.Errorf("syntax error in user script: %w", err)
	}

	// 문법 검사가 통과되었으므로 임시 파일을 최종 위치로 이동
	shFilePath := filepath.Join(path, "user_script.sh")
	if err = os.Rename(tmpFile.Name(), shFilePath); err != nil {
		return "", fmt.Errorf("failed to move temp file to final location: %w", err)
	}

	if err = os.Chmod(shFilePath, 0o755); err != nil {
		return "", fmt.Errorf("failed to set file permissions: %w", err)
	}

	return shFilePath, nil
}
