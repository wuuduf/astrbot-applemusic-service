package main

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

type artifactEntry struct {
	path    string
	size    int64
	modTime time.Time
	isDir   bool
	active  bool
	owner   string
	mode    string
}

type artifactCleanupStats struct {
	RemovedByAge   int
	RemovedByQuota int
	TotalBytes     int64
}

func resolveAstrBotArtifactMaxAge() time.Duration {
	hours := resolvePositiveIntConfigEnv(Config.AstrBotArtifactMaxAgeHours, "ASTRBOT_ARTIFACT_MAX_AGE_HOURS", int(defaultAstrBotArtifactMaxAge/time.Hour))
	return time.Duration(hours) * time.Hour
}

func resolveAstrBotArtifactMaxBytes() int64 {
	mb := resolvePositiveIntConfigEnv(Config.AstrBotArtifactMaxSizeMB, "ASTRBOT_ARTIFACT_MAX_SIZE_MB", defaultAstrBotArtifactMaxSizeMB)
	return int64(mb) * 1024 * 1024
}

func resolveAstrBotArtifactJanitorInterval() time.Duration {
	sec := resolvePositiveIntConfigEnv(Config.AstrBotArtifactJanitorIntervalSec, "ASTRBOT_ARTIFACT_JANITOR_INTERVAL_SEC", int(defaultAstrBotArtifactJanitorInterval/time.Second))
	return time.Duration(sec) * time.Second
}

func resolveAstrBotArtifactProtectAge() time.Duration {
	sec := resolvePositiveIntConfigEnv(Config.AstrBotArtifactProtectSec, "ASTRBOT_ARTIFACT_PROTECT_SEC", int(defaultAstrBotArtifactProtectAge/time.Second))
	return time.Duration(sec) * time.Second
}

func resolveAstrBotJobTimeout() time.Duration {
	sec := resolvePositiveIntConfigEnv(0, "ASTRBOT_JOB_TIMEOUT_SEC", int(defaultAstrBotJobTimeout/time.Second))
	return time.Duration(sec) * time.Second
}

func resolvePositiveIntConfigEnv(configValue int, envKey string, fallback int) int {
	if configValue > 0 {
		return configValue
	}
	raw := strings.TrimSpace(os.Getenv(envKey))
	if raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
			return parsed
		}
	}
	return fallback
}

func (s *astrbotAPIService) startArtifactJanitor() {
	if s.janitorInterval <= 0 {
		return
	}
	s.artifactMu.Lock()
	if s.artifactJanitorStop != nil {
		s.artifactMu.Unlock()
		return
	}
	stop := make(chan struct{})
	s.artifactJanitorStop = stop
	s.artifactJanitorWG.Add(1)
	s.artifactMu.Unlock()

	go func() {
		defer s.artifactJanitorWG.Done()
		runWithRecovery("astrbot artifact janitor", nil, func() {
			ticker := time.NewTicker(s.janitorInterval)
			defer ticker.Stop()
			for {
				select {
				case <-ticker.C:
					runWithRecovery("astrbot artifact janitor tick", nil, func() {
						s.cleanupArtifactsNow()
					})
				case <-stop:
					return
				}
			}
		})
	}()
}

func (s *astrbotAPIService) stopArtifactJanitor() {
	s.artifactMu.Lock()
	stop := s.artifactJanitorStop
	if stop != nil {
		close(stop)
		s.artifactJanitorStop = nil
	}
	s.artifactMu.Unlock()
	if stop != nil {
		s.artifactJanitorWG.Wait()
	}
}

func (s *astrbotAPIService) cleanupArtifactsNow() artifactCleanupStats {
	return s.cleanupArtifactsAt(time.Now())
}

func (s *astrbotAPIService) cleanupArtifactsAt(now time.Time) artifactCleanupStats {
	stats := artifactCleanupStats{}
	entries, err := s.collectArtifactEntries()
	if err != nil {
		return stats
	}
	survivors := make([]artifactEntry, 0, len(entries))
	for _, entry := range entries {
		if entry.active {
			survivors = append(survivors, entry)
			continue
		}
		if s.maxAge > 0 && now.Sub(entry.modTime) > s.maxAge {
			if err := os.RemoveAll(entry.path); err == nil {
				stats.RemovedByAge++
				appRuntimeMetrics.recordCleanupRemoval(entry.size)
				continue
			}
		}
		survivors = append(survivors, entry)
	}

	if s.maxBytes <= 0 {
		return stats
	}

	quotaCandidates := make([]artifactEntry, 0, len(survivors))
	for _, entry := range survivors {
		if entry.isDir {
			continue
		}
		quotaCandidates = append(quotaCandidates, entry)
		stats.TotalBytes += entry.size
	}
	if stats.TotalBytes <= s.maxBytes {
		return stats
	}

	sort.Slice(quotaCandidates, func(i, j int) bool {
		if quotaCandidates[i].modTime.Equal(quotaCandidates[j].modTime) {
			return quotaCandidates[i].path < quotaCandidates[j].path
		}
		return quotaCandidates[i].modTime.Before(quotaCandidates[j].modTime)
	})

	total := stats.TotalBytes
	for _, entry := range quotaCandidates {
		if total <= s.maxBytes {
			break
		}
		if entry.active {
			continue
		}
		if s.protectAge > 0 && now.Sub(entry.modTime) < s.protectAge {
			continue
		}
		if err := os.Remove(entry.path); err != nil {
			continue
		}
		total -= entry.size
		stats.RemovedByQuota++
		appRuntimeMetrics.recordCleanupRemoval(entry.size)
	}
	stats.TotalBytes = total
	return stats
}

func (s *astrbotAPIService) collectArtifactEntries() ([]artifactEntry, error) {
	root := s.artifactCleanupRoot()
	if strings.TrimSpace(root.Path) == "" {
		return nil, nil
	}
	result := []artifactEntry{}
	err := filepath.WalkDir(root.Path, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		result = append(result, artifactEntry{
			path:    path,
			size:    info.Size(),
			modTime: info.ModTime(),
			isDir:   false,
			active:  s.isArtifactIOActive(path),
			owner:   string(root.Owner),
			mode:    string(root.Mode),
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (s *astrbotAPIService) isArtifactIOActive(path string) bool {
	s.artifactMu.Lock()
	defer s.artifactMu.Unlock()
	return s.activeArtifactIO[path] > 0
}
