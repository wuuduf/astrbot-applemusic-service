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

	b.setPendingTransfer(chatID, mediaTypeAlbum, "a1", "Album 1", "us", 21, 201)
	b.setPendingTransfer(chatID, mediaTypePlaylist, "p1", "Playlist 1", "us", 22, 202)

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
