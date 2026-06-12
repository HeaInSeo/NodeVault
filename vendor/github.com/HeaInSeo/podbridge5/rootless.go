package podbridge5

import (
	"context"
	"errors"
	"sync"

	"github.com/containers/buildah"
	"github.com/containers/storage"
	"github.com/containers/storage/pkg/unshare"
)

var (
	initMu      sync.Mutex
	pbStore     storage.Store
	pbCtx       context.Context
	initialized bool
)

func Init() error {
	_, err := initRuntime(context.Background())
	return err
}

func InitWithContext(ctx context.Context) (context.Context, error) {
	return initRuntime(ctx)
}

func initRuntime(ctx context.Context) (context.Context, error) {
	initMu.Lock()
	defer initMu.Unlock()

	if initialized {
		return pbCtx, nil
	}

	connCtx, err := NewConnectionLinux5(ctx)
	if err != nil {
		return nil, err
	}

	store, err := NewStore()
	if err != nil {
		return nil, wrapRuntimeStoreError(err)
	}

	pbCtx = connCtx
	pbStore = store
	initialized = true

	return pbCtx, nil
}

func Shutdown() error {
	initMu.Lock()
	defer initMu.Unlock()

	if !initialized || pbStore == nil {
		return ErrRuntimeNotInitialized
	}
	if err := shutdown(pbStore, false); err != nil {
		return errors.Join(ErrRuntimeNotInitialized, err)
	}

	pbStore = nil
	pbCtx = nil
	initialized = false
	return nil
}

func ReexecIfNeeded() bool {
	if buildah.InitReexec() {
		return true
	}
	unshare.MaybeReexecUsingUserNamespace(false)
	return false
}
