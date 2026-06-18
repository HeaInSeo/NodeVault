package podbridge5

import (
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"go.podman.io/podman/v6/pkg/bindings/images"
)

type imageExportRuntime interface {
	ExportImage(ctx context.Context, imageID string, writer io.Writer) error
}

type realImageExportRuntime struct{}

func (realImageExportRuntime) ExportImage(ctx context.Context, imageID string, writer io.Writer) error {
	exportOptions := &images.ExportOptions{}
	if err := images.Export(ctx, []string{imageID}, writer, exportOptions); err != nil {
		return fmt.Errorf("failed to export image %s: %w", imageID, err)
	}
	return nil
}

func imageArchivePath(basePath, imageName string, compress bool) string {
	extension := ".tar"
	if compress {
		extension = ".tar.gz"
	}
	baseImage := filepath.Base(imageName)
	safeImageName := strings.ReplaceAll(baseImage, ":", "-")
	archiveFileName := fmt.Sprintf("%s%s", safeImageName, extension)
	return filepath.Join(basePath, archiveFileName)
}

type exportWriteCloser struct {
	writer io.Writer
	close  func() error
}

func (w *exportWriteCloser) Close() error {
	if w == nil || w.close == nil {
		return nil
	}
	return w.close()
}

func prepareImageArchiveWriter(archivePath string, compress bool) (io.Writer, *exportWriteCloser, error) {
	dir := filepath.Dir(archivePath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, nil, fmt.Errorf("failed to create directory %s: %w", dir, err)
	}

	outputFile, err := os.OpenFile(archivePath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create output file %s: %w", archivePath, err)
	}

	if !compress {
		return outputFile, &exportWriteCloser{writer: outputFile, close: outputFile.Close}, nil
	}

	gzipWriter := gzip.NewWriter(outputFile)
	return gzipWriter, &exportWriteCloser{writer: gzipWriter, close: func() error {
		var closeErr error
		if err := gzipWriter.Close(); err != nil {
			closeErr = err
		}
		if err := outputFile.Close(); err != nil && closeErr == nil {
			closeErr = err
		}
		return closeErr
	}}, nil
}

func saveImageWithRuntime(ctx context.Context, runtime imageExportRuntime, path, imageName, imageID string, compress bool) error {
	archivePath := imageArchivePath(path, imageName, compress)
	writer, closer, err := prepareImageArchiveWriter(archivePath, compress)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := closer.Close(); closeErr != nil {
			Log.Warnf("Failed to close export writer: %v", closeErr)
		}
	}()

	if err := runtime.ExportImage(ctx, imageID, writer); err != nil {
		return fmt.Errorf("export image %q: %w", imageID, err)
	}
	return nil
}
