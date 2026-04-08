package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
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
	if len(loaded.InflightKeys) != 1 || loaded.InflightKeys[0] != "k1" {
		t.Fatalf("expected inflight keys derived from requests, got %+v", loaded.InflightKeys)
	}
}

func TestTelegramRuntimeStateDoesNotPersistOrphanInflightKeys(t *testing.T) {
	tmpDir := t.TempDir()
	statePath := filepath.Join(tmpDir, "telegram-state.json")
	b := &TelegramBot{
		stateFile:          statePath,
		pending:            make(map[int64]map[int]*PendingSelection),
		pendingTransfers:   make(map[int64]map[int]*PendingTransfer),
		pendingArtistModes: make(map[int64]map[int]*PendingArtistMode),
		activeRequests:     make(map[string]telegramPersistedRequest),
		inflightDownloads:  map[string]struct{}{"orphan-key": {}},
		chatSettings:       make(map[int64]ChatDownloadSettings),
	}

	if err := b.saveRuntimeStateNow(); err != nil {
		t.Fatalf("saveRuntimeStateNow failed: %v", err)
	}
	loaded, err := loadRuntimeStateFromFile(statePath)
	if err != nil {
		t.Fatalf("loadRuntimeStateFromFile failed: %v", err)
	}
	if len(loaded.InflightKeys) != 0 {
		t.Fatalf("expected no orphan inflight keys to be persisted, got %+v", loaded.InflightKeys)
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

func TestTelegramRuntimeStateRestoreIgnoresLegacyOrphanInflightKeys(t *testing.T) {
	tmpDir := t.TempDir()
	statePath := filepath.Join(tmpDir, "telegram-state.json")
	state := telegramPersistedState{
		Version:      telegramStateVersion,
		InflightKeys: []string{"legacy-orphan"},
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
		downloadQueue:      make(chan *downloadRequest, 1),
		stateSave:          make(chan struct{}, 1),
	}
	b.restoreRuntimeState()

	if len(b.inflightDownloads) != 0 {
		t.Fatalf("expected legacy orphan inflight keys to be ignored, got %+v", b.inflightDownloads)
	}
}

func TestTelegramRuntimeStateRestoreQueuesRecoveredHeavyTaskRequests(t *testing.T) {
	tmpDir := t.TempDir()
	statePath := filepath.Join(tmpDir, "telegram-state.json")
	state := telegramPersistedState{
		Version: telegramStateVersion,
		Requests: []telegramPersistedRequest{
			{
				RequestID:    "req-cover-1",
				ChatID:       202,
				ReplyToID:    5,
				Single:       true,
				TaskType:     telegramTaskCover,
				Settings:     ChatDownloadSettings{Format: telegramFormatAlac, SettingsInited: true},
				TransferMode: transferModeOneByOne,
				MediaType:    mediaTypeAlbum,
				MediaID:      "album-123",
				Storefront:   "us",
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
		t.Fatalf("expected recovered heavy request in queue, got %d", len(b.downloadQueue))
	}
	req := <-b.downloadQueue
	if req == nil {
		t.Fatalf("expected non-nil request")
	}
	if req.taskType != telegramTaskCover {
		t.Fatalf("expected recovered cover task, got %q", req.taskType)
	}
	if req.mediaType != mediaTypeAlbum || req.mediaID != "album-123" {
		t.Fatalf("unexpected recovered heavy request: %+v", req)
	}
	if req.run == nil {
		t.Fatalf("expected recovered heavy request runner to be rebuilt")
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

func TestRuntimeMetricsTaskTypeLifecycle(t *testing.T) {
	metrics := &runtimeMetrics{}

	metrics.recordTaskQueued(telegramTaskCover)
	metrics.recordTaskStarted(telegramTaskCover)
	metrics.recordTaskFinished(telegramTaskCover)
	metrics.recordTaskPanic(telegramTaskCover)

	snapshot := metrics.snapshot()
	taskMetrics, ok := snapshot.TaskTypes[telegramTaskCover]
	if !ok {
		t.Fatalf("expected cover task metrics in snapshot")
	}
	if taskMetrics.QueuedTotal != 1 || taskMetrics.StartedTotal != 1 || taskMetrics.FinishedTotal != 1 || taskMetrics.PanicTotal != 1 {
		t.Fatalf("unexpected task lifecycle metrics: %#v", taskMetrics)
	}
}

func TestTrackedRequestStatsByType(t *testing.T) {
	b := &TelegramBot{
		activeRequests: map[string]telegramPersistedRequest{
			"req-download": {TaskType: telegramTaskDownload, State: "queued"},
			"req-cover":    {TaskType: telegramTaskCover, State: "running"},
			"req-lyrics":   {TaskType: telegramTaskSongLyrics, State: "queued"},
		},
	}

	stats := b.trackedRequestStatsByType()

	if got := stats[telegramTaskDownload].QueuedCurrent; got != 1 {
		t.Fatalf("expected download queued current=1, got %d", got)
	}
	if got := stats[telegramTaskCover].RunningCurrent; got != 1 {
		t.Fatalf("expected cover running current=1, got %d", got)
	}
	if got := stats[telegramTaskSongLyrics].QueuedCurrent; got != 1 {
		t.Fatalf("expected song-lyrics queued current=1, got %d", got)
	}
}

func TestRunWithRecoveryRecoversPanic(t *testing.T) {
	var callbackErr error
	panicked := runWithRecovery("test panic", func(err error) {
		callbackErr = err
	}, func() {
		panic("boom")
	})
	if !panicked {
		t.Fatalf("expected panic to be recovered")
	}
	if callbackErr == nil {
		t.Fatalf("expected panic callback error")
	}
	if !strings.Contains(callbackErr.Error(), "test panic panic: boom") {
		t.Fatalf("unexpected callback error: %v", callbackErr)
	}
}

func TestDownloadWorkerContinuesAfterTaskPanic(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "/sendMessage"):
			_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":1}}`))
		default:
			_, _ = w.Write([]byte(`{"ok":true}`))
		}
	}))
	defer server.Close()

	b := &TelegramBot{
		token:             "test",
		apiBase:           server.URL,
		client:            server.Client(),
		pollClient:        server.Client(),
		downloadQueue:     make(chan *downloadRequest, 4),
		workerLimit:       1,
		inflightDownloads: make(map[string]struct{}),
		activeRequests:    make(map[string]telegramPersistedRequest),
	}
	b.queueCond = sync.NewCond(&b.queueMu)
	b.startDownloadWorker()

	done := make(chan struct{})
	b.downloadQueue <- &downloadRequest{
		chatID:    1,
		replyToID: 11,
		requestID: "panic-task",
		mediaType: mediaTypeSong,
		mediaID:   "song-panic",
		single:    true,
		settings:  normalizeChatSettings(ChatDownloadSettings{}),
		fn: func(session *DownloadSession) error {
			panic("task panic")
		},
	}
	b.downloadQueue <- &downloadRequest{
		chatID:    1,
		replyToID: 12,
		requestID: "next-task",
		mediaType: mediaTypeSong,
		mediaID:   "song-next",
		single:    true,
		settings:  normalizeChatSettings(ChatDownloadSettings{}),
		fn: func(session *DownloadSession) error {
			close(done)
			return nil
		},
	}

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("second task did not run after first task panic")
	}
	b.stopDownloadWorkers()
}

