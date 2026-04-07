package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestTelegramCleanupRecordPathsDoesNotTriggerFullScan(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	path := writeSizedFileForTest(t, root, "track.m4a", 64)
	scanCalls := 0
	tracker := newTelegramCleanupTracker([]string{root}, "", 1024, time.Minute, time.Hour, 0)
	tracker.scanFolder = func(root string, cacheFile string) (int64, []downloadFileEntry, error) {
		scanCalls++
		return 0, nil, nil
	}

	tracker.RecordPaths([]string{path})
	if scanCalls != 0 {
		t.Fatalf("recording paths should not trigger full scan")
	}
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	if len(tracker.files) != 1 {
		t.Fatalf("expected 1 tracked file, got %d", len(tracker.files))
	}
}

func TestTelegramCleanupTrackerQuotaEvictionWithoutScan(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	oldPath := writeSizedFileForTest(t, root, "old.m4a", 80)
	newPath := writeSizedFileForTest(t, root, "new.m4a", 80)
	now := time.Now()
	mustChtimes(t, oldPath, now.Add(-2*time.Hour))
	mustChtimes(t, newPath, now.Add(-1*time.Hour))

	tracker := newTelegramCleanupTracker([]string{root}, "", 120, time.Minute, time.Hour, 0)
	tracker.RecordPaths([]string{oldPath, newPath})
	tracker.cleanupOnce(false)

	assertFileMissing(t, oldPath)
	assertFileExists(t, newPath)
	tracker.mu.Lock()
	if tracker.scanRuns != 0 {
		t.Fatalf("expected zero fallback scans, got %d", tracker.scanRuns)
	}
	tracker.mu.Unlock()
}

func TestTelegramCleanupTrackerFallbackScan(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	pathA := filepath.Join(root, "a.m4a")
	pathB := filepath.Join(root, "b.m4a")
	removed := map[string]bool{}

	tracker := newTelegramCleanupTracker([]string{root}, "", 50, time.Minute, time.Second, 0)
	tracker.scanFolder = func(root string, cacheFile string) (int64, []downloadFileEntry, error) {
		return 120, []downloadFileEntry{
			{path: pathA, size: 60, modTime: time.Now().Add(-2 * time.Hour)},
			{path: pathB, size: 60, modTime: time.Now().Add(-1 * time.Hour)},
		}, nil
	}
	tracker.removeFile = func(path string) error {
		removed[path] = true
		return nil
	}

	tracker.cleanupOnce(false)
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	if tracker.scanRuns == 0 {
		t.Fatalf("expected fallback scan to run")
	}
	if len(removed) == 0 {
		t.Fatalf("expected quota cleanup to remove at least one file")
	}
}

func TestTelegramCleanupTrackerJanitor(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	oldPath := writeSizedFileForTest(t, root, "janitor-old.m4a", 80)
	newPath := writeSizedFileForTest(t, root, "janitor-new.m4a", 80)
	now := time.Now()
	mustChtimes(t, oldPath, now.Add(-2*time.Hour))
	mustChtimes(t, newPath, now.Add(-1*time.Hour))

	tracker := newTelegramCleanupTracker([]string{root}, "", 120, 20*time.Millisecond, time.Hour, 0)
	tracker.RecordPaths([]string{oldPath, newPath})
	tracker.start()
	defer tracker.stop()

	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(oldPath); os.IsNotExist(err) {
			assertFileExists(t, newPath)
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("expected janitor to evict old file")
}
