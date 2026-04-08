package utils

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"time"

	nethttp "github.com/wuuduf/astrbot-applemusic-service/utils/nethttp"
)

type ArtistRelationshipPage struct {
	Next string `json:"next"`
	Data []struct {
		ID         string `json:"id"`
		Type       string `json:"type"`
		Href       string `json:"href"`
		Attributes struct {
			Previews []struct {
				URL string `json:"url"`
			} `json:"previews"`
			Artwork struct {
				Width      int    `json:"width"`
				Height     int    `json:"height"`
				URL        string `json:"url"`
				BgColor    string `json:"bgColor"`
				TextColor1 string `json:"textColor1"`
				TextColor2 string `json:"textColor2"`
				TextColor3 string `json:"textColor3"`
				TextColor4 string `json:"textColor4"`
			} `json:"artwork"`
			ArtistName           string   `json:"artistName"`
			URL                  string   `json:"url"`
			DiscNumber           int      `json:"discNumber"`
			GenreNames           []string `json:"genreNames"`
			HasTimeSyncedLyrics  bool     `json:"hasTimeSyncedLyrics"`
			IsMasteredForItunes  bool     `json:"isMasteredForItunes"`
			IsAppleDigitalMaster bool     `json:"isAppleDigitalMaster"`
			ContentRating        string   `json:"contentRating"`
			DurationInMillis     int      `json:"durationInMillis"`
			ReleaseDate          string   `json:"releaseDate"`
			Name                 string   `json:"name"`
			Isrc                 string   `json:"isrc"`
			AudioTraits          []string `json:"audioTraits"`
			HasLyrics            bool     `json:"hasLyrics"`
			AlbumName            string   `json:"albumName"`
			PlayParams           struct {
				ID   string `json:"id"`
				Kind string `json:"kind"`
			} `json:"playParams"`
			TrackNumber  int    `json:"trackNumber"`
			AudioLocale  string `json:"audioLocale"`
			ComposerName string `json:"composerName"`
		} `json:"attributes"`
	} `json:"data"`
}

func fetchArtistRelationship(storefront, artistID, token, relationship, itemType string, limit int, pageOffset int, language string) ([]SearchResultItem, bool, error) {
	apiOffset := 0
	items := []SearchResultItem{}
	for {
		req, err := http.NewRequest("GET", fmt.Sprintf("https://amp-api.music.apple.com/v1/catalog/%s/artists/%s/%s", storefront, artistID, relationship), nil)
		if err != nil {
			return nil, false, err
		}
		req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", token))
		req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.124 Safari/537.36")
		req.Header.Set("Origin", "https://music.apple.com")
		query := url.Values{}
		query.Set("limit", "100")
		query.Set("offset", strconv.Itoa(apiOffset))
		if language != "" {
			query.Set("l", language)
		}
		req.URL.RawQuery = query.Encode()
		resp, err := nethttp.Do(req)
		if err != nil {
			return nil, false, err
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return nil, false, fmt.Errorf("artist %s request failed: %s", relationship, resp.Status)
		}
		obj := new(ArtistRelationshipPage)
		if err := json.NewDecoder(resp.Body).Decode(&obj); err != nil {
			resp.Body.Close()
			return nil, false, err
		}
		resp.Body.Close()
		for _, item := range obj.Data {
			items = append(items, SearchResultItem{
				Type:          itemType,
				Name:          item.Attributes.Name,
				Detail:        item.Attributes.ReleaseDate,
				URL:           item.Attributes.URL,
				ID:            item.ID,
				ContentRating: item.Attributes.ContentRating,
				Artist:        item.Attributes.ArtistName,
				Album:         item.Attributes.AlbumName,
			})
		}
		apiOffset += 100
		if obj.Next == "" {
			break
		}
	}
	sort.Slice(items, func(i, j int) bool {
		di, err1 := time.Parse("2006-01-02", items[i].Detail)
		dj, err2 := time.Parse("2006-01-02", items[j].Detail)
		if err1 != nil || err2 != nil {
			return items[i].Name < items[j].Name
		}
		return di.After(dj)
	})
	if pageOffset < 0 {
		pageOffset = 0
	}
	if limit <= 0 {
		return items, false, nil
	}
	if pageOffset >= len(items) {
		return []SearchResultItem{}, false, nil
	}
	end := pageOffset + limit
	if end > len(items) {
		end = len(items)
	}
	hasNext := end < len(items)
	return items[pageOffset:end], hasNext, nil
}

func FetchArtistAlbums(storefront, artistID, token string, limit int, pageOffset int, language string) ([]SearchResultItem, bool, error) {
	return fetchArtistRelationship(storefront, artistID, token, "albums", "Album", limit, pageOffset, language)
}

func FetchArtistMusicVideos(storefront, artistID, token string, limit int, pageOffset int, language string) ([]SearchResultItem, bool, error) {
	return fetchArtistRelationship(storefront, artistID, token, "music-videos", "Music Video", limit, pageOffset, language)
}
