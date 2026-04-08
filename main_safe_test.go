package main

import (
	"errors"
	"testing"
	"time"

	apputils "github.com/wuuduf/astrbot-applemusic-service/utils"
	"github.com/wuuduf/astrbot-applemusic-service/utils/safe"
	"github.com/wuuduf/astrbot-applemusic-service/utils/structs"
	"github.com/wuuduf/astrbot-applemusic-service/utils/task"
)

func TestWriteMP4TagsMissingGenreReturnsAccessError(t *testing.T) {
	track := &task.Track{}
	cfg := &structs.ConfigSet{}
	defer func() {
		if rec := recover(); rec != nil {
			t.Fatalf("writeMP4Tags should not panic: %v", rec)
		}
	}()
	err := writeMP4Tags(track, "", cfg)
	if err == nil {
		t.Fatalf("expected error")
	}
	var accessErr *safe.AccessError
	if !errors.As(err, &accessErr) {
		t.Fatalf("expected AccessError, got %T", err)
	}
}

func TestTelegramMediaProducesSongAudio(t *testing.T) {
	tests := []struct {
		mediaType string
		want      bool
	}{
		{mediaType: mediaTypeSong, want: true},
		{mediaType: mediaTypeAlbum, want: true},
		{mediaType: mediaTypePlaylist, want: true},
		{mediaType: mediaTypeStation, want: true},
		{mediaType: mediaTypeMusicVideo, want: false},
		{mediaType: mediaTypeArtist, want: false},
		{mediaType: mediaTypeAlbumLyrics, want: false},
	}

	for _, tt := range tests {
		got := telegramMediaProducesSongAudio(tt.mediaType)
		if got != tt.want {
			t.Fatalf("mediaType=%s got=%v want=%v", tt.mediaType, got, tt.want)
		}
	}
}

func TestDownloadSessionShouldReuseExistingFiles(t *testing.T) {
	session := newDownloadSession(structs.ConfigSet{})
	if !session.shouldReuseExistingFiles() {
		t.Fatalf("expected local file reuse by default")
	}
	session.ForceRedownload = true
	if session.shouldReuseExistingFiles() {
		t.Fatalf("expected force redownload to disable local file reuse")
	}
}

func TestApplyTelegramAudioEmbeddingPolicy(t *testing.T) {
	base := structs.ConfigSet{
		LrcFormat:           "lrc",
		SaveLrcFile:         false,
		EmbedLrc:            false,
		EmbedCover:          false,
		SaveAnimatedArtwork: false,
	}
	settings := ChatDownloadSettings{
		LyricsFormat: "ttml",
		AutoLyrics:   false,
		AutoCover:    false,
		AutoAnimated: false,
	}

	for _, mediaType := range []string{mediaTypeSong, mediaTypeAlbum, mediaTypePlaylist, mediaTypeStation} {
		session := newDownloadSession(base)
		session.StaticCoverDownload = false

		applyTelegramAudioEmbeddingPolicy(session, settings, mediaType)

		if !session.Config.EmbedLrc {
			t.Fatalf("mediaType=%s expected EmbedLrc=true", mediaType)
		}
		if !session.Config.EmbedCover {
			t.Fatalf("mediaType=%s expected EmbedCover=true", mediaType)
		}
		if session.Config.LrcFormat != "ttml" {
			t.Fatalf("mediaType=%s expected LyricsFormat=ttml got=%s", mediaType, session.Config.LrcFormat)
		}
		if session.Config.SaveLrcFile {
			t.Fatalf("mediaType=%s expected SaveLrcFile=false when AutoLyrics=false", mediaType)
		}
		if session.Config.SaveAnimatedArtwork {
			t.Fatalf("mediaType=%s expected SaveAnimatedArtwork=false when AutoAnimated=false", mediaType)
		}
		if !session.StaticCoverDownload {
			t.Fatalf("mediaType=%s expected StaticCoverDownload=true for cover embedding", mediaType)
		}
	}
}

func TestParseTelegramRetryAfterFromJSONBody(t *testing.T) {
	err := errors.New(`telegram sendDocument failed: {"ok":false,"error_code":429,"description":"Too Many Requests: retry after 13","parameters":{"retry_after":13}}`)
	got, ok := parseTelegramRetryAfter(err)
	if !ok {
		t.Fatalf("expected retry-after parse success")
	}
	if got != 13*time.Second {
		t.Fatalf("expected 13s, got %s", got)
	}
}

func TestParseTelegramRetryAfterFromDescription(t *testing.T) {
	err := errors.New("telegram sendAudio failed: 429 Too Many Requests: retry after 7")
	got, ok := parseTelegramRetryAfter(err)
	if !ok {
		t.Fatalf("expected retry-after parse success")
	}
	if got != 7*time.Second {
		t.Fatalf("expected 7s, got %s", got)
	}
}

