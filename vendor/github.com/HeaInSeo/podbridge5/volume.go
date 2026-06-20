package podbridge5

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"go.podman.io/podman/v6/pkg/bindings/containers"
	"go.podman.io/podman/v6/pkg/bindings/volumes"
	"go.podman.io/podman/v6/pkg/domain/entities/types"
	"go.podman.io/podman/v6/pkg/specgen"
)

// TODO RemoveVolume 테스트 진행해야 함. 코드 정리 필요.

type VolumeMode int

const (
	// ModeSkip 기존 데이터가 있으면 아무 작업도 하지 않고 바로 리턴
	ModeSkip VolumeMode = iota
	// ModeUpdate 기존 데이터를 유지하되, tar 안의 파일로 "업데이트"(덮어쓰기)만 수행
	ModeUpdate
	// ModeOverwrite 기존 볼륨을 완전 초기화(삭제 → 새로 생성)한 뒤 tar 를 풀어 씀
	ModeOverwrite
)

var (
	ErrVolumeNotFound = errors.New("volume not found")
)

type VolumeRemoveError struct {
	Name  string
	Cause error
}

type RemoveBehavior struct {
	Force          bool
	RetryForce     bool
	IgnoreNotFound bool
	Attempts       int
}

func (e *VolumeRemoveError) Error() string {
	return fmt.Sprintf("remove volume %s: %v", e.Name, e.Cause)
}
func (e *VolumeRemoveError) Unwrap() error { return e.Cause }

type CreateFn func(ctx context.Context, name string) (*types.VolumeConfigResponse, error)

func WithNamedVolume(volumeName, dest, subPath string, options ...string) ContainerOptions {
	return func(spec *specgen.SpecGenerator) error {
		cleaned := strings.TrimSpace(volumeName)
		if cleaned == "" {
			return fmt.Errorf("WithNamedVolume: empty volumeName for dest %s", dest)
		}

		for _, vol := range spec.Volumes {
			if vol.Dest == dest {
				if vol.Name != cleaned {
					return fmt.Errorf("WithNamedVolume: dest %s already mapped to %s", dest, vol.Name)
				}
				return nil
			}
		}
		// TODO Option 하고 SubPath 확인하자.
		spec.Volumes = append(spec.Volumes, &specgen.NamedVolume{
			Name:        cleaned,
			Dest:        dest,
			Options:     options,
			IsAnonymous: false,
			SubPath:     subPath, // 필요 없으면 먼저 제거해 테스트
		})
		return nil
	}
}

// TODO nfs, lustre 로 volume 을 원격지에 둘경우 대응해줘야 함. 지금은 local 만 해줌

// CreateVolume 주어진 볼륨 이름을 기반으로 볼륨 만들어줌. ignoreIfExists true 이면, 동일한 볼륨이 있으면 에러 리턴하지 않고 그대로 사용.
func CreateVolume(ctx context.Context, volumeName string, ignoreIfExists bool) (*types.VolumeConfigResponse, error) {
	volConfig := types.VolumeCreateOptions{
		Name:           volumeName,
		IgnoreIfExists: ignoreIfExists, // 만약 true 이면, 동일한 이름의 볼륨이 있으면 생성하지 않고 기존 볼륨을 사용
	}

	// CreateOptions 객체, 현재 버전에서는 빈 객체임.
	createOptions := &volumes.CreateOptions{}

	// volumes.Create 함수를 호출하여 볼륨 생성
	volumeResp, err := volumes.Create(ctx, volConfig, createOptions)
	if err != nil {
		return nil, fmt.Errorf("failed to create volume: %w", err)
	}

	return volumeResp, nil
}

// Deprecated: use RemoveVolume instead.
func DeleteVolume(ctx context.Context, volumeName string, force *bool) error {
	exists, err := volumes.Exists(ctx, volumeName, &volumes.ExistsOptions{})
	if err != nil {
		return fmt.Errorf("failed to check if volume %q exists: %w", volumeName, err)
	}
	if !exists {
		return fmt.Errorf("volume %q does not exist", volumeName)
	}

	opts := &volumes.RemoveOptions{
		Force: force,
	}

	if err := volumes.Remove(ctx, volumeName, opts); err != nil {
		return fmt.Errorf("failed to remove volume %q: %w", volumeName, err)
	}

	return nil
}

