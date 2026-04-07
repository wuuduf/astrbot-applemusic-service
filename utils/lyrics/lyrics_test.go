package lyrics

import (
	"errors"
	"testing"

	"github.com/wuuduf/astrbot-applemusic-service/utils/safe"
)

func TestExtractLyricsPayloadEmptyData(t *testing.T) {
	_, err := extractLyricsPayload(&SongLyrics{})
	if err == nil {
		t.Fatalf("expected error for empty lyrics data")
	}
	var accessErr *safe.AccessError
	if !errors.As(err, &accessErr) {
		t.Fatalf("expected safe.AccessError, got %T", err)
	}
}
