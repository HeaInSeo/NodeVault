package cachegc_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/HeaInSeo/NodeVault/pkg/cachegc"
)

// writeFakePackage creates a fake conda package entry (dir or file) under root
// with the given size in bytes and modification time.
func writeFakePackage(t *testing.T, root, name string, sizeBytes int, modTime time.Time, isDir bool) {
	t.Helper()
	path := filepath.Join(root, name)
	if isDir {
		if err := os.MkdirAll(path, 0o750); err != nil {
			t.Fatalf("mkdir %s: %v", path, err)
		}
		// Write a single file inside the dir to give it the requested size.
		f := filepath.Join(path, "package.tar.bz2")
		if err := os.WriteFile(f, make([]byte, sizeBytes), 0o600); err != nil {
			t.Fatalf("write %s: %v", f, err)
		}
		if err := os.Chtimes(f, modTime, modTime); err != nil {
			t.Fatalf("chtimes %s: %v", f, err)
		}
	} else {
		if err := os.WriteFile(path, make([]byte, sizeBytes), 0o600); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	if err := os.Chtimes(path, modTime, modTime); err != nil {
		t.Fatalf("chtimes %s: %v", path, err)
	}
}

// TestDirUsageMiB_EmptyDir returns 0 for an empty directory.
func TestDirUsageMiB_EmptyDir(t *testing.T) {
	dir := t.TempDir()
	got, err := cachegc.DirUsageMiB(dir)
	if err != nil {
		t.Fatalf("DirUsageMiB: %v", err)
	}
	if got != 0 {
		t.Errorf("empty dir: got %d MiB, want 0", got)
	}
}

// TestDirUsageMiB_NonExistentDir returns 0 without error.
func TestDirUsageMiB_NonExistentDir(t *testing.T) {
	got, err := cachegc.DirUsageMiB(filepath.Join(t.TempDir(), "nonexistent"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 0 {
		t.Errorf("nonexistent dir: got %d MiB, want 0", got)
	}
}

// TestDirUsageMiB_Counts sums file sizes across subdirectories.
func TestDirUsageMiB_Counts(t *testing.T) {
	dir := t.TempDir()
	// 3 MiB in a subdirectory + 2 MiB flat file = 5 MiB total.
	now := time.Now()
	writeFakePackage(t, dir, "pkg-a", 3*1024*1024, now, true)
	writeFakePackage(t, dir, "pkg-b.tar.bz2", 2*1024*1024, now, false)

	got, err := cachegc.DirUsageMiB(dir)
	if err != nil {
		t.Fatalf("DirUsageMiB: %v", err)
	}
	if got != 5 {
		t.Errorf("got %d MiB, want 5", got)
	}
}

// TestRunOnce_UnderWatermark does not evict anything.
func TestRunOnce_UnderWatermark(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	writeFakePackage(t, dir, "pkg-small", 1*1024*1024, now, true) // 1 MiB

	gc := cachegc.New(cachegc.Config{
		Dir:              dir,
		HighWatermarkMiB: 100,
		Interval:         time.Hour,
	})
	if err := gc.RunOnce(); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	// Entry must still exist.
	if _, err := os.Stat(filepath.Join(dir, "pkg-small")); err != nil {
		t.Errorf("pkg-small was evicted but should not have been: %v", err)
	}
}

// TestRunOnce_EvictsOldestFirst removes oldest entries until under target.
func TestRunOnce_EvictsOldestFirst(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	// Three 3-MiB packages; total = 9 MiB. Watermark = 5 MiB → must evict ≥1.
	writeFakePackage(t, dir, "pkg-old", 3*1024*1024, base, true)
	writeFakePackage(t, dir, "pkg-mid", 3*1024*1024, base.Add(time.Hour), true)
	writeFakePackage(t, dir, "pkg-new", 3*1024*1024, base.Add(2*time.Hour), true)

	gc := cachegc.New(cachegc.Config{
		Dir:              dir,
		HighWatermarkMiB: 5,
		Interval:         time.Hour,
	})
	if err := gc.RunOnce(); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	// pkg-old must be evicted (oldest); pkg-new must survive (newest).
	if _, err := os.Stat(filepath.Join(dir, "pkg-old")); err == nil {
		t.Error("pkg-old should have been evicted")
	}
	if _, err := os.Stat(filepath.Join(dir, "pkg-new")); err != nil {
		t.Errorf("pkg-new should survive: %v", err)
	}
}

// TestRunOnce_NonExistentDir returns nil (cache not yet created).
func TestRunOnce_NonExistentDir(t *testing.T) {
	gc := cachegc.New(cachegc.Config{
		Dir:              filepath.Join(t.TempDir(), "missing"),
		HighWatermarkMiB: 100,
		Interval:         time.Hour,
	})
	if err := gc.RunOnce(); err != nil {
		t.Errorf("expected nil error for missing dir, got: %v", err)
	}
}

// TestRunOnce_NoopWhenDisabled returns immediately when Dir is empty.
func TestRunOnce_NoopWhenDisabled(_ *testing.T) {
	gc := cachegc.New(cachegc.Config{
		Dir:              "",
		HighWatermarkMiB: 100,
		Interval:         time.Hour,
	})
	_ = gc.RunOnce() // no assertion — just must not panic or block
}

// TestRunOnce_ZeroWatermark does not evict anything (disabled config guard).
func TestRunOnce_ZeroWatermark(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	writeFakePackage(t, dir, "pkg-a", 3*1024*1024, now, true)

	gc := cachegc.New(cachegc.Config{
		Dir:              dir,
		HighWatermarkMiB: 0, // invalid — must be treated as disabled
		Interval:         time.Hour,
	})
	if err := gc.RunOnce(); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "pkg-a")); err != nil {
		t.Errorf("pkg-a should not have been evicted with zero watermark: %v", err)
	}
}

// TestRunOnce_SubMiBEntries evicts only enough sub-MiB files to reach target.
// Regression for the sizeMiB=0 truncation bug where all entries were deleted.
func TestRunOnce_SubMiBEntries(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	// Five 700 KiB files: total ~3.4 MiB. Watermark = 2 MiB → target = 1.6 MiB.
	// Expect: oldest files evicted until remaining ≤ target; newest must survive.
	for i, name := range []string{"p1", "p2", "p3", "p4", "p5"} {
		writeFakePackage(t, dir, name, 700*1024, base.Add(time.Duration(i)*time.Hour), false)
	}

	gc := cachegc.New(cachegc.Config{
		Dir:              dir,
		HighWatermarkMiB: 2,
		Interval:         time.Hour,
	})
	if err := gc.RunOnce(); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	// p5 (newest) must survive — it should never be touched.
	if _, err := os.Stat(filepath.Join(dir, "p5")); err != nil {
		t.Errorf("p5 (newest) should survive: %v", err)
	}
	// At least one file must have been evicted (usage was over watermark).
	evicted := 0
	for _, name := range []string{"p1", "p2", "p3", "p4"} {
		if _, err := os.Stat(filepath.Join(dir, name)); os.IsNotExist(err) {
			evicted++
		}
	}
	if evicted == 0 {
		t.Error("expected at least one file to be evicted, got none")
	}
	// Not all files should be evicted (regression check).
	if evicted == 5 {
		t.Error("all 5 files were evicted — sub-MiB truncation bug may have recurred")
	}
}

// TestDefaultConfig_Fallback returns the compiled-in defaults when no env is set.
func TestDefaultConfig_Fallback(t *testing.T) {
	t.Setenv("NODEVAULT_PKG_CACHE_DIR", "")
	t.Setenv("CONDA_PKGS_DIRS", "")
	t.Setenv("NODEVAULT_PKG_CACHE_HIGH_WATERMARK_MIB", "")
	t.Setenv("NODEVAULT_PKG_CACHE_GC_INTERVAL", "")

	cfg := cachegc.DefaultConfig()
	if cfg.Dir != "/var/cache/nodevault/packages/conda" {
		t.Errorf("Dir: got %q, want default path", cfg.Dir)
	}
	if cfg.HighWatermarkMiB != 10240 {
		t.Errorf("HighWatermarkMiB: got %d, want 10240", cfg.HighWatermarkMiB)
	}
	if cfg.Interval != 30*time.Minute {
		t.Errorf("Interval: got %v, want 30m", cfg.Interval)
	}
}

// TestDefaultConfig_EnvOverride picks up environment variables.
func TestDefaultConfig_EnvOverride(t *testing.T) {
	t.Setenv("NODEVAULT_PKG_CACHE_DIR", "/custom/cache")
	t.Setenv("NODEVAULT_PKG_CACHE_HIGH_WATERMARK_MIB", "2048")
	t.Setenv("NODEVAULT_PKG_CACHE_GC_INTERVAL", "15m")

	cfg := cachegc.DefaultConfig()
	if cfg.Dir != "/custom/cache" {
		t.Errorf("Dir: got %q, want /custom/cache", cfg.Dir)
	}
	if cfg.HighWatermarkMiB != 2048 {
		t.Errorf("HighWatermarkMiB: got %d, want 2048", cfg.HighWatermarkMiB)
	}
	if cfg.Interval != 15*time.Minute {
		t.Errorf("Interval: got %v, want 15m", cfg.Interval)
	}
}
