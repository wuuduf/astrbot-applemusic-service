package storage

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/wuuduf/astrbot-applemusic-service/utils/structs"
)

type Owner string
type Mode string

const (
	OwnerTelegram Owner = "telegram"
	OwnerAstrBot  Owner = "astrbot"
	OwnerCLI      Owner = "cli"
)

const (
	ModeDownload Mode = "download"
	ModeTemp     Mode = "temp"
	ModeCache    Mode = "cache"
	ModeArtifact Mode = "artifact"
)

type CleanupRoot struct {
	Owner Owner
	Mode  Mode
	Path  string
}

func ApplyTelegramStorageOverrides(cfg *structs.ConfigSet) {
	if cfg == nil {
		return
	}
	root := strings.TrimSpace(cfg.TelegramDownloadFolder)
	if root == "" {
		return
	}
	cfg.AlacSaveFolder = root
	cfg.AtmosSaveFolder = root
	cfg.AacSaveFolder = root
}

func TelegramCleanupRoots(cfg *structs.ConfigSet) []CleanupRoot {
	if cfg == nil {
		return nil
	}
	candidates := []CleanupRoot{
		cleanupRoot(OwnerTelegram, ModeDownload, cfg.TelegramDownloadFolder),
		cleanupRoot(OwnerTelegram, ModeTemp, os.Getenv("AMDL_TMPDIR")),
		cleanupRoot(OwnerTelegram, ModeTemp, os.Getenv("TMPDIR")),
	}
	if strings.TrimSpace(cfg.TelegramDownloadFolder) == "" {
		candidates = append(candidates,
			cleanupRoot(OwnerTelegram, ModeDownload, cfg.AlacSaveFolder),
			cleanupRoot(OwnerTelegram, ModeDownload, cfg.AtmosSaveFolder),
			cleanupRoot(OwnerTelegram, ModeDownload, cfg.AacSaveFolder),
		)
	}
	return dedupeRoots(candidates)
}

func CleanupRootPaths(roots []CleanupRoot) []string {
	paths := make([]string, 0, len(roots))
	for _, root := range roots {
		if strings.TrimSpace(root.Path) == "" {
			continue
		}
		paths = append(paths, root.Path)
	}
	return paths
}

func cleanupRoot(owner Owner, mode Mode, raw string) CleanupRoot {
	clean := filepath.Clean(strings.TrimSpace(raw))
	if clean == "." || clean == "" || clean == string(filepath.Separator) {
		return CleanupRoot{}
	}
	if clean == "/tmp" || clean == "/var/tmp" {
		return CleanupRoot{}
	}
	return CleanupRoot{
		Owner: owner,
		Mode:  mode,
		Path:  clean,
	}
}

func dedupeRoots(roots []CleanupRoot) []CleanupRoot {
	result := make([]CleanupRoot, 0, len(roots))
	seen := make(map[string]struct{}, len(roots))
	for _, root := range roots {
		if strings.TrimSpace(root.Path) == "" {
			continue
		}
		key := string(root.Owner) + "|" + string(root.Mode) + "|" + root.Path
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, root)
	}
	return result
}