func RemoveVolume(ctx context.Context, name string, beh *RemoveBehavior) error {
	if beh == nil {
		beh = &RemoveBehavior{}
	}
	if beh.Attempts <= 0 {
		beh.Attempts = 1
	}

	tryRemove := func(force bool) error {
		return volumes.Remove(ctx, name, &volumes.RemoveOptions{Force: &force})
	}

	err := withRetry(ctx, beh.Attempts, 100*time.Millisecond, func() error {
		err := tryRemove(beh.Force)
		if err == nil {
			return nil
		}
		if beh.RetryForce && !beh.Force {
			// force 재시도
			ferr := tryRemove(true)
			if ferr == nil {
				return nil
			}
			return ferr
		}
		return err
	})
	if err != nil {
		if beh.IgnoreNotFound && isNotFoundErr(err) {
			return nil
		}
		return &VolumeRemoveError{Name: name, Cause: err}
	}
	return nil
}

func OverwriteVolume(ctx context.Context, name string, create CreateFn) (*types.VolumeConfigResponse, error) {
	exists, err := VolumeExists(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("check exists: %w", err)
	}
	if exists {
		if removeErr := RemoveVolume(ctx, name, &RemoveBehavior{
			Force:          false,
			RetryForce:     true,
			IgnoreNotFound: true,
			Attempts:       3,
		}); removeErr != nil {
			return nil, fmt.Errorf("remove before overwrite: %w", removeErr)
		}
	}
	vcr, err := create(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("create volume: %w", err)
	}
	return vcr, nil
}

func VolumeExists(ctx context.Context, name string) (bool, error) {
	var exists bool
	err := withRetry(ctx, 3, 80*time.Millisecond, func() error {
		e, err := volumes.Exists(ctx, name, nil)
		if err != nil {
			return fmt.Errorf("volumes.Exists: %w", err)
		}
		exists = e
		return nil
	})
	return exists, err
}

