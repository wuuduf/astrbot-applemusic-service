package main

import (
	"fmt"
	"sync/atomic"
	"time"
)

type runtimeMetrics struct {
	uploadSuccesses       atomic.Uint64
	uploadFailures        atomic.Uint64
	telegramRetryAfterHit atomic.Uint64
	externalCmdTimeouts   atomic.Uint64
	cleanupDeletedFiles   atomic.Uint64
	cleanupDeletedBytes   atomic.Int64
}

type runtimeMetricsSnapshot struct {
	UploadSuccesses     uint64
	UploadFailures      uint64
	TelegramRetryAfter  uint64
	ExternalCmdTimeouts uint64
	CleanupDeletedFiles uint64
	CleanupDeletedBytes int64
}

var appRuntimeMetrics = &runtimeMetrics{}

func (m *runtimeMetrics) recordUploadSuccess() {
	if m == nil {
		return
	}
	m.uploadSuccesses.Add(1)
}

func (m *runtimeMetrics) recordUploadFailure() {
	if m == nil {
		return
	}
	m.uploadFailures.Add(1)
}

func (m *runtimeMetrics) recordTelegramRetryAfter() {
	if m == nil {
		return
	}
	m.telegramRetryAfterHit.Add(1)
}

func (m *runtimeMetrics) recordExternalCommandTimeout() {
	if m == nil {
		return
	}
	m.externalCmdTimeouts.Add(1)
}

func (m *runtimeMetrics) recordCleanupRemoval(size int64) {
	if m == nil {
		return
	}
	m.cleanupDeletedFiles.Add(1)
	if size > 0 {
		m.cleanupDeletedBytes.Add(size)
	}
}

func (m *runtimeMetrics) snapshot() runtimeMetricsSnapshot {
	if m == nil {
		return runtimeMetricsSnapshot{}
	}
	return runtimeMetricsSnapshot{
		UploadSuccesses:     m.uploadSuccesses.Load(),
		UploadFailures:      m.uploadFailures.Load(),
		TelegramRetryAfter:  m.telegramRetryAfterHit.Load(),
		ExternalCmdTimeouts: m.externalCmdTimeouts.Load(),
		CleanupDeletedFiles: m.cleanupDeletedFiles.Load(),
		CleanupDeletedBytes: m.cleanupDeletedBytes.Load(),
	}
}

func telegramMetricsInterval() time.Duration {
	sec := Config.TelegramMetricsIntervalSec
	if sec <= 0 {
		return defaultTelegramMetricsInterval
	}
	return time.Duration(sec) * time.Second
}

func (b *TelegramBot) queueStats() (queued int, active int, limit int) {
	if b == nil {
		return 0, 0, 0
	}
	b.queueMu.Lock()
	defer b.queueMu.Unlock()
	return len(b.downloadQueue), b.activeWorkers, b.workerLimit
}

func (b *TelegramBot) trackedRequestCount() int {
	if b == nil {
		return 0
	}
	b.requestStateMu.Lock()
	defer b.requestStateMu.Unlock()
	return len(b.activeRequests)
}

func (b *TelegramBot) inflightCount() int {
	if b == nil {
		return 0
	}
	b.inflightMu.Lock()
	defer b.inflightMu.Unlock()
	return len(b.inflightDownloads)
}

func (b *TelegramBot) startMetricsReporter() {
	if b == nil {
		return
	}
	interval := telegramMetricsInterval()
	if interval <= 0 {
		return
	}
	if b.metricsStop != nil {
		return
	}
	stopCh := make(chan struct{})
	b.metricsStop = stopCh
	b.metricsWG.Add(1)
	go func() {
		defer b.metricsWG.Done()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				b.reportMetricsOnce()
			case <-stopCh:
				return
			}
		}
	}()
}

func (b *TelegramBot) stopMetricsReporter() {
	if b == nil || b.metricsStop == nil {
		return
	}
	stopCh := b.metricsStop
	close(stopCh)
	b.metricsWG.Wait()
	b.metricsStop = nil
}

func (b *TelegramBot) reportMetricsOnce() {
	if b == nil {
		return
	}
	queueLen, active, limit := b.queueStats()
	inflight := b.inflightCount()
	tracked := b.trackedRequestCount()
	metrics := appRuntimeMetrics.snapshot()
	totalUploads := metrics.UploadSuccesses + metrics.UploadFailures
	failRate := 0.0
	if totalUploads > 0 {
		failRate = float64(metrics.UploadFailures) / float64(totalUploads) * 100
	}
	resourceState := ""
	if b.resourceGuard != nil {
		if blocked, reason := b.resourceGuard.snapshot(); blocked {
			resourceState = fmt.Sprintf(" resource_blocked=true reason=%q", reason)
		}
	}
	fmt.Printf(
		"[metrics] queue=%d active=%d/%d inflight=%d tracked=%d uploads_ok=%d uploads_fail=%d upload_fail_rate=%.1f%% retry_after=%d cmd_timeout=%d cleanup_deleted=%d cleanup_bytes=%s%s\n",
		queueLen,
		active,
		limit,
		inflight,
		tracked,
		metrics.UploadSuccesses,
		metrics.UploadFailures,
		failRate,
		metrics.TelegramRetryAfter,
		metrics.ExternalCmdTimeouts,
		metrics.CleanupDeletedFiles,
		formatBytes(metrics.CleanupDeletedBytes),
		resourceState,
	)
	if totalUploads >= 5 && failRate >= 40 {
		fmt.Printf("[warn] upload failure rate is high: %.1f%% (%d/%d)\n", failRate, metrics.UploadFailures, totalUploads)
	}
}
