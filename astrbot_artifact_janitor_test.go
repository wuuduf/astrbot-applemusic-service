package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAstrBotArtifactCleanupAgeAndQuota(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	oldPath := writeSizedFileForTest(t, root, "old.txt", 10)
	aPath := writeSizedFileForTest(t, root, "a.txt", 80)
	bPath := writeSizedFileForTest(t, root, "b.txt", 70)
	cPath := writeSizedFileForTest(t, root, "c.txt", 60)

	now := time.Now()
	mustChtimes(t, oldPath, now.Add(-3*time.Hour))
	mustChtimes(t, aPath, now.Add(-50*time.Minute))
	mustChtimes(t, bPath, now.Add(-40*time.Minute))
	mustChtimes(t, cPath, now.Add(-30*time.Minute))

	svc := &astrbotAPIService{
		artifactRoot: root,
		artifactPolicy: artifactPolicy{
			maxAge:     1 * time.Hour,
			maxBytes:   120,
			protectAge: 0,
		},
		artifactState: artifactState{
			activeArtifactIO: make(map[string]int),
		},
	}

	stats := svc.cleanupArtifactsAt(now)
	if stats.RemovedByAge == 0 {
		t.Fatalf("expected age cleanup to remove old files")
	}
	if stats.RemovedByQuota == 0 {
		t.Fatalf("expected quota cleanup to remove files")
	}
	assertFileMissing(t, oldPath)
	assertFileMissing(t, aPath)
	assertFileMissing(t, bPath)
	assertFileExists(t, cPath)
}

func TestAstrBotArtifactCleanupSkipsActiveWrite(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	path := writeSizedFileForTest(t, root, "active.txt", 32)
	now := time.Now()
	mustChtimes(t, path, now.Add(-2*time.Hour))

	svc := &astrbotAPIService{
		artifactRoot: root,
		artifactPolicy: artifactPolicy{
			maxAge:     10 * time.Minute,
			maxBytes:   1,
			protectAge: 0,
		},
		artifactState: artifactState{
			activeArtifactIO: make(map[string]int),
		},
	}

	svc.beginArtifactIO(path)
	svc.cleanupArtifactsAt(now)
	assertFileExists(t, path)

	svc.endArtifactIO(path)
	svc.cleanupArtifactsAt(now)
	assertFileMissing(t, path)
}

func TestAstrBotArtifactJanitorRemovesExpired(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	path := writeSizedFileForTest(t, root, "expired.txt", 16)
	now := time.Now()
	mustChtimes(t, path, now.Add(-2*time.Minute))

	svc := &astrbotAPIService{
		artifactRoot: root,
		artifactPolicy: artifactPolicy{
			maxAge:          1 * time.Second,
			maxBytes:        1024,
			janitorInterval: 20 * time.Millisecond,
			protectAge:      0,
		},
		artifactState: artifactState{
			activeArtifactIO: make(map[string]int),
		},
	}
	svc.startArtifactJanitor()
	defer svc.stopArtifactJanitor()

	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("expected janitor to remove expired file")
}

func TestAstrBotArtifactCleanupQuotaIncludesNestedFiles(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	nestedDir := filepath.Join(root, "lyrics-album-001")
	if err := os.MkdirAll(nestedDir, 0755); err != nil {
		t.Fatalf("mkdir nested dir failed: %v", err)
	}
	rootPath := writeSizedFileForTest(t, root, "root.txt", 40)
	nestedPath := writeSizedFileForTest(t, nestedDir, "nested.txt", 80)

	now := time.Now()
	mustChtimes(t, rootPath, now.Add(-10*time.Minute))
	mustChtimes(t, nestedPath, now.Add(-20*time.Minute))

	svc := &astrbotAPIService{
		artifactRoot: root,
		artifactPolicy: artifactPolicy{
			maxAge:     0,
			maxBytes:   50,
			protectAge: 0,
		},
		artifactState: artifactState{
			activeArtifactIO: make(map[string]int),
		},
	}

	stats := svc.cleanupArtifactsAt(now)
	if stats.RemovedByQuota == 0 {
		t.Fatalf("expected quota cleanup to remove nested files")
	}
	assertFileMissing(t, nestedPath)
	assertFileExists(t, rootPath)
}

func writeSizedFileForTest(t *testing.T, dir string, name string, size int) string {
	t.Helper()
	path := filepath.Join(dir, name)
	data := make([]byte, size)
	for i := range data {
		data[i] = 'x'
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatalf("write file failed: %v", err)
	}
	return path
}

func mustChtimes(t *testing.T, path string, modTime time.Time) {
	t.Helper()
	if err := os.Chtimes(path, modTime, modTime); err != nil {
		t.Fatalf("chtimes failed: %v", err)
	}
}

func assertFileExists(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected file to exist: %s (%v)", path, err)
	}
}

func assertFileMissing(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("expected file to be removed: %s", path)
	}
}
