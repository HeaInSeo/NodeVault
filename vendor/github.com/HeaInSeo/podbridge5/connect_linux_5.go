// This file now only builds on Linux.
//go:build linux
// +build linux

package podbridge5

import (
	"context"
	"errors"
	"os"

	"github.com/HeaInSeo/utils"
	"github.com/containers/podman/v5/pkg/bindings"
	"github.com/containers/storage/pkg/unshare"
)

func NewConnection5(ctx context.Context, ipcName string) (context.Context, error) {
	if utils.IsEmptyString(ipcName) {
		Log.Error("ipcName cannot be an empty string")
		return nil, errors.New("ipcName cannot be an empty string")
	}
	ctx, err := bindings.NewConnection(ctx, ipcName)

	return ctx, err
}

func NewConnectionLinux5(ctx context.Context) (context.Context, error) {
	target := currentRuntimeConnectionTarget()
	ctx, err := bindings.NewConnection(ctx, target.URI)
	if err != nil {
		return nil, wrapRuntimeConnectionError(target, err)
	}

	return ctx, nil
}

func currentRuntimeConnectionTarget() runtimeConnectionTarget {
	return resolveRuntimeConnectionTarget(
		os.Getuid(),
		os.Getenv("CONTAINER_HOST"),
		os.Getenv("XDG_RUNTIME_DIR"),
		unshare.IsRootless(),
	)
}
