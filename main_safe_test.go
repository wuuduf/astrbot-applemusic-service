package main

import (
	"errors"
	"testing"
	"time"

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
