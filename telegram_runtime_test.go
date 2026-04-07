package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apputils "github.com/wuuduf/astrbot-applemusic-service/utils"
)

func TestTelegramSendLimiterBlockFor(t *testing.T) {
	limiter := newTelegramSendLimiter(100*time.Millisecond, 100*time.Millisecond)
	if limiter == nil {
		t.Fatalf("expected limiter")
	}
	now := time.Unix(2000, 0)
	limiter.nowFn = func() time.Time { return now }
	limiter.blockFor(3 * time.Second)
	wait := limiter.nextWaitLocked(now, 1001)
	if wait != 3*time.Second {
		t.Fatalf("expected 3s wait, got %s", wait)
	}
}

func TestTelegramRuntimeStateSaveLoadRoundTrip(t *testing.T) {
	tmpDir := t.TempDir()
	statePath := filepath.Join(tmpDir, "telegram-state.json")
	b := &TelegramBot{
		stateFile: statePath,
		pending: map[int64]map[int]*PendingSelection{
			1: {
				10: {
					Kind:       "song",
					Query:      "hello",
					Storefront: "us",
					Items: []apputils.SearchResultItem{
						{ID: "song-1", Name: "Song 1", Artist: "Artist 1"},
					},
					CreatedAt:        time.Now(),
					ReplyToMessageID: 99,
					ResultsMessageID: 10,
				},
			},
		},
		pendingTransfers: map[int64]map[int]*PendingTransfer{
			1: {
				11: {
					MediaType:        mediaTypeAlbum,
					MediaID:          "album-1",
					Storefront:       "us",
					ReplyToMessageID: 100,
					MessageID:        11,
					CreatedAt:        time.Now(),
				},
			},
		},
		pendingArtistModes: map[int64]map[int]*PendingArtistMode{
			1: {
				12: {
					ArtistID:         "artist-1",
					ArtistName:       "Artist",
					Storefront:       "us",
					ReplyToMessageID: 101,
					MessageID:        12,
					CreatedAt:        time.Now(),
				},
			},
		},
		activeRequests: map[string]telegramPersistedRequest{
			"req-1": {
				RequestID:   "req-1",
				ChatID:      1,
				MediaType:   mediaTypeSong,
				MediaID:     "song-1",
				Storefront:  "us",
				InflightKey: "k1",
				State:       "queued",
			},
		},
		inflightDownloads: map[string]struct{}{"k1": {}},
		chatSettings: map[int64]ChatDownloadSettings{
			1: {Format: telegramFormatAlac, SettingsInited: true},
		},
	}

	if err := b.saveRuntimeStateNow(); err != nil {
		t.Fatalf("saveRuntimeStateNow failed: %v", err)
	}
	loaded, err := loadRuntimeStateFromFile(statePath)
	if err != nil {
		t.Fatalf("loadRuntimeStateFromFile failed: %v", err)
	}
	if len(loaded.Pending) != 1 {
		t.Fatalf("expected pending data")
	}
	if len(loaded.Requests) != 1 || loaded.Requests[0].RequestID != "req-1" {
		t.Fatalf("expected persisted request")
	}
	if len(loaded.InflightKeys) == 0 {
		t.Fatalf("expected inflight keys")
	}
}

func TestTelegramRuntimeStateRestoreQueuesRecoveredRequests(t *testing.T) {
	tmpDir := t.TempDir()
	statePath := filepath.Join(tmpDir, "telegram-state.json")
	state := telegramPersistedState{
		Version: telegramStateVersion,
		Requests: []telegramPersistedRequest{
			{
				RequestID:    "req-song-1",
				ChatID:       101,
				ReplyToID:    9,
				Single:       true,
				Settings:     ChatDownloadSettings{Format: telegramFormatAlac, SettingsInited: true},
				TransferMode: transferModeOneByOne,
				MediaType:    mediaTypeSong,
				MediaID:      "12345",
				Storefront:   "us",
				InflightKey:  "k-song-1",
				State:        "queued",
				UpdatedAt:    time.Now(),
			},
		},
	}
	payload, err := jsonMarshalIndentForTest(state)
	if err != nil {
		t.Fatalf("marshal state failed: %v", err)
	}
	if err := os.WriteFile(statePath, payload, 0644); err != nil {
		t.Fatalf("write state failed: %v", err)
	}

	b := &TelegramBot{
		stateFile:          statePath,
		appleToken:         "token",
		pending:            make(map[int64]map[int]*PendingSelection),
		pendingTransfers:   make(map[int64]map[int]*PendingTransfer),
		pendingArtistModes: make(map[int64]map[int]*PendingArtistMode),
		chatSettings:       make(map[int64]ChatDownloadSettings),
		inflightDownloads:  make(map[string]struct{}),
		activeRequests:     make(map[string]telegramPersistedRequest),
		downloadQueue:      make(chan *downloadRequest, 2),
		stateSave:          make(chan struct{}, 1),
	}
	b.restoreRuntimeState()

	if len(b.downloadQueue) != 1 {
		t.Fatalf("expected recovered request in queue, got %d", len(b.downloadQueue))
	}
	req := <-b.downloadQueue
	if req == nil {
		t.Fatalf("expected non-nil request")
	}
	if req.mediaType != mediaTypeSong || req.mediaID != "12345" || req.storefront != "us" {
		t.Fatalf("unexpected recovered request: %+v", req)
	}
	if _, ok := b.inflightDownloads["k-song-1"]; !ok {
		t.Fatalf("expected recovered inflight key")
	}
}

func TestTelegramResourceGuardLowDisk(t *testing.T) {
	guard := &telegramResourceGuard{
		minDiskFreeBytes: 100,
		roots:            []string{"/data"},
		diskFreeFn: func(path string) (int64, error) {
			return 80, nil
		},
		memoryFn: func() uint64 { return 10 },
	}
	guard.evaluate()
	blocked, reason := guard.snapshot()
	if !blocked {
		t.Fatalf("expected guard to block")
	}
	if !strings.Contains(reason, "low disk space") {
		t.Fatalf("unexpected reason: %s", reason)
	}
}

func TestRunExternalCommandTimeoutMetrics(t *testing.T) {
	if _, err := exec.LookPath("sleep"); err != nil {
		t.Skip("sleep command not available")
	}
	before := appRuntimeMetrics.snapshot().ExternalCmdTimeouts
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, _ = runExternalCommand(ctx, "sleep", "1")
	after := appRuntimeMetrics.snapshot().ExternalCmdTimeouts
	if after <= before {
		t.Fatalf("expected external timeout metrics to increase")
	}
}

func jsonMarshalIndentForTest(v any) ([]byte, error) {
	return json.MarshalIndent(v, "", "  ")
}