// WriteFolderToVolume TODO 일단 테스트 필요 일단 붙이면서 보자. 동시성 문제의 경우 ctx 관련해서 생각해보자. 중요.
// TODO 부가적으로 시간 또는 퍼센트를 나타내는 것을 추가할지 고민해야 함. 일단 합치는 것 부터 하고 나머지 진행하기로 함.
func WriteFolderToVolume(parentCtx context.Context, volumeName, mountPath, hostDir string, mode VolumeMode) error {
	if parentCtx == nil {
		return fmt.Errorf("WriteFolderToVolume: parentCtx must not be nil")
	}

	ctx, cancel := context.WithCancel(parentCtx)
	defer cancel()

	// 0. hostDir 검증
	st, err := os.Stat(hostDir)
	if err != nil {
		return fmt.Errorf("WriteFolderToVolume: stat hostDir: %w", err)
	}
	if !st.IsDir() {
		return fmt.Errorf("WriteFolderToVolume: hostDir is not directory: %s", hostDir)
	}

	var vcr *types.VolumeConfigResponse

	// 1. 한 번만 존재 여부 확인
	exists, err := VolumeExists(ctx, volumeName)
	if err != nil {
		return fmt.Errorf("WriteFolderToVolume: check volume existence: %w", err)
	}

	action, err := planVolumeSetup(mode, exists)
	if err != nil {
		return fmt.Errorf("WriteFolderToVolume: volume setup plan: %w", err)
	}

	switch action {
	case volumeSetupActionSkip:
		return nil
	case volumeSetupActionCreate:
		vcr, err = CreateVolume(ctx, volumeName, false)
	case volumeSetupActionReuse:
		vcr = &types.VolumeConfigResponse{}
		vcr.Name = volumeName
	case volumeSetupActionOverwrite:
		vcr, err = OverwriteVolume(ctx, volumeName, func(c context.Context, n string) (*types.VolumeConfigResponse, error) {
			return CreateVolume(c, n, false)
		})
	default:
		return fmt.Errorf("WriteFolderToVolume: unsupported volume setup action: %d", action)
	}

	if err != nil {
		return fmt.Errorf("WriteFolderToVolume: volume setup: %w", err)
	}

	// 1. 임시 컨테이너 spec
	spec, err := newVolumeWriterSpec(vcr.Name, mountPath)
	if err != nil {
		return fmt.Errorf("WriteFolderToVolume: build container spec: %w", err)
	}

	runtime := podmanVolumeContainerRuntime{}
	containerID, cleanup, err := startVolumeContainer(ctx, runtime, spec)
	if err != nil {
		return fmt.Errorf("WriteFolderToVolume: %w", err)
	}
	defer cleanup()

	// 4. tar 스트리밍 (WalkDir)
	pr, pw := io.Pipe()
	var wg sync.WaitGroup
	wg.Add(1)
	// TODO tar 를 스트림으로 작성해줌. header 관련해서즌 좀 공부좀 하자. pr, pw 개념은 이해했는데 좀더 숙달하자.
	go func() {
		defer wg.Done()

		tw := tar.NewWriter(pw)
		var walkErr error
		// TODO (지우지 말것) defer 의 경우 LIFO 방식으로 실행되므로, tw.Close() 가 나중에 실행되고, pw.Close 가 나중에 실행됨.
		// tw.Close()가 먼저 호출되어 tar footer가 정상 쓰여지고, 그다음에 pw.Close()가 파이프를 닫으므로 에러가 발생하지 않는다.
		defer func() {
			if walkErr != nil {
				// WalkDir 중 에러가 발생했음을 consumer에 전달
				if cErr := pw.CloseWithError(walkErr); cErr != nil {
					// 실제 CloseWithError 자체 실패도 함께 로깅
					Log.Warnf("pipe writer: forwarded walk error (%v), but CloseWithError failed: %v", walkErr, cErr)
				} else {
					// 정상적으로 walkErr를 전달했을 때
					Log.Errorf("pipe writer closed with walk error: %v", walkErr)
				}
			} else {
				// 정상 종료
				if cErr2 := pw.Close(); cErr2 != nil {
					Log.Warnf("pipe writer close failed: %v", cErr2)
				} else {
					// TODO 향후 Log.Debug 바꾸자. 지금은 이렇게 남겨놓는다.
					Log.Info("pipe writer closed cleanly")
				}
			}
		}()

		defer func() {
			if cErr := tw.Close(); cErr != nil {
				Log.Warnf("tar writer close: %v", cErr)
			}
		}()

		walkErr = filepath.WalkDir(hostDir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, relErr := filepath.Rel(hostDir, path)
			if relErr != nil {
				return fmt.Errorf("relative path %q: %w", path, relErr)
			}
			if rel == "." {
				return nil
			}

			fi, fiErr := d.Info()
			if fiErr != nil {
				return fmt.Errorf("file info %q: %w", path, fiErr)
			}

			var linkTarget string
			// fi.Mode() 의 비트 패턴 중에서 os.ModeSymlink 플래그(심볼릭 링크를 나타내는 위치에 있는 비트)만 남겨내서(AND 연산 & 연산)
			// 그 결과가 0이 아니면 "이 파일이 심볼릭 링크다" 라고 판단할 수 있다.
			if fi.Mode()&os.ModeSymlink != 0 {
				// 심볼릭 링크의 실제 패스를 가져옴.
				if lt, lerr := os.Readlink(path); lerr == nil {
					linkTarget = lt
				} else {
					return fmt.Errorf("readlink %q: %w", path, lerr)
				}
			}

			hdr, hdrErr := tar.FileInfoHeader(fi, linkTarget)
			if hdrErr != nil {
				return fmt.Errorf("tar header %q: %w", path, hdrErr)
			}
			hdr.Name = rel

			if d.IsDir() {
				if err := tw.WriteHeader(hdr); err != nil {
					return fmt.Errorf("write dir header %q: %w", rel, err)
				}
				return nil
			}

			if err := tw.WriteHeader(hdr); err != nil {
				return fmt.Errorf("write header %q: %w", rel, err)
			}

			if fi.Mode().IsRegular() {
				f, openErr := os.Open(path)
				if openErr != nil {
					return fmt.Errorf("open file %q: %w", path, openErr)
				}
				_, cErr := io.Copy(tw, f)
				_ = f.Close()
				if cErr != nil {
					return fmt.Errorf("copy file %q: %w", path, cErr)
				}
			}
			return nil
		})
	}()

	// 5. tar -> 컨테이너 (mountPath), TODO CopyFromArchiveWithOptions 마지막 옵션에서 nil 써서 디폴트로 사용했는데, 이건 위의 내용 살펴봐서 CopyFromArchive 쓸지 고려해보자.
	copyFunc, err := containers.CopyFromArchiveWithOptions(ctx, containerID, mountPath, pr, nil)
	if err != nil {
		cancel()
		_ = pr.CloseWithError(err)
		_ = pw.CloseWithError(err)
		wg.Wait()
		return fmt.Errorf("WriteFolderToVolume: init copy: %w", err)
	}
	if err := copyFunc(); err != nil {
		cancel()
		_ = pr.CloseWithError(err)
		_ = pw.CloseWithError(err)
		wg.Wait()
		return fmt.Errorf("WriteFolderToVolume: copy archive: %w", err)
	}

	wg.Wait()

	// 6. 안정성 대기 (옵션)
	time.Sleep(500 * time.Millisecond)
	return nil
}