func TestTaskWorkerContinuesAfterHeavyTaskPanic(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "/sendMessage"):
			_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":1}}`))
		default:
			_, _ = w.Write([]byte(`{"ok":true}`))
		}
	}))
	defer server.Close()

	b := &TelegramBot{
		token:             "test",
		apiBase:           server.URL,
		client:            server.Client(),
		pollClient:        server.Client(),
		downloadQueue:     make(chan *downloadRequest, 4),
		workerLimit:       1,
		inflightDownloads: make(map[string]struct{}),
		activeRequests:    make(map[string]telegramPersistedRequest),
	}
	b.queueCond = sync.NewCond(&b.queueMu)
	b.startDownloadWorker()

	done := make(chan struct{})
	b.downloadQueue <- &downloadRequest{
		chatID:    1,
		replyToID: 21,
		requestID: "panic-heavy-task",
		taskType:  telegramTaskCover,
		mediaType: mediaTypeAlbum,
		mediaID:   "album-panic",
		run: func(*TelegramBot) error {
			panic("heavy task panic")
		},
	}
	b.downloadQueue <- &downloadRequest{
		chatID:    1,
		replyToID: 22,
		requestID: "next-heavy-task",
		taskType:  telegramTaskCover,
		mediaType: mediaTypeAlbum,
		mediaID:   "album-next",
		run: func(*TelegramBot) error {
			close(done)
			return nil
		},
	}

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("second heavy task did not run after first task panic")
	}
	b.stopDownloadWorkers()
}

func TestStopStateSaverFlushesLatestState(t *testing.T) {
	tmpDir := t.TempDir()
	statePath := filepath.Join(tmpDir, "telegram-state.json")
	b := &TelegramBot{
		stateFile:          statePath,
		pending:            make(map[int64]map[int]*PendingSelection),
		pendingTransfers:   make(map[int64]map[int]*PendingTransfer),
		pendingArtistModes: make(map[int64]map[int]*PendingArtistMode),
		activeRequests: map[string]telegramPersistedRequest{
			"req-1": {RequestID: "req-1", InflightKey: "k1", MediaType: mediaTypeSong, MediaID: "song-1", Storefront: "us"},
		},
		inflightDownloads: map[string]struct{}{"k1": {}},
		chatSettings:      make(map[int64]ChatDownloadSettings),
	}

	b.startStateSaver()
	b.stopStateSaver()

	loaded, err := loadRuntimeStateFromFile(statePath)
	if err != nil {
		t.Fatalf("loadRuntimeStateFromFile failed: %v", err)
	}
	if len(loaded.Requests) != 1 || loaded.Requests[0].RequestID != "req-1" {
		t.Fatalf("expected latest request to be flushed, got %+v", loaded.Requests)
	}
}

func TestStopDownloadWorkersWaitsForRunningTask(t *testing.T) {
	b := &TelegramBot{
		downloadQueue:     make(chan *downloadRequest, 1),
		workerLimit:       1,
		inflightDownloads: make(map[string]struct{}),
		activeRequests:    make(map[string]telegramPersistedRequest),
	}
	b.queueCond = sync.NewCond(&b.queueMu)
	b.startDownloadWorker()

	done := make(chan struct{})
	release := make(chan struct{})
	b.downloadQueue <- &downloadRequest{
		requestID: "req-1",
		taskType:  telegramTaskCover,
		mediaType: mediaTypeAlbum,
		mediaID:   "album-1",
		run: func(bot *TelegramBot) error {
			close(done)
			<-release
			return nil
		},
	}

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("worker did not start task")
	}

	stopped := make(chan struct{})
	go func() {
		b.stopDownloadWorkers()
		close(stopped)
	}()

	select {
	case <-stopped:
		t.Fatalf("stopDownloadWorkers returned before running task finished")
	case <-time.After(100 * time.Millisecond):
	}

	close(release)

	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatalf("stopDownloadWorkers did not wait for task completion")
	}
}

func jsonMarshalIndentForTest(v any) ([]byte, error) {
	return json.MarshalIndent(v, "", "  ")
}