func TestPendingSelectionIsolatedByMessageID(t *testing.T) {
	chatID := int64(1001)
	b := &TelegramBot{
		pending: make(map[int64]map[int]*PendingSelection),
	}

	b.setPending(chatID, "song", "q1", "us", 0, []apputils.SearchResultItem{{ID: "s1"}}, false, 11, 101, "")
	b.setPending(chatID, "song", "q2", "us", 0, []apputils.SearchResultItem{{ID: "s2"}}, false, 12, 102, "")

	pending1, ok := b.getPending(chatID, 101)
	if !ok {
		t.Fatalf("expected pending for message 101")
	}
	if pending1.Query != "q1" || pending1.ReplyToMessageID != 11 {
		t.Fatalf("unexpected pending1: %+v", pending1)
	}

	pending2, ok := b.getPending(chatID, 102)
	if !ok {
		t.Fatalf("expected pending for message 102")
	}
	if pending2.Query != "q2" || pending2.ReplyToMessageID != 12 {
		t.Fatalf("unexpected pending2: %+v", pending2)
	}

	b.clearPendingByMessage(chatID, 101)
	if _, ok := b.getPending(chatID, 101); ok {
		t.Fatalf("message 101 pending should be cleared")
	}
	if _, ok := b.getPending(chatID, 102); !ok {
		t.Fatalf("message 102 pending should remain")
	}
}

func TestPendingTransferIsolatedByMessageID(t *testing.T) {
	chatID := int64(2001)
	b := &TelegramBot{
		pendingTransfers: make(map[int64]map[int]*PendingTransfer),
	}

	b.setPendingTransfer(chatID, mediaTypeAlbum, "a1", "Album 1", "us", 21, 201, false)
	b.setPendingTransfer(chatID, mediaTypePlaylist, "p1", "Playlist 1", "us", 22, 202, false)

	pending1, ok := b.getPendingTransfer(chatID, 201)
	if !ok {
		t.Fatalf("expected pending transfer for message 201")
	}
	if pending1.MediaID != "a1" || pending1.ReplyToMessageID != 21 {
		t.Fatalf("unexpected pending transfer 201: %+v", pending1)
	}

	pending2, ok := b.getPendingTransfer(chatID, 202)
	if !ok {
		t.Fatalf("expected pending transfer for message 202")
	}
	if pending2.MediaID != "p1" || pending2.ReplyToMessageID != 22 {
		t.Fatalf("unexpected pending transfer 202: %+v", pending2)
	}

	b.clearPendingTransferByMessage(chatID, 201)
	if _, ok := b.getPendingTransfer(chatID, 201); ok {
		t.Fatalf("message 201 transfer should be cleared")
	}
	if _, ok := b.getPendingTransfer(chatID, 202); !ok {
		t.Fatalf("message 202 transfer should remain")
	}
}

func TestPendingArtistModeIsolatedByMessageID(t *testing.T) {
	chatID := int64(3001)
	b := &TelegramBot{
		pendingArtistModes: make(map[int64]map[int]*PendingArtistMode),
	}

	b.setPendingArtistMode(chatID, "artist-a", "Artist A", "us", 31, 301)
	b.setPendingArtistMode(chatID, "artist-b", "Artist B", "us", 32, 302)

	pending1, ok := b.getPendingArtistMode(chatID, 301)
	if !ok {
		t.Fatalf("expected pending artist mode for message 301")
	}
	if pending1.ArtistID != "artist-a" || pending1.ReplyToMessageID != 31 {
		t.Fatalf("unexpected pending artist mode 301: %+v", pending1)
	}

	pending2, ok := b.getPendingArtistMode(chatID, 302)
	if !ok {
		t.Fatalf("expected pending artist mode for message 302")
	}
	if pending2.ArtistID != "artist-b" || pending2.ReplyToMessageID != 32 {
		t.Fatalf("unexpected pending artist mode 302: %+v", pending2)
	}

	b.clearPendingArtistModeByMessage(chatID, 301)
	if _, ok := b.getPendingArtistMode(chatID, 301); ok {
		t.Fatalf("message 301 artist mode should be cleared")
	}
	if _, ok := b.getPendingArtistMode(chatID, 302); !ok {
		t.Fatalf("message 302 artist mode should remain")
	}
}

func TestHasSongAutoExtras(t *testing.T) {
	if hasSongAutoExtras(ChatDownloadSettings{}) {
		t.Fatalf("expected false for empty settings")
	}
	if !hasSongAutoExtras(ChatDownloadSettings{AutoLyrics: true, SettingsInited: true}) {
		t.Fatalf("expected true when AutoLyrics enabled")
	}
	if !hasSongAutoExtras(ChatDownloadSettings{AutoCover: true, SettingsInited: true}) {
		t.Fatalf("expected true when AutoCover enabled")
	}
	if !hasSongAutoExtras(ChatDownloadSettings{AutoAnimated: true, SettingsInited: true}) {
		t.Fatalf("expected true when AutoAnimated enabled")
	}
}

func TestAcquireReleaseInflightDownload(t *testing.T) {
	b := &TelegramBot{
		inflightDownloads: make(map[string]struct{}),
	}
	key := "chat|song|123"
	if !b.acquireInflightDownload(key) {
		t.Fatalf("expected first acquire success")
	}
	if b.acquireInflightDownload(key) {
		t.Fatalf("expected second acquire to be blocked")
	}
	b.releaseInflightDownload(key)
	if !b.acquireInflightDownload(key) {
		t.Fatalf("expected acquire after release to succeed")
	}
}