// ReadDataFromVolume TODO 이거 생각해보자. 필요한지
func ReadDataFromVolume(ctx context.Context, volumeName, mountPath, fileName string) (string, error) {
	// 1. Build the container specification.
	spec, err := newVolumeReaderSpec(volumeName, mountPath)
	if err != nil {
		return "", fmt.Errorf("failed to build container spec: %w", err)
	}

	runtime := podmanVolumeContainerRuntime{}
	containerID, cleanup, err := startVolumeContainer(ctx, runtime, spec)
	if err != nil {
		return "", fmt.Errorf("failed to start temporary container: %w", err)
	}
	defer cleanup()

	// 5. Build the full file path inside the container.
	fullPath := mountPath + "/" + fileName

	// Prepare a buffer to receive the tar archive.
	var outBuf bytes.Buffer

	// 6. Use the CopyToArchive API to fetch the file as a tar archive.
	copyFunc, err := containers.CopyToArchive(ctx, containerID, fullPath, &outBuf)
	if err != nil {
		return "", fmt.Errorf("failed to initialize copy from container: %w", err)
	}
	if err := copyFunc(); err != nil {
		return "", fmt.Errorf("failed during copy from container: %w", err)
	}

	// 7. Extract file content from the tar archive.
	tr := tar.NewReader(&outBuf)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break // 아카이브의 끝에 도달
		}
		if err != nil {
			return "", fmt.Errorf("failed to read tar archive: %w", err)
		}
		// tar 헤더의 Name이 파일 이름과 일치하면 파일 내용을 읽음.
		if hdr.Name == fileName {
			var fileBuf bytes.Buffer
			if _, err := io.Copy(&fileBuf, tr); err != nil {
				return "", fmt.Errorf("failed to read file data from tar: %w", err)
			}
			return fileBuf.String(), nil
		}
	}

	return "", fmt.Errorf("file %q not found in tar archive", fileName)
}

func isNotFoundErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	// Podman error 문자열 패턴 기반 (필요 시 개선)
	if strings.Contains(msg, "404") || strings.Contains(msg, "not found") {
		return true
	}
	return false
}

func withRetry(ctx context.Context, attempts int, baseDelay time.Duration, fn func() error) error {
	if attempts <= 0 {
		attempts = 1
	}
	delay := baseDelay
	var err error
	for i := 0; i < attempts; i++ {
		if ctx.Err() != nil {
			return fmt.Errorf("retry: %w", ctx.Err())
		}
		err = fn()
		if err == nil {
			return nil
		}
		if i == attempts-1 {
			break
		}
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return fmt.Errorf("retry: %w", ctx.Err())
		}
		if delay < 2*time.Second {
			delay *= 2
		}
	}
	return err
}
