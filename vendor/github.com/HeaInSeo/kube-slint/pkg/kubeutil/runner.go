package kubeutil

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/HeaInSeo/kube-slint/pkg/slo"
)

// CmdRunner abstracts command execution (stdout-only on success).
// CmdRunner는 명령어 실행을 추상화합니다 (성공 시 stdout만 반환).
type CmdRunner interface {
	Run(ctx context.Context, logger slo.Logger, cmd *exec.Cmd) (string, error)
}

// DefaultRunner executes commands and returns stdout.
// On error, includes stderr+stdout in the returned error.
// DefaultRunner는 명령어를 실행하고 stdout을 반환합니다.
// 에러 발생 시, 반환된 에러에 stderr+stdout이 포함됩니다.
type DefaultRunner struct{}

// Run executes the command and returns stdout.
// Run은 명령어를 실행하고 stdout을 반환합니다.
func (DefaultRunner) Run(ctx context.Context, logger slo.Logger, cmd *exec.Cmd) (string, error) {
	logger = slo.NewLogger(logger)

	// Ensure ctx cancellation works even if the caller constructed cmd without context.
	// We rebuild the command using exec.CommandContext but preserve args, dir, stdin.
	// Note: If cmd.Path is empty, cmd.Args[0] is used; but normally exec.Command sets Path.
	// 컨텍스트 취소(cancellation)가 호출자가 컨텍스트 없이 cmd를 생성했더라도 작동하도록 보장합니다.
	// exec.CommandContext를 사용하여 명령어를 다시 빌드하되 args, dir, stdin은 보존합니다.
	// 참고: cmd.Path가 비어있으면 cmd.Args[0]을 사용하지만, 보통 exec.Command가 Path를 설정합니다.
	path := cmd.Path
	// defensively handle path being empty -> use first arg as path
	// 방어적 코드: path가 비어있으면 -> 첫 번째 arg를 path로 사용
	if path == "" && len(cmd.Args) > 0 {
		path = cmd.Args[0]
	}
	var args []string
	if len(cmd.Args) > 1 {
		args = cmd.Args[1:]
	}
	// time out or cancel via ctx
	// ctx를 통한 타임아웃 또는 취소
	c2 := exec.CommandContext(ctx, path, args...)
	c2.Dir = cmd.Dir
	c2.Stdin = cmd.Stdin
	c2.Env = cmd.Env
	if len(c2.Env) == 0 {
		c2.Env = append(os.Environ(), "GO111MODULE=on")
	} else {
		c2.Env = append(c2.Env, "GO111MODULE=on")
	}

	command := strings.Join(c2.Args, " ")
	logger.Logf("running: %q", command)

	// TODO 왜 이렇게 했는지, 혹시 문제가 발생한다면 어떠한 문제가 발생할 수 있는지 스터디
	// var stdout, stderr bytes.Buffer
	// bytes.Buffer 대신 strings.Builder 사용했다, 그 이유는 메모리 최적화 때문이다.
	var stdout, stderr strings.Builder
	c2.Stdout = &stdout
	c2.Stderr = &stderr

	err := c2.Run()
	outStr := stdout.String()
	errStr := stderr.String()

	if err != nil {
		combined := strings.TrimSpace(errStr + "\n" + outStr)
		return outStr, fmt.Errorf("%q failed: %s: %w", command, combined, err)
	}
	return outStr, nil
}