func TestMakeDownloadInflightKeyIncludesSettings(t *testing.T) {
	base := ChatDownloadSettings{
		Format:         telegramFormatAlac,
		AACType:        "aac",
		MVAudioType:    "atmos",
		LyricsFormat:   "lrc",
		AutoLyrics:     false,
		AutoCover:      false,
		AutoAnimated:   false,
		SettingsInited: true,
	}
	keyA := makeDownloadInflightKey(100, mediaTypeSong, "123", "us", transferModeOneByOne, base)
	base.AutoLyrics = true
	keyB := makeDownloadInflightKey(100, mediaTypeSong, "123", "us", transferModeOneByOne, base)
	if keyA == keyB {
		t.Fatalf("expected different keys when settings differ")
	}
}

func TestNormalizeTelegramBotCommandRefreshAlias(t *testing.T) {
	if got := normalizeTelegramBotCommand("rf"); got != "refresh" {
		t.Fatalf("expected rf alias to map to refresh, got %q", got)
	}
}

func TestResolveRefreshURLTargetSupportsURLPrefixes(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{
			name: "direct",
			args: []string{"https://music.apple.com/us/song/example/123456789"},
		},
		{
			name: "url-prefix",
			args: []string{"url", "https://music.apple.com/us/song/example/123456789"},
		},
		{
			name: "ulr-prefix",
			args: []string{"ulr", "https://music.apple.com/us/song/example/123456789"},
		},
	}
	for _, tt := range tests {
		target, err := resolveRefreshURLTarget(tt.args)
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", tt.name, err)
		}
		if target.MediaType != mediaTypeSong || target.ID != "123456789" || target.Storefront != "us" {
			t.Fatalf("%s: unexpected target: %+v", tt.name, target)
		}
	}
}

func TestPurgeTargetCachesSongClearsAudioAndBundleZip(t *testing.T) {
	b := &TelegramBot{
		cache: map[string]CachedAudio{
			"song-1|alac|false": {FileID: "a1"},
			"song-1|flac|true":  {FileID: "a2"},
			"song-2|alac|false": {FileID: "a3"},
		},
		docCache: map[string]CachedDocument{
			"song:song-1|profile-a|zip":   {FileID: "d1"},
			"song:song-1|profile-b|zip":   {FileID: "d2"},
			"album:album-1|profile-a|zip": {FileID: "d3"},
		},
		videoCache: map[string]CachedVideo{},
	}

	removed := b.purgeTargetCaches(&AppleURLTarget{MediaType: mediaTypeSong, ID: "song-1"})
	if removed != 4 {
		t.Fatalf("expected 4 removed cache entries, got %d", removed)
	}
	if len(b.cache) != 1 {
		t.Fatalf("expected unrelated song cache to remain, got %#v", b.cache)
	}
	if _, ok := b.cache["song-2|alac|false"]; !ok {
		t.Fatalf("expected unrelated audio cache to remain")
	}
	if len(b.docCache) != 1 {
		t.Fatalf("expected unrelated bundle cache to remain, got %#v", b.docCache)
	}
	if _, ok := b.docCache["album:album-1|profile-a|zip"]; !ok {
		t.Fatalf("expected unrelated album zip cache to remain")
	}
}

func TestPurgeTargetCachesMusicVideoClearsVideoAndDocument(t *testing.T) {
	b := &TelegramBot{
		cache: map[string]CachedAudio{},
		docCache: map[string]CachedDocument{
			"music-video:mv-1|profile-a|document": {FileID: "d1"},
			"song:song-1|profile-a|zip":           {FileID: "d2"},
		},
		videoCache: map[string]CachedVideo{
			"music-video:mv-1|profile-a|video": {FileID: "v1"},
			"music-video:mv-2|profile-a|video": {FileID: "v2"},
		},
	}

	removed := b.purgeTargetCaches(&AppleURLTarget{MediaType: mediaTypeMusicVideo, ID: "mv-1"})
	if removed != 2 {
		t.Fatalf("expected 2 removed cache entries, got %d", removed)
	}
	if _, ok := b.docCache["song:song-1|profile-a|zip"]; !ok {
		t.Fatalf("expected unrelated document cache to remain")
	}
	if _, ok := b.videoCache["music-video:mv-2|profile-a|video"]; !ok {
		t.Fatalf("expected unrelated video cache to remain")
	}
}

func TestTelegramSendLimiterNextWait(t *testing.T) {
	limiter := newTelegramSendLimiter(2*time.Second, 4*time.Second)
	if limiter == nil {
		t.Fatalf("expected limiter")
	}
	now := time.Unix(1000, 0)
	limiter.lastAll = now.Add(-1 * time.Second)
	limiter.lastChat[42] = now.Add(-500 * time.Millisecond)
	wait := limiter.nextWaitLocked(now, 42)
	expected := 3500 * time.Millisecond
	if wait != expected {
		t.Fatalf("wait mismatch: got %s want %s", wait, expected)
	}
}
