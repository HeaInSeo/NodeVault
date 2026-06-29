// Package cachegc implements high-watermark-based eviction for the
// NodeVault package cache PVC (CONDA_PKGS_DIRS / MAMBA_PKG_CACHE).
//
// The GC runs as a background loop. When total disk usage of the cache
// directory exceeds HighWatermarkMiB it evicts the oldest (by mtime)
// top-level entries — package directories and tarballs — until usage
// drops to 80 % of the watermark.
package cachegc

import (
	"context"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"
)

const (
	defaultHighWatermarkMiB = 10240 // 10 GiB
	defaultInterval         = 30 * time.Minute
	evictToFraction         = 80 // evict down to 80 % of watermark
)

// Config holds tunable parameters for the package cache GC.
type Config struct {
	// Dir is the root of the conda/mamba package cache directory.
	// Defaults to NODEVAULT_PKG_CACHE_DIR → CONDA_PKGS_DIRS → /var/cache/nodevault/packages/conda.
	Dir string

	// HighWatermarkMiB is the disk usage threshold that triggers eviction.
	// Set via NODEVAULT_PKG_CACHE_HIGH_WATERMARK_MIB. Default: 10240 (10 GiB).
	HighWatermarkMiB int64

	// Interval is how often the GC loop checks disk usage.
	// Set via NODEVAULT_PKG_CACHE_GC_INTERVAL (e.g. "15m"). Default: 30m.
	Interval time.Duration
}

// DefaultConfig returns a Config populated from environment variables.
func DefaultConfig() Config {
	dir := envOr("NODEVAULT_PKG_CACHE_DIR",
		envOr("CONDA_PKGS_DIRS", "/var/cache/nodevault/packages/conda"))
	return Config{
		Dir:              dir,
		HighWatermarkMiB: envInt64("NODEVAULT_PKG_CACHE_HIGH_WATERMARK_MIB", defaultHighWatermarkMiB),
		Interval:         envDuration("NODEVAULT_PKG_CACHE_GC_INTERVAL", defaultInterval),
	}
}

// GC evicts old package cache entries when disk usage exceeds HighWatermarkMiB.
type GC struct {
	cfg Config
}

// New creates a GC with the given Config.
func New(cfg Config) *GC {
	return &GC{cfg: cfg}
}

// Run starts the GC loop. It blocks until ctx is canceled.
func (g *GC) Run(ctx context.Context) {
	if g.cfg.Dir == "" || g.cfg.HighWatermarkMiB <= 0 {
		return
	}
	ticker := time.NewTicker(g.cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := g.RunOnce(); err != nil {
				slog.Error("package cache GC error", "err", err)
			}
		}
	}
}

// RunOnce performs a single GC pass. Exported for testing and operator triggers.
func (g *GC) RunOnce() error {
	usageMiB, err := DirUsageMiB(g.cfg.Dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // cache dir not yet created — nothing to do
		}
		return err
	}

	if usageMiB <= g.cfg.HighWatermarkMiB {
		slog.Debug("package cache under watermark", "usage_mib", usageMiB, "watermark_mib", g.cfg.HighWatermarkMiB)
		return nil
	}

	slog.Info("package cache over high watermark, evicting",
		"usage_mib", usageMiB, "watermark_mib", g.cfg.HighWatermarkMiB)

	entries, err := topLevelEntries(g.cfg.Dir)
	if err != nil {
		return err
	}

	// Oldest mtime first — best proxy for LRU in conda/mamba cache.
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].modTime.Before(entries[j].modTime)
	})

	targetMiB := g.cfg.HighWatermarkMiB * evictToFraction / 100
	freed := int64(0)
	evicted := 0

	for _, e := range entries {
		if usageMiB-freed <= targetMiB {
			break
		}
		if err := os.RemoveAll(e.path); err != nil {
			slog.Warn("failed to evict cache entry", "path", e.path, "err", err)
			continue
		}
		freed += e.sizeMiB
		evicted++
		slog.Info("evicted package cache entry", "path", filepath.Base(e.path), "size_mib", e.sizeMiB)
	}

	slog.Info("package cache GC complete",
		"evicted", evicted, "freed_mib", freed, "remaining_mib", usageMiB-freed)
	return nil
}

// DirUsageMiB returns the total disk usage of dir in MiB (rounds down).
// Returns 0 and a nil error when dir does not exist.
func DirUsageMiB(dir string) (int64, error) {
	var totalBytes int64
	err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable entries
		}
		if d.IsDir() {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			return nil
		}
		totalBytes += info.Size()
		return nil
	})
	if os.IsNotExist(err) {
		return 0, nil
	}
	return totalBytes / (1024 * 1024), err
}

type entry struct {
	path    string
	sizeMiB int64
	modTime time.Time
}

// topLevelEntries lists the immediate children of dir with their sizes.
// Directories are measured recursively; files are measured directly.
func topLevelEntries(dir string) ([]entry, error) {
	des, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	result := make([]entry, 0, len(des))
	for _, de := range des {
		path := filepath.Join(dir, de.Name())
		info, err := de.Info()
		if err != nil {
			continue
		}
		sizeMiB := int64(0)
		if de.IsDir() {
			sizeMiB, _ = DirUsageMiB(path)
		} else {
			sizeMiB = info.Size() / (1024 * 1024)
		}
		result = append(result, entry{
			path:    path,
			sizeMiB: sizeMiB,
			modTime: info.ModTime(),
		})
	}
	return result, nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envInt64(key string, fallback int64) int64 {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			return n
		}
	}
	return fallback
}

func envDuration(key string, fallback time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return fallback
}
