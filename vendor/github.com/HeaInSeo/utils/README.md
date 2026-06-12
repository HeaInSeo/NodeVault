# utils

다른 프로젝트에서 공통으로 사용하는 Go 유틸리티 패키지입니다.  
외부 의존성 없이 표준 라이브러리만 사용합니다.

---

## 패키지 구성

| 패키지 | 경로 | 설명 |
|---|---|---|
| `utils` | `/` | 문자열, 파일, 슬라이스 등 범용 유틸리티 |
| `shellexecmd` | `/shellexecmd` | 쉘 명령 실행 및 실시간 출력 스트리밍 |

---

## utils 패키지

### 주요 함수

```go
// 문자열이 비어 있거나 공백만 있으면 true
IsEmptyString(s string) bool

// 파일 경로 유효성 검사 및 정규화
CheckPath(filePath string) (string, error)

// JSON 직렬화를 이용한 구조체 깊은 복사
DeepCopy(dst interface{}, src interface{}) error

// 파일 존재 여부 확인. 없으면 (false, nil, nil), 있으면 (true, FileInfo, nil)
FileExists(path string) (bool, os.FileInfo, error)

// 파일 내용을 0바이트로 초기화
Truncate(path string) error

// 채널 슬라이스에서 i번째 요소 제거 (순서 유지)
Remove(ss []chan interface{}, i int) []chan interface{}

// 문자열 슬라이스에 item이 포함되어 있는지 확인
Contains(slice []string, item string) bool

// fileName이 exclusions 목록에 있는지 확인
ExcludeFiles(fileName string, exclusions []string) bool
```

### 로거

```go
// 표준 라이브러리 log/slog 기반 구조화 로거
// 필요하면 커스텀 *slog.Logger로 교체 가능
utils.Log.Info("message", "key", "value")
```

---

## shellexecmd 패키지

### 주요 함수

```go
// 쉘 명령을 실행하고 stdout을 한 줄씩 채널로 스트리밍.
// 명령 종료 시 채널이 닫힌다.
Run(ctx context.Context, s string) (<-chan string, error)

// Run을 래핑해 각 출력 줄을 콘솔에 출력하고 성공 여부를 반환.
Runner(ctx context.Context, s string) bool
```

### 사용 예시

```go
ctx := context.Background()

// 출력을 직접 처리할 때
ch, err := shellexecmd.Run(ctx, `echo "hello" && echo "world"`)
if err != nil {
    log.Fatal(err)
}
for line := range ch {
    fmt.Println(line)
}

// 단순 실행만 할 때
ok := shellexecmd.Runner(ctx, "./deploy.sh")
if !ok {
    log.Fatal("script failed")
}

// 타임아웃 설정
ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
defer cancel()
shellexecmd.Runner(ctx, "./long-running.sh")
```

---

## 설치 및 사용

```bash
go get github.com/HeaInSeo/utils
```

```go
import (
    "github.com/HeaInSeo/utils"
    "github.com/HeaInSeo/utils/shellexecmd"
)
```

---

## git 태그 관리

```bash
# 태그 목록 확인
git tag

# 새 태그 생성
git tag -a v1.0.0 -m "Release v1.0.0"

# 태그 푸시 (태그를 명시해야 함)
git push origin v1.0.0
```
