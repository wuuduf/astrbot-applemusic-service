package main

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	sharedcatalog "github.com/wuuduf/astrbot-applemusic-service/internal/catalog"
	sharedstorage "github.com/wuuduf/astrbot-applemusic-service/internal/storage"
	apputils "github.com/wuuduf/astrbot-applemusic-service/utils"
	"github.com/wuuduf/astrbot-applemusic-service/utils/ampapi"
	"github.com/wuuduf/astrbot-applemusic-service/utils/cmdrunner"
	"github.com/wuuduf/astrbot-applemusic-service/utils/safe"
	"github.com/wuuduf/astrbot-applemusic-service/utils/structs"
	"github.com/wuuduf/astrbot-applemusic-service/utils/task"

	"github.com/fatih/color"
	"github.com/grafov/m3u8"
	"github.com/olekukonko/tablewriter"
	"github.com/spf13/pflag"
	"github.com/zhaarey/go-mp4tag"
	"gopkg.in/yaml.v2"
)

var (
	forbiddenNames    = regexp.MustCompile(`[/\\<>:"|?*]`)
	retryAfterJSONRe  = regexp.MustCompile(`(?i)"retry_after"\s*:\s*(\d+)`)
	retryAfterTextRe  = regexp.MustCompile(`(?i)retry\s+after\s+(\d+)`)
	artist_select     bool
	debug_mode        bool
	alac_max          *int
	atmos_max         *int
	mv_max            *int
	mv_audio_type     *string
	aac_type          *string
	Config            structs.ConfigSet
	searchMetaMu      sync.Mutex
	searchMetaByID    = make(map[string]AudioMeta)
	networkHTTPClient = &http.Client{Timeout: 45 * time.Second}
)

type AudioMeta struct {
	TrackID        string
	Title          string
	Performer      string
	DurationMillis int64
}

type CachedAudio struct {
	FileID         string    `json:"file_id"`
	FileSize       int64     `json:"file_size"`
	Compressed     bool      `json:"compressed"`
	Format         string    `json:"format,omitempty"`
	SizeBytes      int64     `json:"size_bytes,omitempty"`
	BitrateKbps    float64   `json:"bitrate_kbps,omitempty"`
	DurationMillis int64     `json:"duration_millis,omitempty"`
	Title          string    `json:"title,omitempty"`
	Performer      string    `json:"performer,omitempty"`
	UpdatedAt      time.Time `json:"updated_at,omitempty"`
}

type CachedDocument struct {
	FileID    string    `json:"file_id"`
	FileSize  int64     `json:"file_size,omitempty"`
	UpdatedAt time.Time `json:"updated_at,omitempty"`
}

type CachedVideo struct {
	FileID    string    `json:"file_id"`
	FileSize  int64     `json:"file_size,omitempty"`
	UpdatedAt time.Time `json:"updated_at,omitempty"`
}

type DownloadSession struct {
	Config              structs.ConfigSet
	DlAtmos             bool
	DlAac               bool
	DlSelect            bool
	DlSong              bool
	ForceRedownload     bool
	Counter             structs.Counter
	OkDict              map[string][]int
	LastDownloadedPaths []string
	DownloadedMeta      map[string]AudioMeta
	ActiveProgress      func(phase string, done, total int64)
	StaticCoverDownload bool

	downloadedMetaMu sync.Mutex
}

type JobContext struct {
	Session        *DownloadSession
	Token          string
	Storefront     string
	MediaUserToken string
}

func newDownloadSession(base structs.ConfigSet) *DownloadSession {
	return &DownloadSession{
		Config:              base,
		OkDict:              make(map[string][]int),
		DownloadedMeta:      make(map[string]AudioMeta),
		StaticCoverDownload: true,
	}
}

func (s *DownloadSession) resetState() {
	if s == nil {
		return
	}
	s.DlAtmos = false
	s.DlAac = false
	s.DlSelect = false
	s.DlSong = false
	s.Counter = structs.Counter{}
	s.OkDict = make(map[string][]int)
	s.clearDownloadState()
}

func (s *DownloadSession) recordDownloadedTrack(track *task.Track) {
	if s == nil || track == nil || track.SavePath == "" {
		return
	}
	meta := AudioMeta{
		TrackID:        strings.TrimSpace(track.ID),
		Title:          strings.TrimSpace(track.Resp.Attributes.Name),
		Performer:      strings.TrimSpace(track.Resp.Attributes.ArtistName),
		DurationMillis: int64(track.Resp.Attributes.DurationInMillis),
	}
	if meta.TrackID != "" {
		if override, ok := popSearchMeta(meta.TrackID); ok {
			if override.Title != "" {
				meta.Title = override.Title
			}
			if override.Performer != "" {
				meta.Performer = override.Performer
			}
		}
	}
	s.recordDownloadedFile(track.SavePath, meta)
}

func (s *DownloadSession) recordDownloadedFile(path string, meta AudioMeta) {
	if s == nil || strings.TrimSpace(path) == "" {
		return
	}
	s.LastDownloadedPaths = append(s.LastDownloadedPaths, path)
	s.downloadedMetaMu.Lock()
	s.DownloadedMeta[path] = meta
	s.downloadedMetaMu.Unlock()
}

func (s *DownloadSession) getDownloadedMeta(path string) (AudioMeta, bool) {
	if s == nil {
		return AudioMeta{}, false
	}
	s.downloadedMetaMu.Lock()
	defer s.downloadedMetaMu.Unlock()
	meta, ok := s.DownloadedMeta[path]
	return meta, ok
}

func (s *DownloadSession) clearDownloadState() {
	if s == nil {
		return
	}
	s.LastDownloadedPaths = nil
	s.downloadedMetaMu.Lock()
	s.DownloadedMeta = make(map[string]AudioMeta)
	s.downloadedMetaMu.Unlock()
	debug.FreeOSMemory()
}

func (s *DownloadSession) shouldDownloadStaticCover() bool {
	if s == nil {
		return true
	}
	return s.StaticCoverDownload
}

func (s *DownloadSession) shouldReuseExistingFiles() bool {
	if s == nil {
		return true
	}
	return !s.ForceRedownload
}

type telegramCacheFile struct {
	Version   int                       `json:"version"`
	Items     map[string]CachedAudio    `json:"items"`
	Documents map[string]CachedDocument `json:"documents,omitempty"`
	Videos    map[string]CachedVideo    `json:"videos,omitempty"`
}

func loadConfig() error {
	data, err := os.ReadFile("config.yaml")
	if err != nil {
		return err
	}
	err = yaml.Unmarshal(data, &Config)
	if err != nil {
		return err
	}
	if len(Config.Storefront) != 2 {
		Config.Storefront = "us"
	}
	return nil
}

func setSearchMeta(trackID string, title string, performer string) {
	trackID = strings.TrimSpace(trackID)
	if trackID == "" {
		return
	}
	meta := AudioMeta{
		TrackID:   trackID,
		Title:     strings.TrimSpace(title),
		Performer: strings.TrimSpace(performer),
	}
	if meta.Title == "" && meta.Performer == "" {
		return
	}
	searchMetaMu.Lock()
	searchMetaByID[trackID] = meta
	searchMetaMu.Unlock()
}

func popSearchMeta(trackID string) (AudioMeta, bool) {
	searchMetaMu.Lock()
	defer searchMetaMu.Unlock()
	meta, ok := searchMetaByID[trackID]
	if ok {
		delete(searchMetaByID, trackID)
	}
	return meta, ok
}

func LimitString(s string) string {
	return LimitStringWithConfig(&Config, s)
}

func LimitStringWithConfig(cfg *structs.ConfigSet, s string) string {
	if cfg == nil {
		return s
	}
	if len([]rune(s)) > cfg.LimitMax {
		return string([]rune(s)[:cfg.LimitMax])
	}
	return s
}

func isInArray(arr []int, target int) bool {
	for _, num := range arr {
		if num == target {
			return true
		}
	}
	return false
}

func fileExists(path string) (bool, error) {
	f, err := os.Stat(path)
	if err == nil {
		return !f.IsDir(), nil
	} else if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

func checkUrl(url string) (string, string) {
	pat := regexp.MustCompile(`^(?:https:\/\/(?:beta\.music|music|classical\.music)\.apple\.com\/(\w{2})(?:\/album|\/album\/.+))\/(?:id)?(\d[^\D]+)(?:$|\?)`)
	matches := pat.FindAllStringSubmatch(url, -1)

	if matches == nil {
		return "", ""
	} else {
		return matches[0][1], matches[0][2]
	}
}
func checkUrlMv(url string) (string, string) {
	pat := regexp.MustCompile(`^(?:https:\/\/(?:beta\.music|music)\.apple\.com\/(\w{2})(?:\/music-video|\/music-video\/.+))\/(?:id)?(\d[^\D]+)(?:$|\?)`)
	matches := pat.FindAllStringSubmatch(url, -1)

	if matches == nil {
		return "", ""
	} else {
		return matches[0][1], matches[0][2]
	}
}
func checkUrlSong(url string) (string, string) {
	pat := regexp.MustCompile(`^(?:https:\/\/(?:beta\.music|music|classical\.music)\.apple\.com\/(\w{2})(?:\/song|\/song\/.+))\/(?:id)?(\d[^\D]+)(?:$|\?)`)
	matches := pat.FindAllStringSubmatch(url, -1)

	if matches == nil {
		return "", ""
	} else {
		return matches[0][1], matches[0][2]
	}
}
func checkUrlPlaylist(url string) (string, string) {
	pat := regexp.MustCompile(`^(?:https:\/\/(?:beta\.music|music|classical\.music)\.apple\.com\/(\w{2})(?:\/playlist|\/playlist\/.+))\/(?:id)?(pl\.[\w-]+)(?:$|\?)`)
	matches := pat.FindAllStringSubmatch(url, -1)

	if matches == nil {
		return "", ""
	} else {
		return matches[0][1], matches[0][2]
	}
}

func checkUrlStation(url string) (string, string) {
	pat := regexp.MustCompile(`^(?:https:\/\/(?:beta\.music|music)\.apple\.com\/(\w{2})(?:\/station|\/station\/.+))\/(?:id)?(ra\.[\w-]+)(?:$|\?)`)
	matches := pat.FindAllStringSubmatch(url, -1)

	if matches == nil {
		return "", ""
	} else {
		return matches[0][1], matches[0][2]
	}
}

func checkUrlArtist(url string) (string, string) {
	pat := regexp.MustCompile(`^(?:https:\/\/(?:beta\.music|music|classical\.music)\.apple\.com\/(\w{2})(?:\/artist|\/artist\/.+))\/(?:id)?(\d[^\D]+)(?:$|\?)`)
	matches := pat.FindAllStringSubmatch(url, -1)

	if matches == nil {
		return "", ""
	} else {
		return matches[0][1], matches[0][2]
	}
}

type AppleURLTarget struct {
	MediaType  string
	Storefront string
	ID         string
	RawURL     string
}

func parseAppleMusicURL(raw string) (*AppleURLTarget, error) {
	cleaned := strings.TrimSpace(raw)
	if cleaned == "" {
		return nil, fmt.Errorf("empty URL")
	}
	if storefront, id := checkUrlSong(cleaned); storefront != "" && id != "" {
		return &AppleURLTarget{MediaType: mediaTypeSong, Storefront: storefront, ID: id, RawURL: cleaned}, nil
	}
	if storefront, id := checkUrlMv(cleaned); storefront != "" && id != "" {
		return &AppleURLTarget{MediaType: mediaTypeMusicVideo, Storefront: storefront, ID: id, RawURL: cleaned}, nil
	}
	if storefront, id := checkUrlPlaylist(cleaned); storefront != "" && id != "" {
		return &AppleURLTarget{MediaType: mediaTypePlaylist, Storefront: storefront, ID: id, RawURL: cleaned}, nil
	}
	if storefront, id := checkUrlStation(cleaned); storefront != "" && id != "" {
		return &AppleURLTarget{MediaType: mediaTypeStation, Storefront: storefront, ID: id, RawURL: cleaned}, nil
	}
	if storefront, id := checkUrlArtist(cleaned); storefront != "" && id != "" {
		return &AppleURLTarget{MediaType: mediaTypeArtist, Storefront: storefront, ID: id, RawURL: cleaned}, nil
	}
	if storefront, id := checkUrl(cleaned); storefront != "" && id != "" {
		parseResult, err := url.Parse(cleaned)
		if err == nil {
			if songID := strings.TrimSpace(parseResult.Query().Get("i")); songID != "" {
				return &AppleURLTarget{MediaType: mediaTypeSong, Storefront: storefront, ID: songID, RawURL: cleaned}, nil
			}
		}
		return &AppleURLTarget{MediaType: mediaTypeAlbum, Storefront: storefront, ID: id, RawURL: cleaned}, nil
	}
	return nil, fmt.Errorf("unsupported Apple Music URL")
}

func extractFirstAppleMusicURL(text string) string {
	if strings.TrimSpace(text) == "" {
		return ""
	}
	tokens := strings.Fields(text)
	for _, token := range tokens {
		candidate := strings.TrimSpace(token)
		candidate = strings.Trim(candidate, "<>()[]{}\"'“”‘’")
		candidate = strings.TrimRight(candidate, ".,!?")
		if candidate == "" {
			continue
		}
		lower := strings.ToLower(candidate)
		if !strings.HasPrefix(lower, "http://") && !strings.HasPrefix(lower, "https://") {
			continue
		}
		if strings.Contains(lower, "music.apple.com/") || strings.Contains(lower, "beta.music.apple.com/") || strings.Contains(lower, "classical.music.apple.com/") {
			return candidate
		}
	}
	return ""
}
func getUrlSong(songUrl string, token string) (string, error) {
	storefront, songId := checkUrlSong(songUrl)
	manifest, err := ampapi.GetSongResp(storefront, songId, Config.Language, token)
	if err != nil {
		fmt.Println("\u26A0 Failed to get manifest:", err)
		return "", err
	}
	item, err := firstSongData("main.getUrlSong", manifest)
	if err != nil {
		return "", err
	}
	albumId, err := firstSongAlbumID("main.getUrlSong", item)
	if err != nil {
		return "", err
	}
	songAlbumUrl := fmt.Sprintf("https://music.apple.com/%s/album/1/%s?i=%s", storefront, albumId, songId)
	return songAlbumUrl, nil
}

func catalogServiceForToken(token string, opPrefix string) *sharedcatalog.Service {
	return &sharedcatalog.Service{
		AppleToken:     token,
		MediaUserToken: Config.MediaUserToken,
		Language:       Config.Language,
		HTTPClient:     networkHTTPClient,
		UserAgent:      "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.124 Safari/537.36",
		OpPrefix:       strings.TrimSpace(opPrefix),
	}
}

func getUrlArtistName(artistUrl string, token string) (string, string, error) {
	storefront, artistId := checkUrlArtist(artistUrl)
	name, _, err := catalogServiceForToken(token, "main.artist").FetchArtistProfile(storefront, artistId)
	if err != nil {
		return "", "", err
	}
	return name, artistId, nil
}

func checkArtist(artistUrl string, token string, relationship string) ([]string, error) {
	storefront, artistId := checkUrlArtist(artistUrl)
	var args []string
	var urls []string
	var options [][]string
	items, err := catalogServiceForToken(token, "main.artist").FetchArtistRelationshipAll(storefront, artistId, relationship)
	if err != nil {
		return nil, err
	}
	for _, item := range items {
		options = append(options, []string{item.Name, item.ReleaseDate, item.ID, item.URL})
	}
	sort.Slice(options, func(i, j int) bool {
		// 将日期字符串解析为 time.Time 类型进行比较
		dateI, _ := time.Parse("2006-01-02", options[i][1])
		dateJ, _ := time.Parse("2006-01-02", options[j][1])
		return dateI.Before(dateJ) // 返回 true 表示 i 在 j 前面
	})

	table := tablewriter.NewWriter(os.Stdout)
	if relationship == "albums" {
		table.SetHeader([]string{"", "Album Name", "Date", "Album ID"})
	} else if relationship == "music-videos" {
		table.SetHeader([]string{"", "MV Name", "Date", "MV ID"})
	}
	table.SetRowLine(false)
	table.SetHeaderColor(tablewriter.Colors{},
		tablewriter.Colors{tablewriter.FgRedColor, tablewriter.Bold},
		tablewriter.Colors{tablewriter.Bold, tablewriter.FgBlackColor},
		tablewriter.Colors{tablewriter.Bold, tablewriter.FgBlackColor})

	table.SetColumnColor(tablewriter.Colors{tablewriter.FgCyanColor},
		tablewriter.Colors{tablewriter.Bold, tablewriter.FgRedColor},
		tablewriter.Colors{tablewriter.Bold, tablewriter.FgBlackColor},
		tablewriter.Colors{tablewriter.Bold, tablewriter.FgBlackColor})
	for i, v := range options {
		urls = append(urls, v[3])
		options[i] = append([]string{fmt.Sprint(i + 1)}, v[:3]...)
		table.Append(options[i])
	}
	table.Render()
	if artist_select {
		fmt.Println("You have selected all options:")
		return urls, nil
	}
	reader := bufio.NewReader(os.Stdin)
	fmt.Println("Please select from the " + relationship + " options above (multiple options separated by commas, ranges supported, or type 'all' to select all)")
	cyanColor := color.New(color.FgCyan)
	cyanColor.Print("Enter your choice: ")
	input, _ := reader.ReadString('\n')

	input = strings.TrimSpace(input)
	if input == "all" {
		fmt.Println("You have selected all options:")
		return urls, nil
	}

	selectedOptions := [][]string{}
	parts := strings.Split(input, ",")
	for _, part := range parts {
		if strings.Contains(part, "-") {
			rangeParts := strings.Split(part, "-")
			selectedOptions = append(selectedOptions, rangeParts)
		} else {
			selectedOptions = append(selectedOptions, []string{part})
		}
	}

	fmt.Println("You have selected the following options:")
	for _, opt := range selectedOptions {
		if len(opt) == 1 {
			num, err := strconv.Atoi(opt[0])
			if err != nil {
				fmt.Println("Invalid option:", opt[0])
				continue
			}
			if num > 0 && num <= len(options) {
				fmt.Println(options[num-1])
				args = append(args, urls[num-1])
			} else {
				fmt.Println("Option out of range:", opt[0])
			}
		} else if len(opt) == 2 {
			start, err1 := strconv.Atoi(opt[0])
			end, err2 := strconv.Atoi(opt[1])
			if err1 != nil || err2 != nil {
				fmt.Println("Invalid range:", opt)
				continue
			}
			if start < 1 || end > len(options) || start > end {
				fmt.Println("Range out of range:", opt)
				continue
			}
			for i := start; i <= end; i++ {
				fmt.Println(options[i-1])
				args = append(args, urls[i-1])
			}
		} else {
			fmt.Println("Invalid option:", opt)
		}
	}
	return args, nil
}

func writeCover(sanAlbumFolder, name string, url string) (string, error) {
	return writeCoverWithConfig(sanAlbumFolder, name, url, &Config)
}

func writeCoverWithConfig(sanAlbumFolder, name string, url string, cfg *structs.ConfigSet) (string, error) {
	if cfg == nil {
		cfg = &Config
	}
	originalUrl := url
	var ext string
	var covPath string
	if cfg.CoverFormat == "original" {
		ext = strings.Split(url, "/")[len(strings.Split(url, "/"))-2]
		ext = ext[strings.LastIndex(ext, ".")+1:]
		covPath = filepath.Join(sanAlbumFolder, name+"."+ext)
	} else {
		covPath = filepath.Join(sanAlbumFolder, name+"."+cfg.CoverFormat)
	}
	exists, err := fileExists(covPath)
	if err != nil {
		fmt.Println("Failed to check if cover exists.")
		return "", err
	}
	if exists {
		_ = os.Remove(covPath)
	}
	if cfg.CoverFormat == "png" {
		re := regexp.MustCompile(`\{w\}x\{h\}`)
		parts := re.Split(url, 2)
		url = parts[0] + "{w}x{h}" + strings.Replace(parts[1], ".jpg", ".png", 1)
	}
	url = strings.Replace(url, "{w}x{h}", cfg.CoverSize, 1)
	if cfg.CoverFormat == "original" {
		url = strings.Replace(url, "is1-ssl.mzstatic.com/image/thumb", "a5.mzstatic.com/us/r1000/0", 1)
		url = url[:strings.LastIndex(url, "/")]
	}
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.124 Safari/537.36")
	do, err := networkHTTPClient.Do(req)
	if err != nil {
		return "", err
	}
	defer do.Body.Close()
	if do.StatusCode != http.StatusOK {
		if cfg.CoverFormat == "original" {
			fmt.Println("Failed to get cover, falling back to " + ext + " url.")
			splitByDot := strings.Split(originalUrl, ".")
			last := splitByDot[len(splitByDot)-1]
			fallback := originalUrl[:len(originalUrl)-len(last)] + ext
			fallback = strings.Replace(fallback, "{w}x{h}", cfg.CoverSize, 1)
			fmt.Println("Fallback URL:", fallback)
			req, err = http.NewRequest("GET", fallback, nil)
			if err != nil {
				fmt.Println("Failed to create request for fallback url.")
				return "", err
			}
			req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.124 Safari/537.36")
			do, err = networkHTTPClient.Do(req)
			if err != nil {
				fmt.Println("Failed to get cover from fallback url.")
				return "", err
			}
			defer do.Body.Close()
			if do.StatusCode != http.StatusOK {
				fmt.Println(fallback)
				return "", errors.New(do.Status)
			}
		} else {
			return "", errors.New(do.Status)
		}
	}
	f, err := os.Create(covPath)
	if err != nil {
		return "", err
	}
	defer f.Close()
	_, err = io.Copy(f, do.Body)
	if err != nil {
		return "", err
	}
	return covPath, nil
}

func writeLyrics(sanAlbumFolder, filename string, lrc string) error {
	lyricspath := filepath.Join(sanAlbumFolder, filename)
	f, err := os.Create(lyricspath)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(lrc)
	if err != nil {
		return err
	}
	return nil
}

func contains(slice []string, item string) bool {
	for _, v := range slice {
		if v == item {
			return true
		}
	}
	return false
}

func setDlFlags(session *DownloadSession, quality string) {
	if session == nil {
		return
	}
	session.DlAtmos = false
	session.DlAac = false

	switch quality {
	case "atmos":
		session.DlAtmos = true
		fmt.Println("Quality set to: Dolby Atmos")
	case "aac":
		session.DlAac = true
		session.Config.AacType = "aac"
		fmt.Println("Quality set to: High-Quality (AAC)")
	case "alac":
		fmt.Println("Quality set to: Lossless (ALAC)")
	}
}

func handleSearch(session *DownloadSession, searchType string, queryParts []string, token string) (string, error) {
	if session == nil {
		return "", fmt.Errorf("download session is nil")
	}
	selection, err := apputils.HandleSearch(searchType, queryParts, token, session.Config.Storefront, session.Config.Language)
	if err != nil {
		return "", err
	}
	if selection == nil || selection.URL == "" {
		return "", nil
	}
	if selection.IsSong {
		session.DlSong = true
	}
	if selection.Quality != "" && selection.Quality != "default" {
		setDlFlags(session, selection.Quality)
	}
	return selection.URL, nil
}

func convertIfNeeded(session *DownloadSession, track *task.Track, lrc string) {
	if session == nil {
		return
	}
	coverPath := ""
	if strings.EqualFold(session.Config.ConvertFormat, "flac") && track.SaveDir != "" {
		coverPath = findCoverFile(track.SaveDir)
	}
	apputils.ConvertIfNeeded(track, lrc, &session.Config, coverPath, session.ActiveProgress)
}

func writeMP4Tags(track *task.Track, lrc string, cfg *structs.ConfigSet) error {
	if cfg == nil {
		return fmt.Errorf("config is nil")
	}
	genre, err := firstGenreName("main.writeMP4Tags", track.Resp.Attributes.GenreNames)
	if err != nil {
		return err
	}
	t := &mp4tag.MP4Tags{
		Title:      track.Resp.Attributes.Name,
		TitleSort:  track.Resp.Attributes.Name,
		Artist:     track.Resp.Attributes.ArtistName,
		ArtistSort: track.Resp.Attributes.ArtistName,
		Custom: map[string]string{
			"PERFORMER":   track.Resp.Attributes.ArtistName,
			"RELEASETIME": track.Resp.Attributes.ReleaseDate,
			"ISRC":        track.Resp.Attributes.Isrc,
			"LABEL":       "",
			"UPC":         "",
		},
		Composer:     track.Resp.Attributes.ComposerName,
		ComposerSort: track.Resp.Attributes.ComposerName,
		CustomGenre:  genre,
		Lyrics:       lrc,
		TrackNumber:  int16(track.Resp.Attributes.TrackNumber),
		DiscNumber:   int16(track.Resp.Attributes.DiscNumber),
		Album:        track.Resp.Attributes.AlbumName,
		AlbumSort:    track.Resp.Attributes.AlbumName,
	}

	if track.PreType == "albums" {
		albumID, err := strconv.ParseUint(track.PreID, 10, 32)
		if err != nil {
			return err
		}
		t.ItunesAlbumID = int32(albumID)
	}

	if artistRef, artistErr := safe.FirstRef("main.writeMP4Tags", "song.relationships.artists.data", track.Resp.Relationships.Artists.Data); artistErr == nil {
		artistID, err := strconv.ParseUint(artistRef.ID, 10, 32)
		if err != nil {
			return err
		}
		t.ItunesArtistID = int32(artistID)
	}

	if (track.PreType == "playlists" || track.PreType == "stations") && !cfg.UseSongInfoForPlaylist {
		t.DiscNumber = 1
		t.DiscTotal = 1
		t.TrackNumber = int16(track.TaskNum)
		t.TrackTotal = int16(track.TaskTotal)
		t.Album = track.PlaylistData.Name
		t.AlbumSort = track.PlaylistData.Name
		t.AlbumArtist = track.PlaylistData.ArtistName
		t.AlbumArtistSort = track.PlaylistData.ArtistName
	} else if (track.PreType == "playlists" || track.PreType == "stations") && cfg.UseSongInfoForPlaylist {
		t.DiscTotal = int16(track.DiscTotal)
		t.TrackTotal = int16(track.AlbumData.TrackCount)
		t.AlbumArtist = track.AlbumData.ArtistName
		t.AlbumArtistSort = track.AlbumData.ArtistName
		t.Custom["UPC"] = track.AlbumData.Upc
		t.Custom["LABEL"] = track.AlbumData.RecordLabel
		t.Date = track.AlbumData.ReleaseDate
		t.Copyright = track.AlbumData.Copyright
		t.Publisher = track.AlbumData.RecordLabel
	} else {
		t.DiscTotal = int16(track.DiscTotal)
		t.TrackTotal = int16(track.AlbumData.TrackCount)
		t.AlbumArtist = track.AlbumData.ArtistName
		t.AlbumArtistSort = track.AlbumData.ArtistName
		t.Custom["UPC"] = track.AlbumData.Upc
		t.Date = track.AlbumData.ReleaseDate
		t.Copyright = track.AlbumData.Copyright
		t.Publisher = track.AlbumData.RecordLabel
	}

	if track.Resp.Attributes.ContentRating == "explicit" {
		t.ItunesAdvisory = mp4tag.ItunesAdvisoryExplicit
	} else if track.Resp.Attributes.ContentRating == "clean" {
		t.ItunesAdvisory = mp4tag.ItunesAdvisoryClean
	} else {
		t.ItunesAdvisory = mp4tag.ItunesAdvisoryNone
	}

	mp4, err := mp4tag.Open(track.SavePath)
	if err != nil {
		return err
	}
	defer mp4.Close()
	err = mp4.Write(t, []string{})
	if err != nil {
		return err
	}
	return nil
}

func main() {
	err := loadConfig()
	if err != nil {
		fmt.Printf("load Config failed: %v", err)
		return
	}
	token, err := ampapi.GetToken()
	if err != nil {
		if Config.AuthorizationToken != "" && Config.AuthorizationToken != "your-authorization-token" {
			token = strings.Replace(Config.AuthorizationToken, "Bearer ", "", -1)
		} else {
			fmt.Println("Failed to get token.")
			return
		}
	}
	var search_type string
	var bot_mode bool
	var astrbot_api bool
	var astrbot_api_listen string
	var cliDlAtmos bool
	var cliDlAac bool
	var cliDlSelect bool
	var cliDlSong bool
	pflag.StringVar(&search_type, "search", "", "Search for 'album', 'song', or 'artist'. Provide query after flags.")
	pflag.BoolVar(&bot_mode, "bot", false, "Run Telegram bot mode")
	pflag.BoolVar(&astrbot_api, "astrbot-api", false, "Run AstrBot HTTP API service mode")
	pflag.StringVar(&astrbot_api_listen, "astrbot-api-listen", defaultAstrBotAPIListen, "Listen address for --astrbot-api")
	pflag.BoolVar(&cliDlAtmos, "atmos", false, "Enable atmos download mode")
	pflag.BoolVar(&cliDlAac, "aac", false, "Enable adm-aac download mode")
	pflag.BoolVar(&cliDlSelect, "select", false, "Enable selective download")
	pflag.BoolVar(&cliDlSong, "song", false, "Enable single song download mode")
	pflag.BoolVar(&artist_select, "all-album", false, "Download all artist albums")
	pflag.BoolVar(&debug_mode, "debug", false, "Enable debug mode to show audio quality information")
	alac_max = pflag.Int("alac-max", Config.AlacMax, "Specify the max quality for download alac")
	atmos_max = pflag.Int("atmos-max", Config.AtmosMax, "Specify the max quality for download atmos")
	aac_type = pflag.String("aac-type", Config.AacType, "Select AAC type, aac aac-binaural aac-downmix")
	mv_audio_type = pflag.String("mv-audio-type", Config.MVAudioType, "Select MV audio type, atmos ac3 aac")
	mv_max = pflag.Int("mv-max", Config.MVMax, "Specify the max quality for download MV")

	pflag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: %s [options] [url1 url2 ...]\n", "[main | main.exe | go run main.go]")
		fmt.Fprintf(os.Stderr, "Search Usage: %s --search [album|song|artist] [query]\n", "[main | main.exe | go run main.go]")
		fmt.Println("\nOptions:")
		pflag.PrintDefaults()
	}

	pflag.Parse()
	Config.AlacMax = *alac_max
	Config.AtmosMax = *atmos_max
	Config.AacType = *aac_type
	Config.MVAudioType = *mv_audio_type
	Config.MVMax = *mv_max

	cliTemplate := newDownloadSession(Config)
	cliTemplate.DlAtmos = cliDlAtmos
	cliTemplate.DlAac = cliDlAac
	cliTemplate.DlSelect = cliDlSelect
	cliTemplate.DlSong = cliDlSong

	if bot_mode {
		runTelegramBot(token)
		return
	}

	if astrbot_api {
		if err := runAstrBotAPIServer(token, astrbot_api_listen); err != nil {
			fmt.Printf("AstrBot API server failed: %v\n", err)
		}
		return
	}

	args := pflag.Args()

	if search_type != "" {
		if len(args) == 0 {
			fmt.Println("Error: --search flag requires a query.")
			pflag.Usage()
			return
		}
		selectedUrl, err := handleSearch(cliTemplate, search_type, args, token)
		if err != nil {
			fmt.Printf("\nSearch process failed: %v\n", err)
			return
		}
		if selectedUrl == "" {
			fmt.Println("\nExiting.")
			return
		}
		os.Args = []string{selectedUrl}
	} else {
		if len(args) == 0 {
			fmt.Println("No URLs provided. Please provide at least one URL.")
			pflag.Usage()
			return
		}
		os.Args = args
	}

	if strings.Contains(os.Args[0], "/artist/") {
		urlArtistName, urlArtistID, err := getUrlArtistName(os.Args[0], token)
		if err != nil {
			fmt.Println("Failed to get artistname.")
			return
		}
		cliTemplate.Config.ArtistFolderFormat = strings.NewReplacer(
			"{UrlArtistName}", LimitStringWithConfig(&cliTemplate.Config, urlArtistName),
			"{ArtistId}", urlArtistID,
		).Replace(cliTemplate.Config.ArtistFolderFormat)
		albumArgs, err := checkArtist(os.Args[0], token, "albums")
		if err != nil {
			fmt.Println("Failed to get artist albums.")
			return
		}
		mvArgs, err := checkArtist(os.Args[0], token, "music-videos")
		if err != nil {
			fmt.Println("Failed to get artist music-videos.")
		}
		os.Args = append(albumArgs, mvArgs...)
	}
	albumTotal := len(os.Args)
	newCLISession := func() *DownloadSession {
		s := newDownloadSession(cliTemplate.Config)
		s.DlAtmos = cliTemplate.DlAtmos
		s.DlAac = cliTemplate.DlAac
		s.DlSelect = cliTemplate.DlSelect
		s.DlSong = cliTemplate.DlSong
		s.StaticCoverDownload = cliTemplate.StaticCoverDownload
		return s
	}
	session := newCLISession()
	for {
		for albumNum, urlRaw := range os.Args {
			fmt.Printf("Queue %d of %d: ", albumNum+1, albumTotal)
			var storefront, albumId string

			if strings.Contains(urlRaw, "/music-video/") {
				fmt.Println("Music Video")
				if debug_mode {
					continue
				}
				session.Counter.Total++
				if len(session.Config.MediaUserToken) <= 50 {
					fmt.Println(": meida-user-token is not set, skip MV dl")
					session.Counter.Success++
					continue
				}
				if _, err := exec.LookPath("mp4decrypt"); err != nil {
					fmt.Println(": mp4decrypt is not found, skip MV dl")
					session.Counter.Success++
					continue
				}
				mvSaveDir := strings.NewReplacer(
					"{ArtistName}", "",
					"{UrlArtistName}", "",
					"{ArtistId}", "",
				).Replace(session.Config.ArtistFolderFormat)
				if mvSaveDir != "" {
					mvSaveDir = filepath.Join(session.Config.AlacSaveFolder, forbiddenNames.ReplaceAllString(mvSaveDir, "_"))
				} else {
					mvSaveDir = session.Config.AlacSaveFolder
				}
				storefront, albumId = checkUrlMv(urlRaw)
				err := mvDownloader(session, albumId, mvSaveDir, token, storefront, session.Config.MediaUserToken, nil)
				if err != nil {
					fmt.Println("\u26A0 Failed to dl MV:", err)
					session.Counter.Error++
					continue
				}
				session.Counter.Success++
				continue
			}
			if strings.Contains(urlRaw, "/song/") {
				fmt.Printf("Song->")
				storefront, songId := checkUrlSong(urlRaw)
				if storefront == "" || songId == "" {
					fmt.Println("Invalid song URL format.")
					continue
				}
				err := ripSong(session, songId, token, storefront, session.Config.MediaUserToken)
				if err != nil {
					fmt.Println("Failed to rip song:", err)
				}
				continue
			}
			parse, err := url.Parse(urlRaw)
			if err != nil {
				log.Fatalf("Invalid URL: %v", err)
			}
			var urlArg_i = parse.Query().Get("i")

			if strings.Contains(urlRaw, "/album/") {
				fmt.Println("Album")
				storefront, albumId = checkUrl(urlRaw)
				err := ripAlbum(session, albumId, token, storefront, session.Config.MediaUserToken, urlArg_i)
				if err != nil {
					fmt.Println("Failed to rip album:", err)
				}
			} else if strings.Contains(urlRaw, "/playlist/") {
				fmt.Println("Playlist")
				storefront, albumId = checkUrlPlaylist(urlRaw)
				err := ripPlaylist(session, albumId, token, storefront, session.Config.MediaUserToken)
				if err != nil {
					fmt.Println("Failed to rip playlist:", err)
				}
			} else if strings.Contains(urlRaw, "/station/") {
				fmt.Printf("Station")
				storefront, albumId = checkUrlStation(urlRaw)
				if len(session.Config.MediaUserToken) <= 50 {
					fmt.Println(": meida-user-token is not set, skip station dl")
					continue
				}
				err := ripStation(session, albumId, token, storefront, session.Config.MediaUserToken)
				if err != nil {
					fmt.Println("Failed to rip station:", err)
				}
			} else {
				fmt.Println("Invalid type")
			}
		}
		fmt.Printf("=======  [\u2714 ] Completed: %d/%d  |  [\u26A0 ] Warnings: %d  |  [\u2716 ] Errors: %d  =======\n", session.Counter.Success, session.Counter.Total, session.Counter.Unavailable+session.Counter.NotSong, session.Counter.Error)
		if session.Counter.Error == 0 {
			break
		}
		fmt.Println("Error detected, press Enter to try again...")
		fmt.Scanln()
		fmt.Println("Start trying again...")
		session = newCLISession()
	}
}

func extractMvAudio(c string) (string, error) {
	return extractMvAudioWithConfig(c, Config)
}

func extractMvAudioWithConfig(c string, cfg structs.ConfigSet) (string, error) {
	MediaUrl, err := url.Parse(c)
	if err != nil {
		return "", err
	}

	resp, err := networkHTTPClient.Get(c)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", errors.New(resp.Status)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	audioString := string(body)
	from, listType, err := m3u8.DecodeFrom(strings.NewReader(audioString), true)
	if err != nil || listType != m3u8.MASTER {
		return "", errors.New("m3u8 not of media type")
	}

	audio := from.(*m3u8.MasterPlaylist)

	var audioPriority = []string{"audio-atmos", "audio-ac3", "audio-stereo-256"}
	if cfg.MVAudioType == "ac3" {
		audioPriority = []string{"audio-ac3", "audio-stereo-256"}
	} else if cfg.MVAudioType == "aac" {
		audioPriority = []string{"audio-stereo-256"}
	}

	re := regexp.MustCompile(`_gr(\d+)_`)

	type AudioStream struct {
		URL     string
		Rank    int
		GroupID string
	}
	var audioStreams []AudioStream

	for _, variant := range audio.Variants {
		for _, audiov := range variant.Alternatives {
			if audiov.URI != "" {
				for _, priority := range audioPriority {
					if audiov.GroupId == priority {
						matches := re.FindStringSubmatch(audiov.URI)
						if len(matches) == 2 {
							var rank int
							fmt.Sscanf(matches[1], "%d", &rank)
							streamUrl, _ := MediaUrl.Parse(audiov.URI)
							audioStreams = append(audioStreams, AudioStream{
								URL:     streamUrl.String(),
								Rank:    rank,
								GroupID: audiov.GroupId,
							})
						}
					}
				}
			}
		}
	}

	if len(audioStreams) == 0 {
		return "", errors.New("no suitable audio stream found")
	}

	sort.Slice(audioStreams, func(i, j int) bool {
		return audioStreams[i].Rank > audioStreams[j].Rank
	})
	fmt.Println("Audio: " + audioStreams[0].GroupID)
	return audioStreams[0].URL, nil
}

func checkM3u8(session *DownloadSession, b string, f string) (string, error) {
	cfg := Config
	if session != nil {
		cfg = session.Config
	}
	var EnhancedHls string
	if cfg.GetM3u8FromDevice {
		adamID := b
		conn, err := net.Dial("tcp", cfg.GetM3u8Port)
		if err != nil {
			fmt.Println("Error connecting to device:", err)
			return "none", err
		}
		defer conn.Close()
		if f == "song" {
			fmt.Println("Connected to device")
		}

		adamIDBuffer := []byte(adamID)
		lengthBuffer := []byte{byte(len(adamIDBuffer))}

		_, err = conn.Write(lengthBuffer)
		if err != nil {
			fmt.Println("Error writing length to device:", err)
			return "none", err
		}

		_, err = conn.Write(adamIDBuffer)
		if err != nil {
			fmt.Println("Error writing adamID to device:", err)
			return "none", err
		}

		response, err := bufio.NewReader(conn).ReadBytes('\n')
		if err != nil {
			fmt.Println("Error reading response from device:", err)
			return "none", err
		}

		response = bytes.TrimSpace(response)
		if len(response) > 0 {
			if f == "song" {
				fmt.Println("Received URL:", string(response))
			}
			EnhancedHls = string(response)
		} else {
			fmt.Println("Received an empty response")
		}
	}
	return EnhancedHls, nil
}

func formatAvailability(available bool, quality string) string {
	if !available {
		return "Not Available"
	}
	return quality
}

func extractMedia(session *DownloadSession, b string, more_mode bool) (string, string, error) {
	if session == nil {
		return "", "", fmt.Errorf("download session is nil")
	}
	cfg := &session.Config
	masterUrl, err := url.Parse(b)
	if err != nil {
		return "", "", err
	}
	resp, err := networkHTTPClient.Get(b)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", errors.New(resp.Status)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", "", err
	}
	masterString := string(body)
	from, listType, err := m3u8.DecodeFrom(strings.NewReader(masterString), true)
	if err != nil || listType != m3u8.MASTER {
		return "", "", errors.New("m3u8 not of master type")
	}
	master := from.(*m3u8.MasterPlaylist)
	var streamUrl *url.URL
	sort.Slice(master.Variants, func(i, j int) bool {
		return master.Variants[i].AverageBandwidth > master.Variants[j].AverageBandwidth
	})
	if debug_mode && more_mode {
		fmt.Println("\nDebug: All Available Variants:")
		var data [][]string
		for _, variant := range master.Variants {
			data = append(data, []string{variant.Codecs, variant.Audio, fmt.Sprint(variant.Bandwidth)})
		}
		table := tablewriter.NewWriter(os.Stdout)
		table.SetHeader([]string{"Codec", "Audio", "Bandwidth"})
		table.SetAutoMergeCells(true)
		table.SetRowLine(true)
		table.AppendBulk(data)
		table.Render()

		var hasAAC, hasLossless, hasHiRes, hasAtmos, hasDolbyAudio bool
		var aacQuality, losslessQuality, hiResQuality, atmosQuality, dolbyAudioQuality string

		for _, variant := range master.Variants {
			if variant.Codecs == "mp4a.40.2" { // AAC
				hasAAC = true
				split := strings.Split(variant.Audio, "-")
				if len(split) >= 3 {
					bitrate, _ := strconv.Atoi(split[2])
					currentBitrate := 0
					if aacQuality != "" {
						current := strings.Split(aacQuality, " | ")[2]
						current = strings.Split(current, " ")[0]
						currentBitrate, _ = strconv.Atoi(current)
					}
					if bitrate > currentBitrate {
						aacQuality = fmt.Sprintf("AAC | 2 Channel | %d Kbps", bitrate)
					}
				}
			} else if variant.Codecs == "ec-3" && strings.Contains(variant.Audio, "atmos") { // Dolby Atmos
				hasAtmos = true
				split := strings.Split(variant.Audio, "-")
				if len(split) > 0 {
					bitrateStr := split[len(split)-1]
					if len(bitrateStr) == 4 && bitrateStr[0] == '2' {
						bitrateStr = bitrateStr[1:]
					}
					bitrate, _ := strconv.Atoi(bitrateStr)
					currentBitrate := 0
					if atmosQuality != "" {
						current := strings.Split(strings.Split(atmosQuality, " | ")[2], " ")[0]
						currentBitrate, _ = strconv.Atoi(current)
					}
					if bitrate > currentBitrate {
						atmosQuality = fmt.Sprintf("E-AC-3 | 16 Channel | %d Kbps", bitrate)
					}
				}
			} else if variant.Codecs == "alac" { // ALAC (Lossless or Hi-Res)
				split := strings.Split(variant.Audio, "-")
				if len(split) >= 3 {
					bitDepth := split[len(split)-1]
					sampleRate := split[len(split)-2]
					sampleRateInt, _ := strconv.Atoi(sampleRate)
					if sampleRateInt > 48000 { // Hi-Res
						hasHiRes = true
						hiResQuality = fmt.Sprintf("ALAC | 2 Channel | %s-bit/%d kHz", bitDepth, sampleRateInt/1000)
					} else { // Standard Lossless
						hasLossless = true
						losslessQuality = fmt.Sprintf("ALAC | 2 Channel | %s-bit/%d kHz", bitDepth, sampleRateInt/1000)
					}
				}
			} else if variant.Codecs == "ac-3" { // Dolby Audio
				hasDolbyAudio = true
				split := strings.Split(variant.Audio, "-")
				if len(split) > 0 {
					bitrate, _ := strconv.Atoi(split[len(split)-1])
					dolbyAudioQuality = fmt.Sprintf("AC-3 |  16 Channel | %d Kbps", bitrate)
				}
			}
		}

		fmt.Println("Available Audio Formats:")
		fmt.Println("------------------------")
		fmt.Printf("AAC             : %s\n", formatAvailability(hasAAC, aacQuality))
		fmt.Printf("Lossless        : %s\n", formatAvailability(hasLossless, losslessQuality))
		fmt.Printf("Hi-Res Lossless : %s\n", formatAvailability(hasHiRes, hiResQuality))
		fmt.Printf("Dolby Atmos     : %s\n", formatAvailability(hasAtmos, atmosQuality))
		fmt.Printf("Dolby Audio     : %s\n", formatAvailability(hasDolbyAudio, dolbyAudioQuality))
		fmt.Println("------------------------")

		return "", "", nil
	}
	var Quality string
	for _, variant := range master.Variants {
		if session.DlAtmos {
			if variant.Codecs == "ec-3" && strings.Contains(variant.Audio, "atmos") {
				if debug_mode && !more_mode {
					fmt.Printf("Debug: Found Dolby Atmos variant - %s (Bitrate: %d Kbps)\n",
						variant.Audio, variant.Bandwidth/1000)
				}
				split := strings.Split(variant.Audio, "-")
				length := len(split)
				length_int, err := strconv.Atoi(split[length-1])
				if err != nil {
					return "", "", err
				}
				if length_int <= cfg.AtmosMax {
					if !debug_mode && !more_mode {
						fmt.Printf("%s\n", variant.Audio)
					}
					streamUrlTemp, err := masterUrl.Parse(variant.URI)
					if err != nil {
						return "", "", err
					}
					streamUrl = streamUrlTemp
					Quality = fmt.Sprintf("%s Kbps", split[len(split)-1])
					break
				}
			} else if variant.Codecs == "ac-3" { // Add Dolby Audio support
				if debug_mode && !more_mode {
					fmt.Printf("Debug: Found Dolby Audio variant - %s (Bitrate: %d Kbps)\n",
						variant.Audio, variant.Bandwidth/1000)
				}
				streamUrlTemp, err := masterUrl.Parse(variant.URI)
				if err != nil {
					return "", "", err
				}
				streamUrl = streamUrlTemp
				split := strings.Split(variant.Audio, "-")
				Quality = fmt.Sprintf("%s Kbps", split[len(split)-1])
				break
			}
		} else if session.DlAac {
			if variant.Codecs == "mp4a.40.2" {
				if debug_mode && !more_mode {
					fmt.Printf("Debug: Found AAC variant - %s (Bitrate: %d)\n", variant.Audio, variant.Bandwidth)
				}
				aacregex := regexp.MustCompile(`audio-stereo-\d+`)
				replaced := aacregex.ReplaceAllString(variant.Audio, "aac")
				if replaced == cfg.AacType {
					if !debug_mode && !more_mode {
						fmt.Printf("%s\n", variant.Audio)
					}
					streamUrlTemp, err := masterUrl.Parse(variant.URI)
					if err != nil {
						return "", "", err
					}
					streamUrl = streamUrlTemp
					split := strings.Split(variant.Audio, "-")
					Quality = fmt.Sprintf("%s Kbps", split[2])
					break
				}
			}
		} else {
			if variant.Codecs == "alac" {
				split := strings.Split(variant.Audio, "-")
				length := len(split)
				length_int, err := strconv.Atoi(split[length-2])
				if err != nil {
					return "", "", err
				}
				if length_int <= cfg.AlacMax {
					if !debug_mode && !more_mode {
						fmt.Printf("%s-bit / %s Hz\n", split[length-1], split[length-2])
					}
					streamUrlTemp, err := masterUrl.Parse(variant.URI)
					if err != nil {
						return "", "", err
					}
					streamUrl = streamUrlTemp
					KHZ := float64(length_int) / 1000.0
					Quality = fmt.Sprintf("%sB-%.1fkHz", split[length-1], KHZ)
					break
				}
			}
		}
	}
	if streamUrl == nil {
		return "", "", errors.New("no codec found")
	}
	return streamUrl.String(), Quality, nil
}

func extractVideo(c string) (string, error) {
	return extractVideoWithConfig(c, Config)
}

func extractVideoWithConfig(c string, cfg structs.ConfigSet) (string, error) {
	MediaUrl, err := url.Parse(c)
	if err != nil {
		return "", err
	}

	resp, err := networkHTTPClient.Get(c)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", errors.New(resp.Status)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	videoString := string(body)

	from, listType, err := m3u8.DecodeFrom(strings.NewReader(videoString), true)
	if err != nil || listType != m3u8.MASTER {
		return "", errors.New("m3u8 not of media type")
	}

	video := from.(*m3u8.MasterPlaylist)

	re := regexp.MustCompile(`_(\d+)x(\d+)`)

	var streamUrl *url.URL
	sort.Slice(video.Variants, func(i, j int) bool {
		return video.Variants[i].AverageBandwidth > video.Variants[j].AverageBandwidth
	})

	maxHeight := cfg.MVMax

	for _, variant := range video.Variants {
		matches := re.FindStringSubmatch(variant.URI)
		if len(matches) == 3 {
			height := matches[2]
			var h int
			_, err := fmt.Sscanf(height, "%d", &h)
			if err != nil {
				continue
			}
			if h <= maxHeight {
				streamUrl, err = MediaUrl.Parse(variant.URI)
				if err != nil {
					return "", err
				}
				fmt.Println("Video: " + variant.Resolution + "-" + variant.VideoRange)
				break
			}
		}
	}

	if streamUrl == nil {
		return "", errors.New("no suitable video stream found")
	}

	return streamUrl.String(), nil
}

const (
	defaultSearchLimit                 = 8
	defaultQueueSize                   = 20
	defaultTaskWorkerLimit             = 1
	maxTaskWorkerLimit                 = 4
	downloadWorkerPoolSize             = 4
	pendingTTL                         = 10 * time.Minute
	defaultTelegramFormat              = "alac"
	defaultTelegramAACType             = "aac-lc"
	defaultTelegramMVAudioType         = "atmos"
	defaultTelegramLyricsFormat        = "lrc"
	defaultTelegramDownloadMaxGB       = 3
	defaultTelegramCleanupInterval     = 5 * time.Minute
	defaultTelegramCleanupScanInterval = 30 * time.Minute
	defaultTelegramCleanupProtectAge   = 2 * time.Minute
	defaultTelegramHTTPTimeout         = 180 * time.Second
	defaultTelegramPollTimeout         = 75 * time.Second
	defaultTelegramStateFile           = "telegram-state.json"
	defaultTelegramMetricsInterval     = 60 * time.Second
	defaultTelegramResourceCheck       = 30 * time.Second
	defaultTelegramMinFreeDiskMB       = 512
	defaultTelegramMinFreeTmpMB        = 256
	defaultTelegramSendGlobalInterval  = 150 * time.Millisecond
	defaultTelegramSendChatInterval    = 800 * time.Millisecond
	minTelegramPollTimeout             = 35 * time.Second
	telegramDialTimeout                = 20 * time.Second
	telegramTLSHandshakeTimeout        = 30 * time.Second
	uploadNoProgressTimeout            = 120 * time.Second
	uploadWatchdogInterval             = 5 * time.Second
	uploadProgressBufferSize           = 32 * 1024
)

const (
	telegramFormatAlac   = "alac"
	telegramFormatFlac   = "flac"
	telegramFormatAac    = "aac"
	telegramFormatAtmos  = "atmos"
	transferModeOneByOne = "one"
	transferModeZip      = "zip"
)

const (
	mediaTypeSong        = "song"
	mediaTypeAlbum       = "album"
	mediaTypePlaylist    = "playlist"
	mediaTypeStation     = "station"
	mediaTypeMusicVideo  = "music-video"
	mediaTypeArtist      = "artist"
	mediaTypeAlbumLyrics = "album-lyrics"
	mediaTypeArtistAsset = "artist-asset"
)

const (
	telegramTaskDownload      = "download"
	telegramTaskCover         = "cover"
	telegramTaskAnimatedCover = "animated-cover"
	telegramTaskSongLyrics    = "song-lyrics"
	telegramTaskAlbumLyrics   = "album-lyrics"
	telegramTaskArtistAssets  = "artist-assets"
)

type ChatDownloadSettings struct {
	Format         string
	AACType        string
	MVAudioType    string
	LyricsFormat   string
	SongZip        bool
	AutoLyrics     bool
	AutoCover      bool
	AutoAnimated   bool
	SettingsInited bool
}

func normalizeTransferModeForMedia(transferMode string, mediaType string, single bool) string {
	if transferMode != transferModeZip {
		transferMode = transferModeOneByOne
	}
	if mediaType == mediaTypeMusicVideo {
		return transferModeOneByOne
	}
	if single && mediaType != mediaTypeSong {
		return transferModeOneByOne
	}
	return transferMode
}

func normalizeTelegramTaskType(taskType string) string {
	switch strings.TrimSpace(taskType) {
	case "", telegramTaskDownload:
		return telegramTaskDownload
	case telegramTaskCover:
		return telegramTaskCover
	case telegramTaskAnimatedCover:
		return telegramTaskAnimatedCover
	case telegramTaskSongLyrics:
		return telegramTaskSongLyrics
	case telegramTaskAlbumLyrics:
		return telegramTaskAlbumLyrics
	case telegramTaskArtistAssets:
		return telegramTaskArtistAssets
	default:
		return strings.TrimSpace(taskType)
	}
}

func telegramMediaProducesSongAudio(mediaType string) bool {
	switch mediaType {
	case mediaTypeSong, mediaTypeAlbum, mediaTypePlaylist, mediaTypeStation:
		return true
	default:
		return false
	}
}

func applyTelegramAudioEmbeddingPolicy(session *DownloadSession, settings ChatDownloadSettings, mediaType string) {
	if session == nil || !telegramMediaProducesSongAudio(mediaType) {
		return
	}
	// Always embed lyrics + cover for downloaded song audio, regardless of extra-file toggles.
	session.Config.LrcFormat = settings.LyricsFormat
	session.Config.SaveLrcFile = settings.AutoLyrics
	session.Config.EmbedLrc = true
	session.Config.EmbedCover = true
	session.Config.SaveAnimatedArtwork = settings.AutoAnimated
	// Keep static cover download enabled so cover embedding always has a source file.
	session.StaticCoverDownload = true
}

type TelegramBot struct {
	token        string
	apiBase      string
	proxyInfo    string
	appleToken   string
	client       *http.Client
	pollClient   *http.Client
	allowedChats map[int64]bool
	searchLimit  int
	maxFileBytes int64

	settingsMu   sync.Mutex
	chatSettings map[int64]ChatDownloadSettings

	pendingMu sync.Mutex
	pending   map[int64]map[int]*PendingSelection

	transferMu       sync.Mutex
	pendingTransfers map[int64]map[int]*PendingTransfer

	artistModeMu       sync.Mutex
	pendingArtistModes map[int64]map[int]*PendingArtistMode

	queueMu       sync.Mutex
	downloadQueue chan *downloadRequest
	queueCond     *sync.Cond
	workerLimit   int
	activeWorkers int

	downloadCoreMu sync.Mutex

	cacheMu    sync.Mutex
	cacheFile  string
	cache      map[string]CachedAudio
	docCache   map[string]CachedDocument
	videoCache map[string]CachedVideo

	inflightMu        sync.Mutex
	inflightDownloads map[string]struct{}
	sendLimiter       *telegramSendLimiter

	requestStateMu sync.Mutex
	activeRequests map[string]telegramPersistedRequest
	requestSeq     atomic.Uint64

	stateFile string
	stateMu   sync.Mutex
	stateSave chan struct{}
	stateStop chan struct{}
	stateWG   sync.WaitGroup

	metricsStop chan struct{}
	metricsWG   sync.WaitGroup

	resourceGuard *telegramResourceGuard

	cleanupTracker *telegramCleanupTracker
}

type PendingSelection struct {
	Kind             string
	Query            string
	Title            string
	Storefront       string
	Offset           int
	HasNext          bool
	Items            []apputils.SearchResultItem
	CreatedAt        time.Time
	ReplyToMessageID int
	ResultsMessageID int
}

type PendingTransfer struct {
	MediaType        string
	MediaID          string
	MediaName        string
	Storefront       string
	ForceRefresh     bool
	ReplyToMessageID int
	MessageID        int
	CreatedAt        time.Time
}

type PendingArtistMode struct {
	ArtistID         string
	ArtistName       string
	Storefront       string
	ReplyToMessageID int
	MessageID        int
	CreatedAt        time.Time
}

type downloadRequest struct {
	chatID       int64
	replyToID    int
	single       bool
	forceRefresh bool
	taskType     string
	settings     ChatDownloadSettings
	transferMode string
	mediaType    string
	mediaID      string
	storefront   string
	inflightKey  string
	requestID    string
	fn           func(session *DownloadSession) error
	run          func(*TelegramBot) error
}

type Update struct {
	UpdateID      int            `json:"update_id"`
	Message       *Message       `json:"message,omitempty"`
	CallbackQuery *CallbackQuery `json:"callback_query,omitempty"`
	InlineQuery   *InlineQuery   `json:"inline_query,omitempty"`
}

type Message struct {
	MessageID int    `json:"message_id"`
	From      *User  `json:"from,omitempty"`
	Chat      Chat   `json:"chat"`
	Text      string `json:"text,omitempty"`
}

type CallbackQuery struct {
	ID      string   `json:"id"`
	From    *User    `json:"from,omitempty"`
	Message *Message `json:"message,omitempty"`
	Data    string   `json:"data,omitempty"`
}

type InlineQuery struct {
	ID    string `json:"id"`
	From  *User  `json:"from,omitempty"`
	Query string `json:"query"`
}

type User struct {
	ID       int64  `json:"id"`
	Username string `json:"username,omitempty"`
}

type Chat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
}

type InlineKeyboardMarkup struct {
	InlineKeyboard [][]InlineKeyboardButton `json:"inline_keyboard"`
}

type InlineKeyboardButton struct {
	Text                         string  `json:"text"`
	CallbackData                 string  `json:"callback_data,omitempty"`
	SwitchInlineQuery            *string `json:"switch_inline_query,omitempty"`
	SwitchInlineQueryCurrentChat *string `json:"switch_inline_query_current_chat,omitempty"`
	Url                          string  `json:"url,omitempty"`
}

type ReplyKeyboardMarkup struct {
	Keyboard        [][]KeyboardButton `json:"keyboard"`
	ResizeKeyboard  bool               `json:"resize_keyboard,omitempty"`
	OneTimeKeyboard bool               `json:"one_time_keyboard,omitempty"`
}

type ReplyKeyboardRemove struct {
	RemoveKeyboard bool `json:"remove_keyboard"`
}

type KeyboardButton struct {
	Text string `json:"text"`
}

type getUpdatesResponse struct {
	OK          bool     `json:"ok"`
	Result      []Update `json:"result"`
	Description string   `json:"description,omitempty"`
}

type apiResponse struct {
	OK          bool   `json:"ok"`
	Description string `json:"description,omitempty"`
}

type sendMessageResponse struct {
	OK          bool    `json:"ok"`
	Result      Message `json:"result"`
	Description string  `json:"description,omitempty"`
}

type sendAudioResponse struct {
	OK          bool         `json:"ok"`
	Result      AudioMessage `json:"result"`
	Description string       `json:"description,omitempty"`
}

type sendDocumentResponse struct {
	OK          bool            `json:"ok"`
	Result      DocumentMessage `json:"result"`
	Description string          `json:"description,omitempty"`
}

type sendVideoResponse struct {
	OK          bool         `json:"ok"`
	Result      VideoMessage `json:"result"`
	Description string       `json:"description,omitempty"`
}

type AudioMessage struct {
	MessageID int   `json:"message_id"`
	Audio     Audio `json:"audio"`
}

type DocumentMessage struct {
	MessageID int      `json:"message_id"`
	Document  Document `json:"document"`
}

type VideoMessage struct {
	MessageID int   `json:"message_id"`
	Video     Video `json:"video"`
}

type Audio struct {
	FileID       string `json:"file_id"`
	FileUniqueID string `json:"file_unique_id,omitempty"`
	FileSize     int64  `json:"file_size,omitempty"`
}

type Document struct {
	FileID       string `json:"file_id"`
	FileUniqueID string `json:"file_unique_id,omitempty"`
	FileSize     int64  `json:"file_size,omitempty"`
	FileName     string `json:"file_name,omitempty"`
}

type Video struct {
	FileID       string `json:"file_id"`
	FileUniqueID string `json:"file_unique_id,omitempty"`
	FileSize     int64  `json:"file_size,omitempty"`
	Duration     int    `json:"duration,omitempty"`
	Width        int    `json:"width,omitempty"`
	Height       int    `json:"height,omitempty"`
}

type InlineQueryResultCachedAudio struct {
	Type        string `json:"type"`
	ID          string `json:"id"`
	AudioFileID string `json:"audio_file_id"`
	Caption     string `json:"caption,omitempty"`
}

type InlineQueryResultArticle struct {
	Type                string              `json:"type"`
	ID                  string              `json:"id"`
	Title               string              `json:"title"`
	Description         string              `json:"description,omitempty"`
	InputMessageContent InputMessageContent `json:"input_message_content"`
}

type InputMessageContent struct {
	MessageText string `json:"message_text"`
}

func runTelegramBot(appleToken string) {
	botToken := strings.TrimSpace(Config.TelegramBotToken)
	if botToken == "" {
		botToken = strings.TrimSpace(os.Getenv("TELEGRAM_BOT_TOKEN"))
	}
	if botToken == "" {
		fmt.Println("telegram-bot-token is not set. Add it to config.yaml or TELEGRAM_BOT_TOKEN.")
		return
	}
	sharedstorage.ApplyTelegramStorageOverrides(&Config)

	bot := newTelegramBot(botToken, appleToken)
	defer func() {
		if bot.cleanupTracker != nil {
			bot.cleanupTracker.stop()
		}
		if bot.resourceGuard != nil {
			bot.resourceGuard.stop()
		}
		bot.stopMetricsReporter()
		bot.stopStateSaver()
	}()
	fmt.Println("Telegram bot started. Waiting for updates...")
	fmt.Printf("Telegram API base: %s (proxy=%s, api timeout=%s, poll timeout=%s)\n", bot.apiBase, bot.proxyInfo, bot.client.Timeout, bot.pollClient.Timeout)
	bot.loopWithAutoRestart()
}

func panicErrorWithStack(scope string, rec any) error {
	label := strings.TrimSpace(scope)
	if label == "" {
		label = "runtime"
	}
	return fmt.Errorf("%s panic: %v\n%s", label, rec, string(debug.Stack()))
}

func logRecoveredPanic(scope string, rec any) error {
	err := panicErrorWithStack(scope, rec)
	fmt.Println(err.Error())
	return err
}

func runWithRecovery(scope string, onPanic func(error), fn func()) (panicked bool) {
	if fn == nil {
		return false
	}
	defer func() {
		if rec := recover(); rec != nil {
			panicked = true
			err := logRecoveredPanic(scope, rec)
			if onPanic != nil {
				onPanic(err)
			}
		}
	}()
	fn()
	return false
}

func (b *TelegramBot) loopWithAutoRestart() {
	const restartDelay = 2 * time.Second
	for {
		panicked := runWithRecovery("telegram loop", nil, func() {
			b.loop()
		})
		if !panicked {
			return
		}
		fmt.Printf("telegram loop recovered from panic, restarting in %s\n", restartDelay)
		closeHTTPIdleConnections(b.pollClient)
		closeHTTPIdleConnections(b.client)
		time.Sleep(restartDelay)
	}
}

func normalizeTelegramAPIBase(raw string) string {
	base := strings.TrimSpace(raw)
	if base == "" {
		return "https://api.telegram.org"
	}
	return strings.TrimRight(base, "/")
}

func sanitizeTelegramError(err error, token string) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	if token == "" {
		return msg
	}
	return strings.ReplaceAll(msg, token, "<redacted-token>")
}

func warnInsecureTelegramAPIBase(apiBase string) {
	parsed, err := url.Parse(apiBase)
	if err != nil {
		fmt.Printf("Warning: invalid telegram-api-url %q. Falling back to runtime errors if unreachable.\n", apiBase)
		return
	}
	if !strings.EqualFold(parsed.Scheme, "http") {
		return
	}
	host := strings.ToLower(strings.TrimSpace(parsed.Hostname()))
	if host == "localhost" {
		fmt.Printf("Warning: telegram-api-url uses plain HTTP (%s). Use only trusted local environments.\n", apiBase)
		return
	}
	ip := net.ParseIP(host)
	if ip != nil && ip.IsLoopback() {
		fmt.Printf("Warning: telegram-api-url uses plain HTTP (%s). Use only trusted local environments.\n", apiBase)
		return
	}
	fmt.Printf("Warning: telegram-api-url uses plain HTTP (%s). Telegram bot token may be exposed in transit.\n", apiBase)
}

func resolveTelegramProxy() (func(*http.Request) (*url.URL, error), string) {
	if Config.TelegramNoProxy {
		return nil, "disabled"
	}
	rawProxy := strings.TrimSpace(Config.TelegramProxyURL)
	if rawProxy == "" {
		return http.ProxyFromEnvironment, "env"
	}
	parsed, err := url.Parse(rawProxy)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		fmt.Printf("Warning: invalid telegram-proxy-url %q. Falling back to environment proxy.\n", rawProxy)
		return http.ProxyFromEnvironment, "env(fallback)"
	}
	return http.ProxyURL(parsed), parsed.Redacted()
}

func newTelegramHTTPClient(timeout time.Duration, proxyFunc func(*http.Request) (*url.URL, error)) *http.Client {
	transport := &http.Transport{
		Proxy: proxyFunc,
		DialContext: (&net.Dialer{
			Timeout:   telegramDialTimeout,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		// Force HTTP/1.1 for better compatibility with some proxy/tunnel paths.
		ForceAttemptHTTP2:     false,
		MaxIdleConns:          200,
		MaxIdleConnsPerHost:   20,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   telegramTLSHandshakeTimeout,
		ExpectContinueTimeout: 1 * time.Second,
	}
	return &http.Client{
		Timeout:   timeout,
		Transport: transport,
	}
}

func telegramDownloadMaxBytes() int64 {
	gb := Config.TelegramDownloadMaxGB
	if gb <= 0 {
		gb = defaultTelegramDownloadMaxGB
	}
	return int64(gb) * 1024 * 1024 * 1024
}

type telegramSendLimiter struct {
	globalInterval time.Duration
	chatInterval   time.Duration

	mu           sync.Mutex
	lastAll      time.Time
	lastChat     map[int64]time.Time
	blockedUntil time.Time
	nowFn        func() time.Time
}

func newTelegramSendLimiter(globalInterval time.Duration, chatInterval time.Duration) *telegramSendLimiter {
	if globalInterval < 0 {
		globalInterval = 0
	}
	if chatInterval < 0 {
		chatInterval = 0
	}
	if globalInterval == 0 && chatInterval == 0 {
		return nil
	}
	return &telegramSendLimiter{
		globalInterval: globalInterval,
		chatInterval:   chatInterval,
		lastChat:       make(map[int64]time.Time),
		nowFn:          time.Now,
	}
}

func (l *telegramSendLimiter) wait(ctx context.Context, chatID int64) error {
	if l == nil {
		return nil
	}
	for {
		wait := l.reserve(chatID)
		if wait <= 0 {
			return nil
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (l *telegramSendLimiter) reserve(chatID int64) time.Duration {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.nowFn()
	wait := l.nextWaitLocked(now, chatID)
	if wait <= 0 {
		if l.globalInterval > 0 {
			l.lastAll = now
		}
		if chatID != 0 && l.chatInterval > 0 {
			l.lastChat[chatID] = now
		}
	}
	return wait
}

func (l *telegramSendLimiter) nextWait(chatID int64) time.Duration {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.nextWaitLocked(l.nowFn(), chatID)
}

func (l *telegramSendLimiter) nextWaitLocked(now time.Time, chatID int64) time.Duration {
	wait := time.Duration(0)
	if !l.blockedUntil.IsZero() {
		if remain := l.blockedUntil.Sub(now); remain > wait {
			wait = remain
		}
	}
	if l.globalInterval > 0 && !l.lastAll.IsZero() {
		if remain := l.globalInterval - now.Sub(l.lastAll); remain > wait {
			wait = remain
		}
	}
	if chatID != 0 && l.chatInterval > 0 {
		if last, ok := l.lastChat[chatID]; ok && !last.IsZero() {
			if remain := l.chatInterval - now.Sub(last); remain > wait {
				wait = remain
			}
		}
	}
	return wait
}

func (l *telegramSendLimiter) blockFor(duration time.Duration) {
	if l == nil || duration <= 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	until := l.nowFn().Add(duration)
	if until.After(l.blockedUntil) {
		l.blockedUntil = until
	}
}

func parsePositiveIntEnv(name string) (int, bool) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return 0, false
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		return 0, false
	}
	return value, true
}

func telegramSendGlobalInterval() time.Duration {
	if ms, ok := parsePositiveIntEnv("AMDL_TELEGRAM_SEND_GLOBAL_INTERVAL_MS"); ok {
		return time.Duration(ms) * time.Millisecond
	}
	return defaultTelegramSendGlobalInterval
}

func telegramSendChatInterval() time.Duration {
	if ms, ok := parsePositiveIntEnv("AMDL_TELEGRAM_SEND_CHAT_INTERVAL_MS"); ok {
		return time.Duration(ms) * time.Millisecond
	}
	return defaultTelegramSendChatInterval
}

func newTelegramBot(token, appleToken string) *TelegramBot {
	allowed := make(map[int64]bool)
	for _, id := range Config.TelegramAllowedChatIDs {
		allowed[id] = true
	}
	searchLimit := Config.TelegramSearchLimit
	if searchLimit <= 0 {
		searchLimit = defaultSearchLimit
	}
	maxFileBytes := int64(Config.TelegramMaxFileMB) * 1024 * 1024
	if maxFileBytes <= 0 {
		maxFileBytes = 50 * 1024 * 1024
	}
	cacheFile := strings.TrimSpace(Config.TelegramCacheFile)
	if cacheFile == "" {
		cacheFile = "telegram-cache.json"
	}
	queueSize := defaultQueueSize
	if queueSize <= 0 {
		queueSize = 1
	}
	apiBase := normalizeTelegramAPIBase(Config.TelegramAPIURL)
	warnInsecureTelegramAPIBase(apiBase)
	apiTimeout := time.Duration(Config.TelegramHTTPTimeoutSec) * time.Second
	if apiTimeout <= 0 {
		apiTimeout = defaultTelegramHTTPTimeout
	}
	pollTimeout := time.Duration(Config.TelegramPollTimeoutSec) * time.Second
	if pollTimeout <= 0 {
		pollTimeout = defaultTelegramPollTimeout
	}
	if pollTimeout < minTelegramPollTimeout {
		pollTimeout = minTelegramPollTimeout
	}
	proxyFunc, proxyInfo := resolveTelegramProxy()
	bot := &TelegramBot{
		token:              token,
		apiBase:            apiBase,
		proxyInfo:          proxyInfo,
		appleToken:         appleToken,
		client:             newTelegramHTTPClient(apiTimeout, proxyFunc),
		pollClient:         newTelegramHTTPClient(pollTimeout, proxyFunc),
		allowedChats:       allowed,
		searchLimit:        searchLimit,
		maxFileBytes:       maxFileBytes,
		chatSettings:       make(map[int64]ChatDownloadSettings),
		pending:            make(map[int64]map[int]*PendingSelection),
		pendingTransfers:   make(map[int64]map[int]*PendingTransfer),
		pendingArtistModes: make(map[int64]map[int]*PendingArtistMode),
		downloadQueue:      make(chan *downloadRequest, queueSize),
		workerLimit:        defaultTaskWorkerLimit,
		cacheFile:          cacheFile,
		stateFile:          resolveTelegramStateFile(cacheFile),
		cache:              make(map[string]CachedAudio),
		docCache:           make(map[string]CachedDocument),
		videoCache:         make(map[string]CachedVideo),
		inflightDownloads:  make(map[string]struct{}),
		activeRequests:     make(map[string]telegramPersistedRequest),
		sendLimiter:        newTelegramSendLimiter(telegramSendGlobalInterval(), telegramSendChatInterval()),
	}
	bot.queueCond = sync.NewCond(&bot.queueMu)
	bot.loadCache()
	bot.startStateSaver()
	bot.startDownloadWorker()
	bot.restoreRuntimeState()
	bot.startMetricsReporter()
	bot.resourceGuard = newTelegramResourceGuard(telegramCleanupRootPaths())
	if bot.resourceGuard != nil {
		bot.resourceGuard.start()
	}
	bot.cleanupTracker = newTelegramCleanupTracker(
		telegramCleanupRoots(),
		cacheFile,
		telegramDownloadMaxBytes(),
		telegramCleanupInterval(),
		telegramCleanupScanInterval(),
		telegramCleanupProtectAge(),
	)
	bot.cleanupTracker.onDelete = func(path string, size int64) {
		appRuntimeMetrics.recordCleanupRemoval(size)
	}
	bot.cleanupTracker.start()
	bot.requestStateSave()
	return bot
}

func (b *TelegramBot) loop() {
	if err := b.dropPendingUpdatesOnStart(); err != nil {
		fmt.Println("startup drop-pending-updates failed:", sanitizeTelegramError(err, b.token))
	}
	offset := b.consumePendingUpdatesOnStart()
	lastConflictHint := time.Time{}
	for {
		updates, err := b.getUpdates(offset)
		if err != nil {
			msg := sanitizeTelegramError(err, b.token)
			fmt.Println("getUpdates error:", msg)
			lower := strings.ToLower(msg)
			if strings.Contains(lower, "409") || strings.Contains(lower, "conflict") {
				if lastConflictHint.IsZero() || time.Since(lastConflictHint) > 30*time.Second {
					fmt.Println("Hint: 409 Conflict means another getUpdates consumer is active (another bot process) or webhook is set. Keep only one bot instance and clear webhook if needed.")
					lastConflictHint = time.Now()
				}
				time.Sleep(5 * time.Second)
				continue
			}
			time.Sleep(2 * time.Second)
			continue
		}
		for _, upd := range updates {
			if upd.UpdateID >= offset {
				offset = upd.UpdateID + 1
			}
			b.dispatchUpdateSafely(upd)
		}
	}
}

func updateReplyContext(upd Update) (int64, int) {
	if upd.Message != nil {
		return upd.Message.Chat.ID, upd.Message.MessageID
	}
	if upd.CallbackQuery != nil && upd.CallbackQuery.Message != nil {
		return upd.CallbackQuery.Message.Chat.ID, upd.CallbackQuery.Message.MessageID
	}
	return 0, 0
}

func (b *TelegramBot) dispatchUpdate(upd Update) {
	if upd.Message != nil {
		b.handleMessage(upd.Message)
		return
	}
	if upd.CallbackQuery != nil {
		b.handleCallback(upd.CallbackQuery)
		return
	}
	if upd.InlineQuery != nil {
		b.handleInlineQuery(upd.InlineQuery)
	}
}

func (b *TelegramBot) dispatchUpdateSafely(upd Update) {
	scope := fmt.Sprintf("telegram update handler update_id=%d", upd.UpdateID)
	runWithRecovery(scope, func(_ error) {
		chatID, replyToID := updateReplyContext(upd)
		if chatID != 0 {
			_ = b.sendMessageWithReply(chatID, "任务异常已记录，已自动跳过并继续后续任务。", nil, replyToID)
		}
	}, func() {
		b.dispatchUpdate(upd)
	})
}

func (b *TelegramBot) dropPendingUpdatesOnStart() error {
	payload := map[string]any{
		"drop_pending_updates": true,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequest("POST", b.apiURL("deleteWebhook"), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	responseBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("deleteWebhook failed: %s", strings.TrimSpace(string(responseBody)))
	}
	apiResp := apiResponse{}
	if err := json.Unmarshal(responseBody, &apiResp); err == nil && !apiResp.OK {
		return fmt.Errorf("deleteWebhook error: %s", apiResp.Description)
	}
	return nil
}

func (b *TelegramBot) consumePendingUpdatesOnStart() int {
	offset := 0
	skipped := 0
	for {
		updates, err := b.getUpdatesWithOptions(offset, 0, 100)
		if err != nil {
			fmt.Println("startup pending-update check failed:", sanitizeTelegramError(err, b.token))
			return offset
		}
		if len(updates) == 0 {
			break
		}
		for _, upd := range updates {
			if upd.UpdateID >= offset {
				offset = upd.UpdateID + 1
				skipped++
			}
		}
		if len(updates) < 100 {
			break
		}
	}
	if skipped > 0 {
		fmt.Printf("Skipped %d pending updates on startup.\n", skipped)
	}
	return offset
}

func (b *TelegramBot) startDownloadWorker() {
	for i := 0; i < downloadWorkerPoolSize; i++ {
		go func() {
			for req := range b.downloadQueue {
				if req == nil {
					continue
				}
				func() {
					b.queueMu.Lock()
					for b.activeWorkers >= b.workerLimit {
						b.queueCond.Wait()
					}
					b.activeWorkers++
					b.queueMu.Unlock()

					defer func() {
						if rec := recover(); rec != nil {
							reqID := ""
							mediaType := ""
							mediaID := ""
							chatID := int64(0)
							replyToID := 0
							if req != nil {
								reqID = strings.TrimSpace(req.requestID)
								mediaType = req.mediaType
								mediaID = req.mediaID
								chatID = req.chatID
								replyToID = req.replyToID
							}
							scope := fmt.Sprintf("telegram download worker request=%s media=%s:%s", reqID, mediaType, mediaID)
							_ = logRecoveredPanic(scope, rec)
							if chatID != 0 {
								_ = b.sendMessageWithReply(chatID, "任务发生异常，已自动跳过并继续后续任务。", nil, replyToID)
							}
						}
					}()

					defer func() {
						if strings.TrimSpace(req.requestID) != "" {
							b.untrackRequest(req.requestID)
						}
						if strings.TrimSpace(req.inflightKey) != "" {
							b.releaseInflightDownload(req.inflightKey)
						}
						b.queueMu.Lock()
						if b.activeWorkers > 0 {
							b.activeWorkers--
						}
						b.queueCond.Broadcast()
						b.queueMu.Unlock()
					}()

					if strings.TrimSpace(req.requestID) != "" {
						b.markRequestRunning(req.requestID)
					}
					b.runQueuedRequest(req)
				}()
			}
		}()
	}
}

func normalizeTaskWorkerLimit(limit int) int {
	if limit < 1 {
		return 1
	}
	if limit > maxTaskWorkerLimit {
		return maxTaskWorkerLimit
	}
	return limit
}

func parseTaskWorkerLimit(raw string) (int, bool) {
	value := strings.ToLower(strings.TrimSpace(raw))
	if value == "" {
		return 0, false
	}
	for _, prefix := range []string{"workers", "worker", "threads", "thread"} {
		if strings.HasPrefix(value, prefix) {
			value = strings.TrimPrefix(value, prefix)
			break
		}
	}
	value = strings.TrimSpace(strings.Trim(value, "=:_-x"))
	if value == "" {
		return 0, false
	}
	num, err := strconv.Atoi(value)
	if err != nil {
		return 0, false
	}
	return normalizeTaskWorkerLimit(num), true
}

func (b *TelegramBot) getTaskWorkerLimit() int {
	b.queueMu.Lock()
	defer b.queueMu.Unlock()
	return b.workerLimit
}

func (b *TelegramBot) setTaskWorkerLimit(limit int) int {
	normalized := normalizeTaskWorkerLimit(limit)
	b.queueMu.Lock()
	b.workerLimit = normalized
	if b.queueCond != nil {
		b.queueCond.Broadcast()
	}
	b.queueMu.Unlock()
	return normalized
}

func normalizeTelegramFormat(format string) string {
	switch strings.ToLower(strings.TrimSpace(format)) {
	case telegramFormatAlac:
		return telegramFormatAlac
	case telegramFormatFlac:
		return telegramFormatFlac
	case telegramFormatAac:
		return telegramFormatAac
	case telegramFormatAtmos:
		return telegramFormatAtmos
	default:
		return ""
	}
}

func normalizeTelegramAACType(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "aac", "aac-lc", "aac-binaural", "aac-downmix":
		return strings.ToLower(strings.TrimSpace(value))
	default:
		return ""
	}
}

func normalizeTelegramMVAudioType(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "atmos", "ac3", "aac":
		return strings.ToLower(strings.TrimSpace(value))
	default:
		return ""
	}
}

func normalizeTelegramLyricsFormat(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "lrc", "ttml":
		return strings.ToLower(strings.TrimSpace(value))
	default:
		return ""
	}
}

func normalizeChatSettings(settings ChatDownloadSettings) ChatDownloadSettings {
	normalizedFormat := normalizeTelegramFormat(settings.Format)
	if normalizedFormat == "" {
		normalizedFormat = defaultTelegramFormat
	}
	aacType := normalizeTelegramAACType(settings.AACType)
	if aacType == "" {
		aacType = normalizeTelegramAACType(Config.AacType)
	}
	if aacType == "" {
		aacType = defaultTelegramAACType
	}
	mvAudioType := normalizeTelegramMVAudioType(settings.MVAudioType)
	if mvAudioType == "" {
		mvAudioType = normalizeTelegramMVAudioType(Config.MVAudioType)
	}
	if mvAudioType == "" {
		mvAudioType = defaultTelegramMVAudioType
	}
	lyricsFormat := normalizeTelegramLyricsFormat(settings.LyricsFormat)
	if lyricsFormat == "" {
		lyricsFormat = normalizeTelegramLyricsFormat(Config.LrcFormat)
	}
	if lyricsFormat == "" {
		lyricsFormat = defaultTelegramLyricsFormat
	}
	songZip := settings.SongZip
	autoLyrics := settings.AutoLyrics
	autoCover := settings.AutoCover
	autoAnimated := settings.AutoAnimated
	if !settings.SettingsInited {
		songZip = false
		autoLyrics = false
		autoCover = false
		autoAnimated = false
	}
	return ChatDownloadSettings{
		Format:         normalizedFormat,
		AACType:        aacType,
		MVAudioType:    mvAudioType,
		LyricsFormat:   lyricsFormat,
		SongZip:        songZip,
		AutoLyrics:     autoLyrics,
		AutoCover:      autoCover,
		AutoAnimated:   autoAnimated,
		SettingsInited: true,
	}
}

func (b *TelegramBot) getChatSettings(chatID int64) ChatDownloadSettings {
	b.settingsMu.Lock()
	defer b.settingsMu.Unlock()
	if b.chatSettings == nil {
		b.chatSettings = make(map[int64]ChatDownloadSettings)
	}
	settings, ok := b.chatSettings[chatID]
	if !ok {
		return normalizeChatSettings(ChatDownloadSettings{})
	}
	normalized := normalizeChatSettings(settings)
	if normalized != settings {
		b.chatSettings[chatID] = normalized
	}
	return normalized
}

func (b *TelegramBot) updateChatSettings(chatID int64, updateFn func(current ChatDownloadSettings) ChatDownloadSettings) ChatDownloadSettings {
	b.settingsMu.Lock()
	if b.chatSettings == nil {
		b.chatSettings = make(map[int64]ChatDownloadSettings)
	}
	current := normalizeChatSettings(b.chatSettings[chatID])
	updated := normalizeChatSettings(updateFn(current))
	b.chatSettings[chatID] = updated
	b.settingsMu.Unlock()
	b.requestStateSave()
	return updated
}

func (b *TelegramBot) setChatFormat(chatID int64, format string) ChatDownloadSettings {
	normalized := normalizeTelegramFormat(format)
	if normalized == "" {
		return b.getChatSettings(chatID)
	}
	return b.updateChatSettings(chatID, func(current ChatDownloadSettings) ChatDownloadSettings {
		current.Format = normalized
		return current
	})
}

func (b *TelegramBot) setChatAACType(chatID int64, aacType string) ChatDownloadSettings {
	normalized := normalizeTelegramAACType(aacType)
	if normalized == "" {
		return b.getChatSettings(chatID)
	}
	return b.updateChatSettings(chatID, func(current ChatDownloadSettings) ChatDownloadSettings {
		current.AACType = normalized
		return current
	})
}

func (b *TelegramBot) setChatMVAudioType(chatID int64, mvAudioType string) ChatDownloadSettings {
	normalized := normalizeTelegramMVAudioType(mvAudioType)
	if normalized == "" {
		return b.getChatSettings(chatID)
	}
	return b.updateChatSettings(chatID, func(current ChatDownloadSettings) ChatDownloadSettings {
		current.MVAudioType = normalized
		return current
	})
}

func (b *TelegramBot) setChatLyricsFormat(chatID int64, lyricsFormat string) ChatDownloadSettings {
	normalized := normalizeTelegramLyricsFormat(lyricsFormat)
	if normalized == "" {
		return b.getChatSettings(chatID)
	}
	return b.updateChatSettings(chatID, func(current ChatDownloadSettings) ChatDownloadSettings {
		current.LyricsFormat = normalized
		return current
	})
}

func (b *TelegramBot) toggleChatAutoLyrics(chatID int64) ChatDownloadSettings {
	return b.updateChatSettings(chatID, func(current ChatDownloadSettings) ChatDownloadSettings {
		current.AutoLyrics = !current.AutoLyrics
		return current
	})
}

func (b *TelegramBot) toggleChatAutoCover(chatID int64) ChatDownloadSettings {
	return b.updateChatSettings(chatID, func(current ChatDownloadSettings) ChatDownloadSettings {
		current.AutoCover = !current.AutoCover
		return current
	})
}

func (b *TelegramBot) toggleChatAutoAnimated(chatID int64) ChatDownloadSettings {
	return b.updateChatSettings(chatID, func(current ChatDownloadSettings) ChatDownloadSettings {
		current.AutoAnimated = !current.AutoAnimated
		return current
	})
}

func (b *TelegramBot) toggleChatSongZip(chatID int64) ChatDownloadSettings {
	return b.updateChatSettings(chatID, func(current ChatDownloadSettings) ChatDownloadSettings {
		current.SongZip = !current.SongZip
		return current
	})
}

func (b *TelegramBot) getChatFormat(chatID int64) string {
	return b.getChatSettings(chatID).Format
}

func cacheProfileKey(settings ChatDownloadSettings) string {
	normalized := normalizeChatSettings(settings)
	return fmt.Sprintf("%s|aac:%s|mv:%s|lyr:%s|auto:%t-%t-%t",
		normalized.Format,
		normalized.AACType,
		normalized.MVAudioType,
		normalized.LyricsFormat,
		normalized.AutoLyrics,
		normalized.AutoCover,
		normalized.AutoAnimated,
	)
}

func (b *TelegramBot) cacheKey(trackID, format string, compressed bool) string {
	normalized := normalizeTelegramFormat(format)
	if normalized == "" {
		normalized = defaultTelegramFormat
	}
	return fmt.Sprintf("%s|%s|%t", trackID, normalized, compressed)
}

func (b *TelegramBot) bundleZipCacheKey(mediaType, mediaID string, settings ChatDownloadSettings) string {
	return fmt.Sprintf("%s:%s|%s|zip", mediaType, mediaID, cacheProfileKey(settings))
}

func (b *TelegramBot) mvCacheKey(mvID string, settings ChatDownloadSettings, mode string) string {
	return fmt.Sprintf("%s:%s|%s|%s", mediaTypeMusicVideo, mvID, cacheProfileKey(settings), mode)
}

func (b *TelegramBot) loadCache() {
	b.cacheMu.Lock()
	defer b.cacheMu.Unlock()
	b.cache = make(map[string]CachedAudio)
	b.docCache = make(map[string]CachedDocument)
	b.videoCache = make(map[string]CachedVideo)
	if b.cacheFile == "" {
		return
	}
	data, err := os.ReadFile(b.cacheFile)
	if err != nil {
		return
	}
	var payload telegramCacheFile
	if err := json.Unmarshal(data, &payload); err != nil {
		return
	}
	if payload.Documents != nil {
		b.docCache = payload.Documents
	}
	if payload.Videos != nil {
		b.videoCache = payload.Videos
	}
	if payload.Items == nil {
		if payload.Version > 0 && payload.Version < 4 {
			b.saveCacheLocked()
		}
		return
	}
	if payload.Version < 2 {
		migrated := make(map[string]CachedAudio)
		for key, entry := range payload.Items {
			parts := strings.Split(key, "|")
			if len(parts) == 2 {
				trackID := parts[0]
				compressed, err := strconv.ParseBool(parts[1])
				if err != nil {
					continue
				}
				entry.Compressed = compressed
				if entry.Format == "" {
					entry.Format = defaultTelegramFormat
				}
				migrated[b.cacheKey(trackID, entry.Format, entry.Compressed)] = entry
				continue
			}
			if len(parts) >= 3 {
				trackID := parts[0]
				format := normalizeTelegramFormat(parts[1])
				compressed, err := strconv.ParseBool(parts[2])
				if err != nil {
					continue
				}
				if format == "" {
					format = defaultTelegramFormat
				}
				entry.Compressed = compressed
				if entry.Format == "" {
					entry.Format = format
				}
				migrated[b.cacheKey(trackID, format, entry.Compressed)] = entry
			}
		}
		b.cache = migrated
		b.saveCacheLocked()
		return
	}
	b.cache = payload.Items
	for key, entry := range b.cache {
		if entry.Format == "" {
			parts := strings.Split(key, "|")
			if len(parts) >= 2 {
				entry.Format = normalizeTelegramFormat(parts[1])
			}
			if entry.Format == "" {
				entry.Format = defaultTelegramFormat
			}
			b.cache[key] = entry
		}
	}
	if payload.Version < 4 {
		b.saveCacheLocked()
	}
}

func (b *TelegramBot) saveCacheLocked() {
	if b.cacheFile == "" {
		return
	}
	dir := filepath.Dir(b.cacheFile)
	if dir != "." && dir != "" {
		_ = os.MkdirAll(dir, 0755)
	}
	payload := telegramCacheFile{
		Version:   4,
		Items:     b.cache,
		Documents: b.docCache,
		Videos:    b.videoCache,
	}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return
	}
	tmp := b.cacheFile + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return
	}
	_ = os.Rename(tmp, b.cacheFile)
}

func (b *TelegramBot) fetchTrackMeta(trackID string) (AudioMeta, error) {
	if trackID == "" {
		return AudioMeta{}, fmt.Errorf("empty track id")
	}
	resp, err := ampapi.GetSongResp(Config.Storefront, trackID, b.searchLanguage(), b.appleToken)
	if err != nil || resp == nil {
		if err != nil {
			return AudioMeta{}, err
		}
		return AudioMeta{}, fmt.Errorf("empty song response")
	}
	data, err := firstSongData("main.telegram.fetchTrackMeta", resp)
	if err != nil {
		return AudioMeta{}, err
	}
	return AudioMeta{
		TrackID:        trackID,
		Title:          strings.TrimSpace(data.Attributes.Name),
		Performer:      strings.TrimSpace(data.Attributes.ArtistName),
		DurationMillis: int64(data.Attributes.DurationInMillis),
	}, nil
}

func (b *TelegramBot) enrichCachedAudio(trackID string, entry CachedAudio) CachedAudio {
	updated := false
	sizeBytes := entry.SizeBytes
	if sizeBytes <= 0 {
		sizeBytes = entry.FileSize
		if sizeBytes > 0 {
			entry.SizeBytes = sizeBytes
			updated = true
		}
	}
	if trackID != "" && (entry.DurationMillis <= 0 || entry.Title == "" || entry.Performer == "") {
		if meta, err := b.fetchTrackMeta(trackID); err == nil {
			if entry.DurationMillis <= 0 && meta.DurationMillis > 0 {
				entry.DurationMillis = meta.DurationMillis
				updated = true
			}
			if entry.Title == "" && meta.Title != "" {
				entry.Title = meta.Title
				updated = true
			}
			if entry.Performer == "" && meta.Performer != "" {
				entry.Performer = meta.Performer
				updated = true
			}
		}
	}
	if entry.BitrateKbps <= 0 && sizeBytes > 0 && entry.DurationMillis > 0 {
		entry.BitrateKbps = calcBitrateKbps(sizeBytes, entry.DurationMillis)
		updated = true
	}
	if updated && trackID != "" {
		b.storeCachedAudio(trackID, entry)
	}
	return entry
}

func (b *TelegramBot) storeCachedAudio(trackID string, entry CachedAudio) {
	if trackID == "" || entry.FileID == "" {
		return
	}
	b.cacheMu.Lock()
	defer b.cacheMu.Unlock()
	if b.cache == nil {
		b.cache = make(map[string]CachedAudio)
	}
	entry.Format = normalizeTelegramFormat(entry.Format)
	if entry.Format == "" {
		entry.Format = defaultTelegramFormat
	}
	entry.UpdatedAt = time.Now()
	b.cache[b.cacheKey(trackID, entry.Format, entry.Compressed)] = entry
	b.saveCacheLocked()
}

func (b *TelegramBot) deleteCachedAudio(trackID, format string, compressed bool) {
	if trackID == "" {
		return
	}
	b.cacheMu.Lock()
	defer b.cacheMu.Unlock()
	if b.cache == nil {
		return
	}
	delete(b.cache, b.cacheKey(trackID, format, compressed))
	b.saveCacheLocked()
}

func (b *TelegramBot) storeCachedDocument(key string, entry CachedDocument) {
	if key == "" || entry.FileID == "" {
		return
	}
	b.cacheMu.Lock()
	defer b.cacheMu.Unlock()
	if b.docCache == nil {
		b.docCache = make(map[string]CachedDocument)
	}
	entry.UpdatedAt = time.Now()
	b.docCache[key] = entry
	b.saveCacheLocked()
}

func (b *TelegramBot) getCachedDocument(key string) (CachedDocument, bool) {
	if key == "" {
		return CachedDocument{}, false
	}
	b.cacheMu.Lock()
	defer b.cacheMu.Unlock()
	if b.docCache == nil {
		return CachedDocument{}, false
	}
	entry, ok := b.docCache[key]
	return entry, ok
}

func (b *TelegramBot) deleteCachedDocument(key string) {
	if key == "" {
		return
	}
	b.cacheMu.Lock()
	defer b.cacheMu.Unlock()
	if b.docCache == nil {
		return
	}
	delete(b.docCache, key)
	b.saveCacheLocked()
}

func (b *TelegramBot) storeCachedVideo(key string, entry CachedVideo) {
	if key == "" || entry.FileID == "" {
		return
	}
	b.cacheMu.Lock()
	defer b.cacheMu.Unlock()
	if b.videoCache == nil {
		b.videoCache = make(map[string]CachedVideo)
	}
	entry.UpdatedAt = time.Now()
	b.videoCache[key] = entry
	b.saveCacheLocked()
}

func (b *TelegramBot) getCachedVideo(key string) (CachedVideo, bool) {
	if key == "" {
		return CachedVideo{}, false
	}
	b.cacheMu.Lock()
	defer b.cacheMu.Unlock()
	if b.videoCache == nil {
		return CachedVideo{}, false
	}
	entry, ok := b.videoCache[key]
	return entry, ok
}

func (b *TelegramBot) deleteCachedVideo(key string) {
	if key == "" {
		return
	}
	b.cacheMu.Lock()
	defer b.cacheMu.Unlock()
	if b.videoCache == nil {
		return
	}
	delete(b.videoCache, key)
	b.saveCacheLocked()
}

func deleteCacheEntriesWithPrefix[T any](items map[string]T, prefix string) int {
	if len(items) == 0 || strings.TrimSpace(prefix) == "" {
		return 0
	}
	removed := 0
	for key := range items {
		if strings.HasPrefix(key, prefix) {
			delete(items, key)
			removed++
		}
	}
	return removed
}

func (b *TelegramBot) purgeTargetCaches(target *AppleURLTarget) int {
	if b == nil || target == nil {
		return 0
	}
	mediaType := strings.TrimSpace(target.MediaType)
	mediaID := strings.TrimSpace(target.ID)
	if mediaType == "" || mediaID == "" {
		return 0
	}

	b.cacheMu.Lock()
	defer b.cacheMu.Unlock()

	removed := 0
	switch mediaType {
	case mediaTypeSong:
		removed += deleteCacheEntriesWithPrefix(b.cache, mediaID+"|")
		removed += deleteCacheEntriesWithPrefix(b.docCache, mediaTypeSong+":"+mediaID+"|")
	case mediaTypeAlbum, mediaTypePlaylist, mediaTypeStation:
		removed += deleteCacheEntriesWithPrefix(b.docCache, mediaType+":"+mediaID+"|")
	case mediaTypeMusicVideo:
		prefix := mediaTypeMusicVideo + ":" + mediaID + "|"
		removed += deleteCacheEntriesWithPrefix(b.docCache, prefix)
		removed += deleteCacheEntriesWithPrefix(b.videoCache, prefix)
	}
	if removed > 0 {
		b.saveCacheLocked()
	}
	return removed
}

func (b *TelegramBot) getCachedAudio(trackID string, maxBytes int64, format string) (CachedAudio, bool) {
	if trackID == "" {
		return CachedAudio{}, false
	}
	b.cacheMu.Lock()
	defer b.cacheMu.Unlock()
	if b.cache == nil {
		return CachedAudio{}, false
	}
	var candidates []CachedAudio
	normalized := normalizeTelegramFormat(format)
	if normalized != "" {
		if entry, ok := b.cache[b.cacheKey(trackID, normalized, false)]; ok {
			if entry.Format == "" {
				entry.Format = normalized
			}
			candidates = append(candidates, entry)
		}
		if entry, ok := b.cache[b.cacheKey(trackID, normalized, true)]; ok {
			if entry.Format == "" {
				entry.Format = normalized
			}
			candidates = append(candidates, entry)
		}
	} else {
		prefix := trackID + "|"
		for key, entry := range b.cache {
			if !strings.HasPrefix(key, prefix) {
				continue
			}
			if entry.Format == "" {
				parts := strings.Split(key, "|")
				if len(parts) >= 3 {
					entry.Format = normalizeTelegramFormat(parts[1])
				}
				if entry.Format == "" {
					entry.Format = defaultTelegramFormat
				}
			}
			candidates = append(candidates, entry)
		}
	}
	var best *CachedAudio
	for _, entry := range candidates {
		entrySize := entry.SizeBytes
		if entrySize <= 0 {
			entrySize = entry.FileSize
		}
		if maxBytes > 0 && entrySize > 0 && entrySize > maxBytes {
			continue
		}
		if best == nil {
			copyEntry := entry
			best = &copyEntry
			continue
		}
		if best.Compressed && !entry.Compressed {
			copyEntry := entry
			best = &copyEntry
			continue
		}
		bestSize := best.SizeBytes
		if bestSize <= 0 {
			bestSize = best.FileSize
		}
		if best.Compressed == entry.Compressed && entrySize > bestSize {
			copyEntry := entry
			best = &copyEntry
		}
	}
	if best == nil {
		return CachedAudio{}, false
	}
	return *best, true
}

func (b *TelegramBot) handleMessage(msg *Message) {
	if msg.Text == "" {
		return
	}
	text := strings.TrimSpace(msg.Text)
	if cmd, args, ok := parseCommand(text); ok {
		if !b.isAllowedChat(msg.Chat.ID) {
			// Allow querying chat_id even when not in allowlist.
			if cmd == "id" && len(args) == 0 {
				_ = b.sendMessage(msg.Chat.ID, formatChatIDText(msg.Chat.ID), nil)
			} else {
				_ = b.sendMessage(msg.Chat.ID, "Not authorized for this bot.", nil)
			}
			return
		}
		b.handleCommand(msg.Chat.ID, msg.Chat.Type, cmd, args, msg.MessageID)
		return
	}
	if !b.isAllowedChat(msg.Chat.ID) {
		_ = b.sendMessage(msg.Chat.ID, "Not authorized for this bot.", nil)
		return
	}
	urlText := extractFirstAppleMusicURL(text)
	if urlText == "" {
		return
	}
	target, err := parseAppleMusicURL(urlText)
	if err != nil {
		_ = b.sendMessageWithReply(msg.Chat.ID, fmt.Sprintf("Unsupported Apple Music URL: %s", urlText), nil, msg.MessageID)
		return
	}
	b.handleURLTarget(msg.Chat.ID, msg.MessageID, target)
}

func (b *TelegramBot) handleCallback(cb *CallbackQuery) {
	if cb == nil || cb.Message == nil {
		return
	}
	if !b.isAllowedChat(cb.Message.Chat.ID) {
		return
	}
	data := strings.TrimSpace(cb.Data)
	if data == "panel_cancel" || data == "setting_exit" || data == "setting_close" {
		b.cancelPanelAndDelete(cb.Message.Chat.ID, cb.Message.MessageID)
		_ = b.answerCallbackQuery(cb.ID)
		return
	}
	if strings.HasPrefix(data, "sel:") {
		numStr := strings.TrimPrefix(data, "sel:")
		if n, err := strconv.Atoi(numStr); err == nil {
			b.handleSelection(cb.Message.Chat.ID, cb.Message.MessageID, n)
		}
	} else if strings.HasPrefix(data, "setting_format:") {
		format := strings.TrimPrefix(data, "setting_format:")
		settings := b.setChatFormat(cb.Message.Chat.ID, format)
		_ = b.editMessageText(cb.Message.Chat.ID, cb.Message.MessageID, b.formatSettingsText(settings), b.buildSettingsKeyboard(settings))
	} else if strings.HasPrefix(data, "setting_aac:") {
		aacType := strings.TrimPrefix(data, "setting_aac:")
		settings := b.setChatAACType(cb.Message.Chat.ID, aacType)
		_ = b.editMessageText(cb.Message.Chat.ID, cb.Message.MessageID, b.formatSettingsText(settings), b.buildSettingsKeyboard(settings))
	} else if strings.HasPrefix(data, "setting_mv_audio:") {
		mvAudioType := strings.TrimPrefix(data, "setting_mv_audio:")
		settings := b.setChatMVAudioType(cb.Message.Chat.ID, mvAudioType)
		_ = b.editMessageText(cb.Message.Chat.ID, cb.Message.MessageID, b.formatSettingsText(settings), b.buildSettingsKeyboard(settings))
	} else if strings.HasPrefix(data, "setting_lyrics_format:") {
		lyricsFormat := strings.TrimPrefix(data, "setting_lyrics_format:")
		settings := b.setChatLyricsFormat(cb.Message.Chat.ID, lyricsFormat)
		_ = b.editMessageText(cb.Message.Chat.ID, cb.Message.MessageID, b.formatSettingsText(settings), b.buildSettingsKeyboard(settings))
	} else if data == "setting_auto:lyrics" {
		settings := b.toggleChatAutoLyrics(cb.Message.Chat.ID)
		_ = b.editMessageText(cb.Message.Chat.ID, cb.Message.MessageID, b.formatSettingsText(settings), b.buildSettingsKeyboard(settings))
	} else if data == "setting_auto:cover" {
		settings := b.toggleChatAutoCover(cb.Message.Chat.ID)
		_ = b.editMessageText(cb.Message.Chat.ID, cb.Message.MessageID, b.formatSettingsText(settings), b.buildSettingsKeyboard(settings))
	} else if data == "setting_auto:animated" {
		settings := b.toggleChatAutoAnimated(cb.Message.Chat.ID)
		_ = b.editMessageText(cb.Message.Chat.ID, cb.Message.MessageID, b.formatSettingsText(settings), b.buildSettingsKeyboard(settings))
	} else if data == "setting_song_zip" {
		settings := b.toggleChatSongZip(cb.Message.Chat.ID)
		_ = b.editMessageText(cb.Message.Chat.ID, cb.Message.MessageID, b.formatSettingsText(settings), b.buildSettingsKeyboard(settings))
	} else if strings.HasPrefix(data, "setting_worker:") {
		workerRaw := strings.TrimPrefix(data, "setting_worker:")
		if limit, ok := parseTaskWorkerLimit(workerRaw); ok {
			_ = b.setTaskWorkerLimit(limit)
		}
		settings := b.getChatSettings(cb.Message.Chat.ID)
		_ = b.editMessageText(cb.Message.Chat.ID, cb.Message.MessageID, b.formatSettingsText(settings), b.buildSettingsKeyboard(settings))
	} else if strings.HasPrefix(data, "setting:") {
		// Backward compatibility for old callbacks.
		format := strings.TrimPrefix(data, "setting:")
		settings := b.setChatFormat(cb.Message.Chat.ID, format)
		_ = b.editMessageText(cb.Message.Chat.ID, cb.Message.MessageID, b.formatSettingsText(settings), b.buildSettingsKeyboard(settings))
	} else if strings.HasPrefix(data, "transfer:") {
		mode := strings.TrimPrefix(data, "transfer:")
		b.handleMediaTransfer(cb.Message.Chat.ID, cb.Message.MessageID, mode)
	} else if strings.HasPrefix(data, "album_transfer:") {
		// Backward compatibility for old callbacks.
		mode := strings.TrimPrefix(data, "album_transfer:")
		b.handleMediaTransfer(cb.Message.Chat.ID, cb.Message.MessageID, mode)
	} else if strings.HasPrefix(data, "artist_rel:") {
		relationship := strings.TrimPrefix(data, "artist_rel:")
		b.handleArtistModeSelection(cb.Message.Chat.ID, cb.Message.MessageID, relationship)
	} else if strings.HasPrefix(data, "page:") {
		deltaStr := strings.TrimPrefix(data, "page:")
		if delta, err := strconv.Atoi(deltaStr); err == nil {
			b.handlePage(cb.Message.Chat.ID, cb.Message.MessageID, delta)
		}
	}
	_ = b.answerCallbackQuery(cb.ID)
}

func (b *TelegramBot) cancelPanelAndDelete(chatID int64, messageID int) {
	b.clearPendingByMessage(chatID, messageID)
	b.clearPendingTransferByMessage(chatID, messageID)
	b.clearPendingArtistModeByMessage(chatID, messageID)
	if err := b.deleteMessage(chatID, messageID); err != nil {
		_ = b.editMessageText(chatID, messageID, "已取消。", nil)
	}
}

func (b *TelegramBot) handleInlineQuery(q *InlineQuery) {
	if q == nil || q.ID == "" {
		return
	}
	query := strings.TrimSpace(q.Query)
	if query == "" {
		_ = b.answerInlineQuery(q.ID, []any{}, true)
		return
	}
	trackID := extractInlineTrackID(query)
	if trackID == "" {
		_ = b.answerInlineQuery(q.ID, []any{}, true)
		return
	}
	entry, ok := b.getCachedAudio(trackID, b.maxFileBytes, "")
	results := []any{}
	if ok {
		entry = b.enrichCachedAudio(trackID, entry)
		format := normalizeTelegramFormat(entry.Format)
		if format == "" {
			format = defaultTelegramFormat
		}
		results = append(results, InlineQueryResultCachedAudio{
			Type:        "audio",
			ID:          fmt.Sprintf("song_%s", trackID),
			AudioFileID: entry.FileID,
			Caption:     formatTelegramCaption(entry.SizeBytes, entry.BitrateKbps, format),
		})
	} else {
		meta, err := b.fetchTrackMeta(trackID)
		title := "Send /songid " + trackID
		description := ""
		if err == nil {
			if meta.Title != "" && meta.Performer != "" {
				title = meta.Performer + " - " + meta.Title
				description = "Send /songid " + trackID
			} else if meta.Title != "" {
				title = meta.Title
				description = "Send /songid " + trackID
			}
		}
		results = append(results, InlineQueryResultArticle{
			Type:        "article",
			ID:          fmt.Sprintf("songcmd_%s", trackID),
			Title:       title,
			Description: description,
			InputMessageContent: InputMessageContent{
				MessageText: "/songid " + trackID,
			},
		})
	}
	_ = b.answerInlineQuery(q.ID, results, true)
}

func (b *TelegramBot) handleCommand(chatID int64, chatType string, cmd string, args []string, replyToID int) {
	cmd = normalizeTelegramBotCommand(cmd)
	switch cmd {
	case "start", "help":
		_ = b.sendMessage(chatID, botHelpText(), nil)
	case "search_song":
		b.handleSearch(chatID, "song", strings.Join(args, " "), replyToID)
	case "search_album":
		b.handleSearch(chatID, "album", strings.Join(args, " "), replyToID)
	case "search_artist":
		b.handleSearch(chatID, "artist", strings.Join(args, " "), replyToID)
	case "search":
		if len(args) < 2 {
			_ = b.sendMessageWithReply(chatID, "Usage: /search <song|album|artist> <keywords>", nil, replyToID)
			return
		}
		kind := strings.ToLower(args[0])
		b.handleSearch(chatID, kind, strings.Join(args[1:], " "), replyToID)
	case "url":
		if len(args) == 0 {
			_ = b.sendMessageWithReply(chatID, "Usage: /url <apple-music-url>", nil, replyToID)
			return
		}
		raw := extractFirstAppleMusicURL(strings.Join(args, " "))
		if raw == "" {
			raw = args[0]
		}
		target, err := parseAppleMusicURL(raw)
		if err != nil {
			_ = b.sendMessageWithReply(chatID, fmt.Sprintf("Unsupported Apple Music URL: %s", raw), nil, replyToID)
			return
		}
		b.handleURLTarget(chatID, replyToID, target)
	case "refresh":
		target, err := resolveRefreshURLTarget(args)
		if err != nil {
			_ = b.sendMessageWithReply(chatID, "Usage: /refresh <apple-music-url> OR /refresh url <apple-music-url>", nil, replyToID)
			return
		}
		if target.MediaType == mediaTypeArtist {
			_ = b.sendMessageWithReply(chatID, "refresh supports song/album/playlist/station/mv URLs only.", nil, replyToID)
			return
		}
		b.handleURLTargetWithOptions(chatID, replyToID, target, true)
	case "artistphoto":
		target, err := resolveCommandTarget(args, mediaTypeArtist)
		if err != nil {
			_ = b.sendMessageWithReply(chatID, "Usage: /artistphoto <artist-url|artist-id>", nil, replyToID)
			return
		}
		if target.MediaType != mediaTypeArtist {
			_ = b.sendMessageWithReply(chatID, "artistphoto only supports artist URL/ID.", nil, replyToID)
			return
		}
		b.promptMediaTransfer(chatID, mediaTypeArtistAsset, target.ID, target.Storefront, "", replyToID, false)
	case "cover":
		target, err := resolveCommandTarget(args, "")
		if err != nil {
			_ = b.sendMessageWithReply(chatID, "Usage: /cover <apple-music-url> OR /cover <song|album|playlist|station|mv|artist> <id>", nil, replyToID)
			return
		}
		b.enqueueCoverTask(chatID, replyToID, target)
	case "animatedcover", "motioncover":
		target, err := resolveCommandTarget(args, "")
		if err != nil {
			_ = b.sendMessageWithReply(chatID, "Usage: /animatedcover <apple-music-url> OR /animatedcover <song|album|playlist|station> <id>", nil, replyToID)
			return
		}
		b.enqueueAnimatedCoverTask(chatID, replyToID, target)
	case "lyrics", "lyric":
		if len(args) == 0 {
			_ = b.sendMessageWithReply(chatID, "Usage: /lyrics <song-url|song-id|album-url|album <id>>", nil, replyToID)
			return
		}
		target, err := resolveCommandTarget(args, "")
		if err != nil && len(args) == 1 {
			target, err = resolveCommandTarget(args, mediaTypeSong)
		}
		if err != nil {
			_ = b.sendMessageWithReply(chatID, "Usage: /lyrics <song-url|song-id|album-url|album <id>>", nil, replyToID)
			return
		}
		switch target.MediaType {
		case mediaTypeSong:
			b.enqueueSongLyricsTask(chatID, replyToID, target)
		case mediaTypeAlbum:
			b.promptMediaTransfer(chatID, mediaTypeAlbumLyrics, target.ID, target.Storefront, "", replyToID, false)
		default:
			_ = b.sendMessageWithReply(chatID, "lyrics command supports song/album only.", nil, replyToID)
		}
	case "id":
		if len(args) == 0 {
			_ = b.sendMessage(chatID, formatChatIDText(chatID), nil)
			return
		}
		if len(args) == 1 {
			if target, err := parseAppleMusicURL(args[0]); err == nil {
				b.handleURLTarget(chatID, replyToID, target)
				return
			}
			b.queueDownloadSong(chatID, args[0])
			return
		}
		kind := strings.ToLower(args[0])
		switch kind {
		case "song":
			b.queueDownloadSong(chatID, args[1])
		case "album":
			b.queueDownloadAlbum(chatID, args[1])
		case "playlist":
			b.queueDownloadPlaylist(chatID, args[1])
		case "station":
			b.queueDownloadStation(chatID, args[1])
		case "mv", "music-video", "musicvideo":
			b.queueDownloadMusicVideo(chatID, args[1])
		case "artist":
			b.startArtistSelection(chatID, args[1], "", Config.Storefront, replyToID)
		default:
			_ = b.sendMessage(chatID, "Usage: /id <song|album|playlist|station|mv|artist> <id>", nil)
		}
	case "songid":
		if len(args) == 0 {
			_ = b.sendMessage(chatID, "Usage: /songid <id>", nil)
			return
		}
		b.queueDownloadSong(chatID, args[0])
	case "albumid":
		if len(args) == 0 {
			_ = b.sendMessage(chatID, "Usage: /albumid <id>", nil)
			return
		}
		b.queueDownloadAlbum(chatID, args[0])
	case "playlistid":
		if len(args) == 0 {
			_ = b.sendMessage(chatID, "Usage: /playlistid <id>", nil)
			return
		}
		b.queueDownloadPlaylist(chatID, args[0])
	case "stationid":
		if len(args) == 0 {
			_ = b.sendMessage(chatID, "Usage: /stationid <id>", nil)
			return
		}
		b.queueDownloadStation(chatID, args[0])
	case "mvid":
		if len(args) == 0 {
			_ = b.sendMessage(chatID, "Usage: /mvid <id>", nil)
			return
		}
		b.queueDownloadMusicVideo(chatID, args[0])
	case "artistid":
		if len(args) == 0 {
			_ = b.sendMessage(chatID, "Usage: /artistid <id>", nil)
			return
		}
		b.startArtistSelection(chatID, args[0], "", Config.Storefront, replyToID)
	case "settings":
		if len(args) > 0 {
			settings := b.getChatSettings(chatID)
			raw := strings.ToLower(strings.TrimSpace(args[0]))
			if normalized := normalizeTelegramFormat(raw); normalized != "" {
				settings = b.setChatFormat(chatID, normalized)
				_ = b.sendMessageWithReply(chatID, b.formatSettingsText(settings), b.buildSettingsKeyboard(settings), replyToID)
				return
			}
			if normalized := normalizeTelegramAACType(raw); normalized != "" {
				settings = b.setChatAACType(chatID, normalized)
				_ = b.sendMessageWithReply(chatID, b.formatSettingsText(settings), b.buildSettingsKeyboard(settings), replyToID)
				return
			}
			if normalized := normalizeTelegramMVAudioType(raw); normalized != "" {
				settings = b.setChatMVAudioType(chatID, normalized)
				_ = b.sendMessageWithReply(chatID, b.formatSettingsText(settings), b.buildSettingsKeyboard(settings), replyToID)
				return
			}
			if normalized := normalizeTelegramLyricsFormat(raw); normalized != "" {
				settings = b.setChatLyricsFormat(chatID, normalized)
				_ = b.sendMessageWithReply(chatID, b.formatSettingsText(settings), b.buildSettingsKeyboard(settings), replyToID)
				return
			}
			if workerLimit, ok := parseTaskWorkerLimit(raw); ok {
				b.setTaskWorkerLimit(workerLimit)
				settings = b.getChatSettings(chatID)
				_ = b.sendMessageWithReply(chatID, b.formatSettingsText(settings), b.buildSettingsKeyboard(settings), replyToID)
				return
			}
			switch raw {
			case "lyrics", "lyrics_on", "lyrics_off":
				if raw == "lyrics_on" {
					if !settings.AutoLyrics {
						settings = b.toggleChatAutoLyrics(chatID)
					}
				} else if raw == "lyrics_off" {
					if settings.AutoLyrics {
						settings = b.toggleChatAutoLyrics(chatID)
					}
				} else {
					settings = b.toggleChatAutoLyrics(chatID)
				}
				_ = b.sendMessageWithReply(chatID, b.formatSettingsText(settings), b.buildSettingsKeyboard(settings), replyToID)
				return
			case "cover", "cover_on", "cover_off":
				if raw == "cover_on" {
					if !settings.AutoCover {
						settings = b.toggleChatAutoCover(chatID)
					}
				} else if raw == "cover_off" {
					if settings.AutoCover {
						settings = b.toggleChatAutoCover(chatID)
					}
				} else {
					settings = b.toggleChatAutoCover(chatID)
				}
				_ = b.sendMessageWithReply(chatID, b.formatSettingsText(settings), b.buildSettingsKeyboard(settings), replyToID)
				return
			case "animated", "animated_on", "animated_off":
				if raw == "animated_on" {
					if !settings.AutoAnimated {
						settings = b.toggleChatAutoAnimated(chatID)
					}
				} else if raw == "animated_off" {
					if settings.AutoAnimated {
						settings = b.toggleChatAutoAnimated(chatID)
					}
				} else {
					settings = b.toggleChatAutoAnimated(chatID)
				}
				_ = b.sendMessageWithReply(chatID, b.formatSettingsText(settings), b.buildSettingsKeyboard(settings), replyToID)
				return
			case "songzip", "song_zip", "songzip_toggle", "song_zip_toggle":
				settings = b.toggleChatSongZip(chatID)
				_ = b.sendMessageWithReply(chatID, b.formatSettingsText(settings), b.buildSettingsKeyboard(settings), replyToID)
				return
			case "songzip_on", "song_zip_on", "songzip_zip", "song_zip_zip":
				if !settings.SongZip {
					settings = b.toggleChatSongZip(chatID)
				}
				_ = b.sendMessageWithReply(chatID, b.formatSettingsText(settings), b.buildSettingsKeyboard(settings), replyToID)
				return
			case "songzip_off", "song_zip_off", "song_one", "song_onebyone", "song_one_by_one":
				if settings.SongZip {
					settings = b.toggleChatSongZip(chatID)
				}
				_ = b.sendMessageWithReply(chatID, b.formatSettingsText(settings), b.buildSettingsKeyboard(settings), replyToID)
				return
			}
			_ = b.sendMessageWithReply(chatID, "Usage: /settings [alac|flac|aac|atmos|aac-lc|aac-binaural|aac-downmix|ac3|lrc|ttml|lyrics|cover|animated|songzip|worker1..worker4]", nil, replyToID)
			return
		}
		settings := b.getChatSettings(chatID)
		_ = b.sendMessageWithReply(chatID, b.formatSettingsText(settings), b.buildSettingsKeyboard(settings), replyToID)
	default:
		if strings.EqualFold(strings.TrimSpace(chatType), "private") {
			_ = b.sendMessage(chatID, "Unknown command. Send /help for usage.", nil)
		}
		return
	}
}

func normalizeTelegramBotCommand(cmd string) string {
	switch strings.ToLower(strings.TrimSpace(cmd)) {
	case "h":
		return "help"
	case "i":
		return "id"
	case "sg":
		return "search_song"
	case "sa":
		return "search_album"
	case "sr":
		return "search_artist"
	case "s":
		return "search"
	case "u":
		return "url"
	case "ap":
		return "artistphoto"
	case "cv":
		return "cover"
	case "ac":
		return "animatedcover"
	case "ly":
		return "lyrics"
	case "st":
		return "settings"
	case "rf":
		return "refresh"
	default:
		return cmd
	}
}

func (b *TelegramBot) handleURLTarget(chatID int64, replyToID int, target *AppleURLTarget) {
	b.handleURLTargetWithOptions(chatID, replyToID, target, false)
}

func (b *TelegramBot) handleURLTargetWithOptions(chatID int64, replyToID int, target *AppleURLTarget, forceRefresh bool) {
	if target == nil {
		_ = b.sendMessageWithReply(chatID, "Invalid Apple Music URL.", nil, replyToID)
		return
	}
	switch target.MediaType {
	case mediaTypeSong:
		b.queueDownloadSongWithStorefrontOptions(chatID, target.ID, target.Storefront, replyToID, forceRefresh)
	case mediaTypeAlbum:
		b.queueDownloadAlbumWithStorefrontOptions(chatID, target.ID, target.Storefront, replyToID, forceRefresh)
	case mediaTypePlaylist:
		b.queueDownloadPlaylistWithStorefrontOptions(chatID, target.ID, target.Storefront, replyToID, forceRefresh)
	case mediaTypeStation:
		b.queueDownloadStationWithStorefront(chatID, target.ID, target.Storefront, replyToID, forceRefresh)
	case mediaTypeMusicVideo:
		b.queueDownloadMusicVideoWithStorefront(chatID, target.ID, target.Storefront, replyToID, forceRefresh)
	case mediaTypeArtist:
		artistName := ""
		if target.RawURL != "" {
			if name, _, err := getUrlArtistName(target.RawURL, b.appleToken); err == nil {
				artistName = name
			}
		}
		storefront := target.Storefront
		if storefront == "" {
			storefront = Config.Storefront
		}
		b.startArtistSelection(chatID, target.ID, artistName, storefront, replyToID)
	default:
		_ = b.sendMessageWithReply(chatID, "Unsupported Apple Music URL type.", nil, replyToID)
	}
}

type artworkFetchResult struct {
	DisplayName string
	CoverURL    string
	MotionURL   string
}

type artistProfileResponse struct {
	Data []struct {
		ID         string `json:"id"`
		Attributes struct {
			Name    string `json:"name"`
			Artwork struct {
				URL string `json:"url"`
			} `json:"artwork"`
		} `json:"attributes"`
	} `json:"data"`
}

func normalizeCommandMediaType(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case mediaTypeSong, "songs":
		return mediaTypeSong
	case mediaTypeAlbum, "albums":
		return mediaTypeAlbum
	case mediaTypePlaylist, "playlists":
		return mediaTypePlaylist
	case mediaTypeStation, "stations":
		return mediaTypeStation
	case "mv", "mvs", "video", "videos", mediaTypeMusicVideo, "musicvideo", "musicvideos":
		return mediaTypeMusicVideo
	case mediaTypeArtist, "artists":
		return mediaTypeArtist
	default:
		return ""
	}
}

func resolveCommandTarget(args []string, defaultType string) (*AppleURLTarget, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("empty args")
	}
	joined := strings.TrimSpace(strings.Join(args, " "))
	if urlText := extractFirstAppleMusicURL(joined); urlText != "" {
		return parseAppleMusicURL(urlText)
	}
	first := strings.TrimSpace(args[0])
	lowerFirst := strings.ToLower(first)
	if strings.HasPrefix(lowerFirst, "http://") || strings.HasPrefix(lowerFirst, "https://") {
		return parseAppleMusicURL(first)
	}
	if len(args) >= 2 {
		mediaType := normalizeCommandMediaType(args[0])
		mediaID := strings.TrimSpace(args[1])
		if mediaType != "" && mediaID != "" {
			return &AppleURLTarget{
				MediaType:  mediaType,
				Storefront: Config.Storefront,
				ID:         mediaID,
			}, nil
		}
	}
	defaultType = normalizeCommandMediaType(defaultType)
	if defaultType != "" {
		mediaID := strings.TrimSpace(args[0])
		if mediaID == "" {
			return nil, fmt.Errorf("empty media id")
		}
		return &AppleURLTarget{
			MediaType:  defaultType,
			Storefront: Config.Storefront,
			ID:         mediaID,
		}, nil
	}
	return nil, fmt.Errorf("unable to resolve target")
}

func resolveRefreshURLTarget(args []string) (*AppleURLTarget, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("empty args")
	}
	trimmedArgs := args
	if len(trimmedArgs) > 0 {
		switch strings.ToLower(strings.TrimSpace(trimmedArgs[0])) {
		case "url", "ulr":
			trimmedArgs = trimmedArgs[1:]
		}
	}
	if len(trimmedArgs) == 0 {
		return nil, fmt.Errorf("missing url")
	}
	joined := strings.TrimSpace(strings.Join(trimmedArgs, " "))
	raw := extractFirstAppleMusicURL(joined)
	if raw == "" {
		raw = strings.TrimSpace(trimmedArgs[0])
	}
	return parseAppleMusicURL(raw)
}

func normalizeLyricsOutputFormat(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "lrc":
		return "lrc"
	case "ttml":
		return "ttml"
	default:
		return ""
	}
}

func sanitizeFileBaseName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "apple-music"
	}
	name = forbiddenNames.ReplaceAllString(name, "_")
	name = strings.ReplaceAll(name, "\n", " ")
	name = strings.Join(strings.Fields(name), " ")
	if len([]rune(name)) > 80 {
		name = string([]rune(name)[:80])
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return "apple-music"
	}
	return name
}

func resolveStorefront(target *AppleURLTarget) string {
	if target != nil && strings.TrimSpace(target.Storefront) != "" {
		return target.Storefront
	}
	storefront := strings.TrimSpace(Config.Storefront)
	if storefront == "" {
		storefront = "us"
	}
	return storefront
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func composeArtistTitle(artistName, title string) string {
	artistName = strings.TrimSpace(artistName)
	title = strings.TrimSpace(title)
	if artistName != "" && title != "" {
		return artistName + " - " + title
	}
	if title != "" {
		return title
	}
	return artistName
}

func firstSongData(op string, resp *ampapi.SongResp) (*ampapi.SongRespData, error) {
	if resp == nil {
		return nil, &safe.AccessError{Op: op, Path: "song.response", Reason: "nil response"}
	}
	return safe.FirstRef(op, "song.data", resp.Data)
}

func firstAlbumData(op string, resp *ampapi.AlbumResp) (*ampapi.AlbumRespData, error) {
	if resp == nil {
		return nil, &safe.AccessError{Op: op, Path: "album.response", Reason: "nil response"}
	}
	return safe.FirstRef(op, "album.data", resp.Data)
}

func firstPlaylistData(op string, resp *ampapi.PlaylistResp) (*ampapi.PlaylistRespData, error) {
	if resp == nil {
		return nil, &safe.AccessError{Op: op, Path: "playlist.response", Reason: "nil response"}
	}
	return safe.FirstRef(op, "playlist.data", resp.Data)
}

func firstStationData(op string, resp *ampapi.StationResp) (*ampapi.StationRespData, error) {
	if resp == nil {
		return nil, &safe.AccessError{Op: op, Path: "station.response", Reason: "nil response"}
	}
	return safe.FirstRef(op, "station.data", resp.Data)
}

func firstMusicVideoData(op string, resp *ampapi.MusicVideoResp) (*ampapi.MusicVideoRespData, error) {
	if resp == nil {
		return nil, &safe.AccessError{Op: op, Path: "music_video.response", Reason: "nil response"}
	}
	return safe.FirstRef(op, "music_video.data", resp.Data)
}

func firstSongAlbumID(op string, item *ampapi.SongRespData) (string, error) {
	if item == nil {
		return "", &safe.AccessError{Op: op, Path: "song.data[0]", Reason: "nil track item"}
	}
	albumRef, err := safe.FirstRef(op, "song.data[0].relationships.albums.data", item.Relationships.Albums.Data)
	if err != nil {
		return "", err
	}
	albumID := strings.TrimSpace(albumRef.ID)
	if albumID == "" {
		return "", &safe.AccessError{Op: op, Path: "song.data[0].relationships.albums.data[0].id", Reason: "empty album id"}
	}
	return albumID, nil
}

func firstGenreName(op string, genres []string) (string, error) {
	return safe.FirstString(op, "attributes.genreNames", genres)
}

func runExternalCommand(ctx context.Context, name string, args ...string) (cmdrunner.Result, error) {
	result, err := cmdrunner.RunWithOptions(ctx, name, args, cmdrunner.RunOptions{})
	if cmdrunner.IsTimeout(err) {
		appRuntimeMetrics.recordExternalCommandTimeout()
	}
	return result, err
}

func runExternalCommandInDir(ctx context.Context, dir string, name string, args ...string) (cmdrunner.Result, error) {
	result, err := cmdrunner.RunWithOptions(ctx, name, args, cmdrunner.RunOptions{Dir: dir})
	if cmdrunner.IsTimeout(err) {
		appRuntimeMetrics.recordExternalCommandTimeout()
	}
	return result, err
}

func (b *TelegramBot) catalogService() *sharedcatalog.Service {
	return &sharedcatalog.Service{
		AppleToken:     b.appleToken,
		MediaUserToken: Config.MediaUserToken,
		Language:       Config.Language,
		HTTPClient:     b.client,
		UserAgent:      "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.124 Safari/537.36",
		OpPrefix:       "main.telegram",
	}
}

func (b *TelegramBot) fetchArtistProfile(storefront string, artistID string) (string, string, error) {
	return b.catalogService().FetchArtistProfile(storefront, artistID)
}

func (b *TelegramBot) fetchArtwork(target *AppleURLTarget) (artworkFetchResult, error) {
	if target == nil {
		return artworkFetchResult{}, fmt.Errorf("invalid target")
	}
	info, err := b.catalogService().FetchArtwork(sharedcatalog.ArtworkTarget{
		MediaType:  target.MediaType,
		ID:         target.ID,
		Storefront: resolveStorefront(target),
	})
	if err != nil {
		return artworkFetchResult{}, err
	}
	return artworkFetchResult{
		DisplayName: info.DisplayName,
		CoverURL:    info.CoverURL,
		MotionURL:   info.MotionURL,
	}, nil
}

func renderCoverToTemp(coverURL string) (string, string, error) {
	tmpDir, err := os.MkdirTemp("", "amdl-cover-*")
	if err != nil {
		return "", "", err
	}
	coverPath, err := writeCover(tmpDir, "cover", coverURL)
	if err != nil {
		_ = os.RemoveAll(tmpDir)
		return "", "", err
	}
	return coverPath, tmpDir, nil
}

func copyTempFile(srcPath, dstPath string) error {
	src, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer src.Close()
	dst, err := os.Create(dstPath)
	if err != nil {
		return err
	}
	if _, err := io.Copy(dst, src); err != nil {
		_ = dst.Close()
		return err
	}
	return dst.Close()
}

func (b *TelegramBot) saveAnimatedCover(motionURL string, savePath string) error {
	if strings.TrimSpace(motionURL) == "" {
		return fmt.Errorf("motion url is empty")
	}
	videoURL, err := extractVideo(motionURL)
	if err != nil {
		return err
	}
	result, err := runExternalCommand(context.Background(), "ffmpeg", "-loglevel", "error", "-y", "-i", videoURL, "-c", "copy", savePath)
	if err != nil {
		outText := strings.TrimSpace(result.Combined)
		if outText == "" {
			outText = err.Error()
		}
		return fmt.Errorf("%s", outText)
	}
	return nil
}

func (b *TelegramBot) exportArtistAssets(chatID int64, replyToID int, artistID string, storefront string, transferMode string) {
	if strings.TrimSpace(artistID) == "" {
		_ = b.sendMessageWithReply(chatID, "Artist ID is empty.", nil, replyToID)
		return
	}
	storefront = resolveStorefront(&AppleURLTarget{Storefront: storefront})
	status, err := newDownloadStatus(b, chatID, replyToID)
	if err != nil {
		_ = b.sendMessageWithReply(chatID, fmt.Sprintf("Failed to create status message: %v", err), nil, replyToID)
		return
	}
	defer status.Stop()

	tmpDir, err := os.MkdirTemp("", "amdl-artist-assets-*")
	if err != nil {
		status.UpdateSync(fmt.Sprintf("Failed to create temp directory: %v", err), 0, 0)
		return
	}
	defer os.RemoveAll(tmpDir)

	usedNames := make(map[string]struct{})
	assetPaths := []string{}
	coverCount := 0
	motionCount := 0
	failedAlbums := 0
	ffmpegOK := true
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		ffmpegOK = false
	}

	status.Update("Loading artist profile", 0, 0)
	artistInfo, artistErr := b.fetchArtwork(&AppleURLTarget{
		MediaType:  mediaTypeArtist,
		Storefront: storefront,
		ID:         artistID,
	})
	artistName := sanitizeFileBaseName(firstNonEmpty(artistInfo.DisplayName, "artist-"+artistID))
	if artistErr == nil && strings.TrimSpace(artistInfo.CoverURL) != "" {
		profilePath, profileTmp, err := renderCoverToTemp(artistInfo.CoverURL)
		if err == nil {
			profileName := uniqueName(usedNames, artistName+"-artist-photo"+strings.ToLower(filepath.Ext(profilePath)))
			profileDst := filepath.Join(tmpDir, profileName)
			if err := copyTempFile(profilePath, profileDst); err == nil {
				assetPaths = append(assetPaths, profileDst)
				coverCount++
			}
		}
		if strings.TrimSpace(profileTmp) != "" {
			_ = os.RemoveAll(profileTmp)
		}
	}

	status.Update("Loading artist albums", 0, 0)
	albums, _, err := apputils.FetchArtistAlbums(storefront, artistID, b.appleToken, 0, 0, b.searchLanguage())
	if err != nil {
		status.UpdateSync(fmt.Sprintf("Failed to load artist albums: %v", err), 0, 0)
		return
	}
	if strings.HasPrefix(artistName, "artist-") && len(albums) > 0 {
		if resolvedName := sanitizeFileBaseName(albums[0].Artist); resolvedName != "" {
			artistName = resolvedName
		}
	}
	totalAlbums := len(albums)
	for i, album := range albums {
		status.Update("Collecting artist assets", int64(i), int64(totalAlbums))
		if strings.TrimSpace(album.ID) == "" {
			continue
		}
		info, err := b.fetchArtwork(&AppleURLTarget{
			MediaType:  mediaTypeAlbum,
			Storefront: storefront,
			ID:         album.ID,
		})
		if err != nil {
			failedAlbums++
			continue
		}
		albumBase := sanitizeFileBaseName(firstNonEmpty(info.DisplayName, album.Name, "album-"+album.ID))

		if strings.TrimSpace(info.CoverURL) != "" {
			coverPath, coverTmp, err := renderCoverToTemp(info.CoverURL)
			if err == nil {
				coverName := uniqueName(usedNames, fmt.Sprintf("%03d-%s-cover%s", i+1, albumBase, strings.ToLower(filepath.Ext(coverPath))))
				coverDst := filepath.Join(tmpDir, coverName)
				if err := copyTempFile(coverPath, coverDst); err == nil {
					assetPaths = append(assetPaths, coverDst)
					coverCount++
				}
			}
			if strings.TrimSpace(coverTmp) != "" {
				_ = os.RemoveAll(coverTmp)
			}
		}

		if ffmpegOK && strings.TrimSpace(info.MotionURL) != "" {
			motionName := uniqueName(usedNames, fmt.Sprintf("%03d-%s-animated-cover.mp4", i+1, albumBase))
			motionDst := filepath.Join(tmpDir, motionName)
			if err := b.saveAnimatedCover(info.MotionURL, motionDst); err == nil {
				assetPaths = append(assetPaths, motionDst)
				motionCount++
			}
		}
	}

	if !ffmpegOK {
		_ = b.sendMessageWithReply(chatID, "ffmpeg 不可用，已跳过动态封面导出。", nil, replyToID)
	}
	if len(assetPaths) == 0 {
		status.UpdateSync("No artist assets exported.", 0, 0)
		return
	}

	if transferMode == transferModeZip {
		status.Update("Building ZIP", int64(len(assetPaths)), int64(len(assetPaths)))
		zipPath, displayName, err := createZipFromPaths(assetPaths)
		if err != nil {
			status.UpdateSync(fmt.Sprintf("Failed to build ZIP: %v", err), 0, 0)
			return
		}
		defer os.Remove(zipPath)
		if artistName != "" {
			displayName = artistName + ".artist-assets.zip"
		}
		if err := b.sendDocumentFile(chatID, zipPath, displayName, replyToID, status, ""); err == nil {
			status.Stop()
			_ = b.deleteMessage(chatID, status.messageID)
		} else if strings.Contains(strings.ToLower(err.Error()), "zip exceeds telegram limit") {
			status.UpdateSync("ZIP exceeds Telegram size limit, fallback to one-by-one.", 0, 0)
			transferMode = transferModeOneByOne
		} else {
			status.UpdateSync(fmt.Sprintf("Failed to send ZIP: %v", err), 0, 0)
			return
		}
	}

	if transferMode == transferModeOneByOne {
		sentCount := 0
		for idx, path := range assetPaths {
			status.Update("Sending artist assets", int64(idx), int64(len(assetPaths)))
			ext := strings.ToLower(filepath.Ext(path))
			var err error
			if ext == ".mp4" {
				err = b.sendVideoFile(chatID, path, replyToID, "", status, "")
				if err != nil {
					err = b.sendDocumentFile(chatID, path, filepath.Base(path), replyToID, status, "")
				}
			} else {
				err = b.sendDocumentFile(chatID, path, filepath.Base(path), replyToID, status, "")
			}
			if err == nil {
				sentCount++
			}
		}
		status.Stop()
		_ = b.deleteMessage(chatID, status.messageID)
		_ = b.sendMessageWithReply(chatID, fmt.Sprintf("Artist assets done: sent=%d, covers=%d, animated=%d, failed_albums=%d.", sentCount, coverCount, motionCount, failedAlbums), nil, replyToID)
		return
	}
}

func (b *TelegramBot) handleCoverOnly(chatID int64, replyToID int, target *AppleURLTarget, artistOnly bool) {
	info, err := b.fetchArtwork(target)
	if err != nil {
		_ = b.sendMessageWithReply(chatID, fmt.Sprintf("Failed to fetch cover info: %v", err), nil, replyToID)
		return
	}
	coverPath, tmpDir, err := renderCoverToTemp(info.CoverURL)
	if err != nil {
		_ = b.sendMessageWithReply(chatID, fmt.Sprintf("Failed to download cover: %v", err), nil, replyToID)
		return
	}
	defer os.RemoveAll(tmpDir)
	base := sanitizeFileBaseName(info.DisplayName)
	suffix := "-cover"
	if artistOnly {
		suffix = "-artist-photo"
	}
	displayName := base + suffix + strings.ToLower(filepath.Ext(coverPath))
	if err := b.sendDocumentFile(chatID, coverPath, displayName, replyToID, nil, ""); err != nil {
		_ = b.sendMessageWithReply(chatID, fmt.Sprintf("Failed to send cover: %v", err), nil, replyToID)
		return
	}
}

func (b *TelegramBot) enqueueCoverTask(chatID int64, replyToID int, target *AppleURLTarget) {
	if target == nil || strings.TrimSpace(target.MediaType) == "" || strings.TrimSpace(target.ID) == "" {
		_ = b.sendMessageWithReply(chatID, "Invalid Apple Music URL.", nil, replyToID)
		return
	}
	settings := b.getChatSettings(chatID)
	_ = b.enqueueTelegramTask(&downloadRequest{
		chatID:       chatID,
		replyToID:    replyToID,
		single:       true,
		taskType:     telegramTaskCover,
		settings:     settings,
		transferMode: transferModeOneByOne,
		mediaType:    target.MediaType,
		mediaID:      target.ID,
		storefront:   resolveStorefront(target),
		requestID:    b.nextRequestID(),
	})
}

func (b *TelegramBot) handleAnimatedCoverOnly(chatID int64, replyToID int, target *AppleURLTarget) {
	if target == nil {
		_ = b.sendMessageWithReply(chatID, "Invalid target.", nil, replyToID)
		return
	}
	switch target.MediaType {
	case mediaTypeSong, mediaTypeAlbum, mediaTypePlaylist, mediaTypeStation:
	default:
		_ = b.sendMessageWithReply(chatID, "animatedcover only supports song/album/playlist/station.", nil, replyToID)
		return
	}
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		_ = b.sendMessageWithReply(chatID, "animatedcover requires ffmpeg in PATH.", nil, replyToID)
		return
	}
	info, err := b.fetchArtwork(target)
	if err != nil {
		_ = b.sendMessageWithReply(chatID, fmt.Sprintf("Failed to fetch artwork info: %v", err), nil, replyToID)
		return
	}
	if strings.TrimSpace(info.MotionURL) == "" {
		_ = b.sendMessageWithReply(chatID, "No animated cover found for this item.", nil, replyToID)
		return
	}
	videoURL, err := extractVideo(info.MotionURL)
	if err != nil {
		_ = b.sendMessageWithReply(chatID, fmt.Sprintf("Animated cover not available: %v", err), nil, replyToID)
		return
	}
	tmp, err := os.CreateTemp("", "amdl-animated-cover-*.mp4")
	if err != nil {
		_ = b.sendMessageWithReply(chatID, fmt.Sprintf("Failed to create temp file: %v", err), nil, replyToID)
		return
	}
	tmpPath := tmp.Name()
	_ = tmp.Close()
	defer os.Remove(tmpPath)
	result, err := runExternalCommand(context.Background(), "ffmpeg", "-loglevel", "error", "-y", "-i", videoURL, "-c", "copy", tmpPath)
	if err != nil {
		errText := strings.TrimSpace(result.Combined)
		if errText == "" {
			errText = err.Error()
		}
		_ = b.sendMessageWithReply(chatID, fmt.Sprintf("Failed to download animated cover: %s", errText), nil, replyToID)
		return
	}
	displayName := sanitizeFileBaseName(info.DisplayName) + "-animated-cover.mp4"
	if err := b.sendVideoFile(chatID, tmpPath, replyToID, "", nil, ""); err == nil {
		return
	} else {
		if docErr := b.sendDocumentFile(chatID, tmpPath, displayName, replyToID, nil, ""); docErr != nil {
			_ = b.sendMessageWithReply(chatID, fmt.Sprintf("Failed to send animated cover: %v; fallback failed: %v", err, docErr), nil, replyToID)
			return
		}
	}
}

func (b *TelegramBot) enqueueAnimatedCoverTask(chatID int64, replyToID int, target *AppleURLTarget) {
	if target == nil || strings.TrimSpace(target.MediaType) == "" || strings.TrimSpace(target.ID) == "" {
		_ = b.sendMessageWithReply(chatID, "Invalid target.", nil, replyToID)
		return
	}
	switch target.MediaType {
	case mediaTypeSong, mediaTypeAlbum, mediaTypePlaylist, mediaTypeStation:
	default:
		_ = b.sendMessageWithReply(chatID, "animatedcover only supports song/album/playlist/station.", nil, replyToID)
		return
	}
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		_ = b.sendMessageWithReply(chatID, "animatedcover requires ffmpeg in PATH.", nil, replyToID)
		return
	}
	settings := b.getChatSettings(chatID)
	_ = b.enqueueTelegramTask(&downloadRequest{
		chatID:       chatID,
		replyToID:    replyToID,
		single:       true,
		taskType:     telegramTaskAnimatedCover,
		settings:     settings,
		transferMode: transferModeOneByOne,
		mediaType:    target.MediaType,
		mediaID:      target.ID,
		storefront:   resolveStorefront(target),
		requestID:    b.nextRequestID(),
	})
}

func (b *TelegramBot) fetchLyricsOnly(songID string, storefront string, outputFormat string) (string, string, error) {
	return b.catalogService().FetchLyricsOnly(songID, storefront, outputFormat)
}

func (b *TelegramBot) sendTextAsDocument(chatID int64, replyToID int, displayName string, ext string, content string) error {
	ext = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(ext)), ".")
	if ext == "" {
		ext = "txt"
	}
	tmp, err := os.CreateTemp("", "amdl-doc-*."+ext)
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	if _, err := tmp.WriteString(content); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	defer os.Remove(tmpPath)
	if displayName == "" {
		displayName = "apple-music." + ext
	}
	return b.sendDocumentFile(chatID, tmpPath, displayName, replyToID, nil, "")
}

func uniqueName(existing map[string]struct{}, candidate string) string {
	if _, ok := existing[candidate]; !ok {
		existing[candidate] = struct{}{}
		return candidate
	}
	ext := filepath.Ext(candidate)
	base := strings.TrimSuffix(candidate, ext)
	for i := 2; i <= 9999; i++ {
		name := fmt.Sprintf("%s-%d%s", base, i, ext)
		if _, ok := existing[name]; !ok {
			existing[name] = struct{}{}
			return name
		}
	}
	existing[candidate] = struct{}{}
	return candidate
}

func (b *TelegramBot) exportAlbumLyrics(chatID int64, replyToID int, albumID string, storefront string, transferMode string) {
	b.exportAlbumLyricsWithSettings(chatID, replyToID, albumID, storefront, transferMode, b.getChatSettings(chatID))
}

func (b *TelegramBot) exportAlbumLyricsWithSettings(chatID int64, replyToID int, albumID string, storefront string, transferMode string, settings ChatDownloadSettings) {
	if len(strings.TrimSpace(Config.MediaUserToken)) <= 50 {
		_ = b.sendMessageWithReply(chatID, "Lyrics export requires media-user-token in config.yaml.", nil, replyToID)
		return
	}
	if strings.TrimSpace(albumID) == "" {
		_ = b.sendMessageWithReply(chatID, "Album ID is empty.", nil, replyToID)
		return
	}
	storefront = resolveStorefront(&AppleURLTarget{Storefront: storefront})
	settings = normalizeChatSettings(settings)
	lyricsFormat := normalizeLyricsOutputFormat(settings.LyricsFormat)
	if lyricsFormat == "" {
		lyricsFormat = defaultTelegramLyricsFormat
	}
	status, err := newDownloadStatus(b, chatID, replyToID)
	if err != nil {
		_ = b.sendMessageWithReply(chatID, fmt.Sprintf("Failed to create status message: %v", err), nil, replyToID)
		return
	}
	defer status.Stop()
	status.Update("Loading album metadata", 0, 0)
	exported, err := b.catalogService().ExportAlbumLyrics("", albumID, storefront, lyricsFormat)
	if err != nil {
		switch {
		case errors.Is(err, sharedcatalog.ErrAlbumNotFound):
			status.UpdateSync("Album not found.", 0, 0)
		case errors.Is(err, sharedcatalog.ErrNoLyricsExported):
			status.UpdateSync("No lyrics files could be exported for this album.", 0, 0)
		default:
			var accessErr *safe.AccessError
			if errors.As(err, &accessErr) {
				status.UpdateSync(fmt.Sprintf("Album metadata is invalid: %v", err), 0, 0)
			} else {
				status.UpdateSync(fmt.Sprintf("Failed to load album: %v", err), 0, 0)
			}
		}
		return
	}
	lyricPaths := exported.Paths
	failedCount := exported.FailedCount
	if len(lyricPaths) == 0 {
		status.UpdateSync("No lyrics files could be exported for this album.", 0, 0)
		return
	}
	status.Update("Preparing output", int64(len(lyricPaths)), int64(len(lyricPaths)+failedCount))
	if transferMode == transferModeZip {
		zipPath, displayName, err := createZipFromPaths(lyricPaths)
		if err != nil {
			status.UpdateSync(fmt.Sprintf("Failed to build ZIP: %v", err), 0, 0)
			return
		}
		defer os.Remove(zipPath)
		safeAlbumName := sanitizeFileBaseName(exported.AlbumName)
		if safeAlbumName != "" {
			displayName = safeAlbumName + ".lyrics.zip"
		}
		if err := b.sendDocumentFile(chatID, zipPath, displayName, replyToID, status, ""); err == nil {
			status.Stop()
			_ = b.deleteMessage(chatID, status.messageID)
		} else if strings.Contains(strings.ToLower(err.Error()), "zip exceeds telegram limit") {
			status.UpdateSync("ZIP exceeds Telegram size limit, fallback to one-by-one.", 0, 0)
			transferMode = transferModeOneByOne
		} else {
			status.UpdateSync(fmt.Sprintf("Failed to send ZIP: %v", err), 0, 0)
			return
		}
	}
	if transferMode == transferModeOneByOne {
		for idx, lyricPath := range lyricPaths {
			status.Update("Sending lyrics files", int64(idx), int64(len(lyricPaths)))
			if err := b.sendDocumentFile(chatID, lyricPath, filepath.Base(lyricPath), replyToID, status, ""); err != nil {
				failedCount++
			}
		}
		status.Stop()
		_ = b.deleteMessage(chatID, status.messageID)
	}
	if failedCount > 0 {
		_ = b.sendMessageWithReply(chatID, fmt.Sprintf("Lyrics export completed with %d failed tracks.", failedCount), nil, replyToID)
	}
}

func (b *TelegramBot) handleLyricsOnly(chatID int64, replyToID int, target *AppleURLTarget) {
	b.handleLyricsOnlyWithSettings(chatID, replyToID, target, b.getChatSettings(chatID))
}

func (b *TelegramBot) handleLyricsOnlyWithSettings(chatID int64, replyToID int, target *AppleURLTarget, settings ChatDownloadSettings) {
	if target == nil || strings.TrimSpace(target.ID) == "" {
		_ = b.sendMessageWithReply(chatID, "Invalid song target.", nil, replyToID)
		return
	}
	if len(strings.TrimSpace(Config.MediaUserToken)) <= 50 {
		_ = b.sendMessageWithReply(chatID, "Lyrics export requires media-user-token in config.yaml.", nil, replyToID)
		return
	}
	settings = normalizeChatSettings(settings)
	outputFormat := normalizeLyricsOutputFormat(settings.LyricsFormat)
	if outputFormat == "" {
		outputFormat = defaultTelegramLyricsFormat
	}
	storefront := resolveStorefront(target)
	songID := strings.TrimSpace(target.ID)
	content, lyricType, err := b.fetchLyricsOnly(songID, storefront, outputFormat)
	if err != nil {
		_ = b.sendMessageWithReply(chatID, fmt.Sprintf("Failed to fetch lyrics: %v", err), nil, replyToID)
		return
	}
	baseName := "song-" + songID
	if songResp, err := ampapi.GetSongResp(storefront, songID, Config.Language, b.appleToken); err == nil {
		item, dataErr := firstSongData("main.telegram.handleLyricsOnly", songResp)
		if dataErr == nil {
			if title := composeArtistTitle(item.Attributes.ArtistName, item.Attributes.Name); strings.TrimSpace(title) != "" {
				baseName = title
			}
		}
	}
	displayName := sanitizeFileBaseName(baseName) + ".lyrics." + outputFormat
	if err := b.sendTextAsDocument(chatID, replyToID, displayName, outputFormat, content); err != nil {
		_ = b.sendMessageWithReply(chatID, fmt.Sprintf("Failed to send lyrics file: %v", err), nil, replyToID)
		return
	}
	if outputFormat == "lrc" {
		if lyricType == "syllable-lyrics" {
			_ = b.sendMessageWithReply(chatID, "LRC exported. Translation lines are included when Apple provides them.", nil, replyToID)
		} else {
			_ = b.sendMessageWithReply(chatID, "LRC exported from fallback source (translation may be unavailable).", nil, replyToID)
		}
		return
	}
	if lyricType == "syllable-lyrics" {
		_ = b.sendMessageWithReply(chatID, "TTML exported. Translation and transliteration are included when Apple provides them.", nil, replyToID)
	} else {
		_ = b.sendMessageWithReply(chatID, "TTML exported from fallback lyrics source (translation/transliteration may be unavailable).", nil, replyToID)
	}
}

func (b *TelegramBot) enqueueSongLyricsTask(chatID int64, replyToID int, target *AppleURLTarget) {
	if target == nil || strings.TrimSpace(target.ID) == "" || target.MediaType != mediaTypeSong {
		_ = b.sendMessageWithReply(chatID, "Invalid song target.", nil, replyToID)
		return
	}
	if len(strings.TrimSpace(Config.MediaUserToken)) <= 50 {
		_ = b.sendMessageWithReply(chatID, "Lyrics export requires media-user-token in config.yaml.", nil, replyToID)
		return
	}
	settings := b.getChatSettings(chatID)
	_ = b.enqueueTelegramTask(&downloadRequest{
		chatID:       chatID,
		replyToID:    replyToID,
		single:       true,
		taskType:     telegramTaskSongLyrics,
		settings:     settings,
		transferMode: transferModeOneByOne,
		mediaType:    target.MediaType,
		mediaID:      target.ID,
		storefront:   resolveStorefront(target),
		requestID:    b.nextRequestID(),
	})
}

func (b *TelegramBot) enqueueAlbumLyricsTask(chatID int64, replyToID int, albumID string, storefront string, transferMode string) {
	if strings.TrimSpace(albumID) == "" {
		_ = b.sendMessageWithReply(chatID, "Album ID is empty.", nil, replyToID)
		return
	}
	if len(strings.TrimSpace(Config.MediaUserToken)) <= 50 {
		_ = b.sendMessageWithReply(chatID, "Lyrics export requires media-user-token in config.yaml.", nil, replyToID)
		return
	}
	settings := b.getChatSettings(chatID)
	_ = b.enqueueTelegramTask(&downloadRequest{
		chatID:       chatID,
		replyToID:    replyToID,
		single:       false,
		taskType:     telegramTaskAlbumLyrics,
		settings:     settings,
		transferMode: transferMode,
		mediaType:    mediaTypeAlbum,
		mediaID:      albumID,
		storefront:   resolveStorefront(&AppleURLTarget{Storefront: storefront}),
		requestID:    b.nextRequestID(),
	})
}

func (b *TelegramBot) enqueueArtistAssetsTask(chatID int64, replyToID int, artistID string, storefront string, transferMode string) {
	if strings.TrimSpace(artistID) == "" {
		_ = b.sendMessageWithReply(chatID, "Artist ID is empty.", nil, replyToID)
		return
	}
	settings := b.getChatSettings(chatID)
	_ = b.enqueueTelegramTask(&downloadRequest{
		chatID:       chatID,
		replyToID:    replyToID,
		single:       false,
		taskType:     telegramTaskArtistAssets,
		settings:     settings,
		transferMode: transferMode,
		mediaType:    mediaTypeArtist,
		mediaID:      artistID,
		storefront:   resolveStorefront(&AppleURLTarget{Storefront: storefront}),
		requestID:    b.nextRequestID(),
	})
}

func normalizeArtistRelationship(relationship string) string {
	switch strings.ToLower(strings.TrimSpace(relationship)) {
	case "albums", "album":
		return "albums"
	case "music-videos", "musicvideos", "music_video", "musicvideo", "mv", "mvs", "videos", "video":
		return "music-videos"
	default:
		return ""
	}
}

func (b *TelegramBot) startArtistSelection(chatID int64, artistID string, artistName string, storefront string, replyToID int) {
	artistID = strings.TrimSpace(artistID)
	if artistID == "" {
		_ = b.sendMessageWithReply(chatID, "Artist ID is empty.", nil, replyToID)
		return
	}
	artistName = strings.TrimSpace(artistName)
	if artistName == "" {
		artistName = "artist " + artistID
	}
	message := fmt.Sprintf("Choose what to browse from %s:", artistName)
	messageID, err := b.sendMessageWithReplyReturn(chatID, message, buildArtistModeKeyboard(), replyToID)
	if err != nil {
		return
	}
	if storefront == "" {
		storefront = Config.Storefront
	}
	b.setPendingArtistMode(chatID, artistID, artistName, storefront, replyToID, messageID)
}

func (b *TelegramBot) handleArtistModeSelection(chatID int64, messageID int, relationship string) {
	pending, ok := b.getPendingArtistMode(chatID, messageID)
	if !ok {
		return
	}
	if time.Since(pending.CreatedAt) > pendingTTL {
		b.clearPendingArtistModeByMessage(chatID, messageID)
		_ = b.editMessageText(chatID, messageID, "Selection expired. Please request the artist again.", nil)
		return
	}
	normalizedRelationship := normalizeArtistRelationship(relationship)
	if normalizedRelationship == "" {
		_ = b.editMessageText(chatID, messageID, "Unknown artist view.", nil)
		return
	}
	replyToID := pending.ReplyToMessageID
	var (
		items   []apputils.SearchResultItem
		hasNext bool
		err     error
		kind    string
		text    string
	)
	switch normalizedRelationship {
	case "albums":
		items, hasNext, err = apputils.FetchArtistAlbums(pending.Storefront, pending.ArtistID, b.appleToken, b.searchLimit, 0, b.searchLanguage())
		kind = "artist_album"
		if err == nil {
			text = apputils.FormatArtistAlbums(pending.ArtistName, items)
		}
	case "music-videos":
		items, hasNext, err = apputils.FetchArtistMusicVideos(pending.Storefront, pending.ArtistID, b.appleToken, b.searchLimit, 0, b.searchLanguage())
		kind = "artist_mv"
		if err == nil {
			text = apputils.FormatArtistMusicVideos(pending.ArtistName, items)
		}
	}
	if err != nil {
		_ = b.sendMessageWithReply(chatID, fmt.Sprintf("Failed to load artist content: %v", err), nil, replyToID)
		return
	}
	if len(items) == 0 {
		_ = b.sendMessageWithReply(chatID, "No content found for this artist.", nil, replyToID)
		return
	}
	resultMessageID, err := b.sendMessageWithReplyReturn(chatID, text, buildInlineKeyboard(len(items), false, hasNext), replyToID)
	if err != nil {
		return
	}
	b.setPending(chatID, kind, pending.ArtistID, pending.Storefront, 0, items, hasNext, replyToID, resultMessageID, pending.ArtistName)
	b.clearPendingArtistModeByMessage(chatID, messageID)
}

func (b *TelegramBot) handleSearch(chatID int64, kind string, query string, replyToID int) {
	query = strings.TrimSpace(query)
	if query == "" {
		_ = b.sendMessageWithReply(chatID, "Please provide a search query.", nil, replyToID)
		return
	}
	kind = strings.ToLower(kind)
	if kind != "song" && kind != "album" && kind != "artist" {
		_ = b.sendMessageWithReply(chatID, "Search type must be song, album, or artist.", nil, replyToID)
		return
	}
	offset := 0
	items, hasNext, err := b.fetchSearchPage(kind, query, offset)
	if err != nil {
		_ = b.sendMessageWithReply(chatID, fmt.Sprintf("Search failed: %v", err), nil, replyToID)
		return
	}
	if len(items) == 0 {
		_ = b.sendMessageWithReply(chatID, "No results found.", nil, replyToID)
		return
	}
	message := apputils.FormatSearchResults(kind, query, items)
	messageID, err := b.sendMessageWithReplyReturn(chatID, message, buildInlineKeyboard(len(items), offset > 0, hasNext), replyToID)
	if err != nil {
		return
	}
	b.setPending(chatID, kind, query, Config.Storefront, offset, items, hasNext, replyToID, messageID, "")
}

func (b *TelegramBot) searchLanguage() string {
	lang := strings.TrimSpace(Config.TelegramSearchLanguage)
	if lang == "" {
		lang = strings.TrimSpace(Config.Language)
	}
	return lang
}

func (b *TelegramBot) fetchSearchPage(kind string, query string, offset int) ([]apputils.SearchResultItem, bool, error) {
	apiType := kind + "s"
	resp, err := ampapi.Search(Config.Storefront, query, apiType, b.searchLanguage(), b.appleToken, b.searchLimit, offset)
	if err != nil {
		return nil, false, err
	}
	items, hasNext := apputils.BuildSearchItems(kind, resp)
	return items, hasNext, nil
}

func (b *TelegramBot) handleSelection(chatID int64, messageID int, choice int) {
	pending, ok := b.getPending(chatID, messageID)
	if !ok {
		_ = b.sendMessage(chatID, "No active selection. Start with /search.", nil)
		return
	}
	replyToID := pending.ReplyToMessageID
	if time.Since(pending.CreatedAt) > pendingTTL {
		b.clearPendingByMessage(chatID, messageID)
		_ = b.sendMessageWithReply(chatID, "Selection expired. Please search again.", nil, replyToID)
		return
	}
	if choice < 1 || choice > len(pending.Items) {
		_ = b.sendMessageWithReply(chatID, "Selection out of range.", nil, replyToID)
		return
	}
	selected := pending.Items[choice-1]
	storefront := pending.Storefront
	if storefront == "" {
		storefront = Config.Storefront
	}
	// Selection confirmed: remove the search list message and clear pending state.
	b.clearPendingByMessage(chatID, messageID)
	_ = b.deleteMessage(chatID, messageID)
	switch pending.Kind {
	case "song":
		setSearchMeta(selected.ID, selected.Name, selected.Artist)
		b.queueDownloadSongWithStorefront(chatID, selected.ID, storefront, replyToID)
	case "album", "artist_album":
		b.queueDownloadAlbumWithStorefront(chatID, selected.ID, storefront, replyToID)
	case "artist_mv":
		b.queueDownloadMusicVideoWithStorefront(chatID, selected.ID, storefront, replyToID, false)
	case "artist":
		b.startArtistSelection(chatID, selected.ID, selected.Name, storefront, replyToID)
	}
}

func (b *TelegramBot) handleMediaTransfer(chatID int64, messageID int, mode string) {
	pending, ok := b.getPendingTransfer(chatID, messageID)
	if !ok {
		return
	}
	if time.Since(pending.CreatedAt) > pendingTTL {
		b.clearPendingTransferByMessage(chatID, messageID)
		_ = b.editMessageText(chatID, messageID, "Selection expired. Please request it again.", nil)
		return
	}
	mediaID := pending.MediaID
	mediaType := pending.MediaType
	replyToID := pending.ReplyToMessageID
	b.clearPendingTransferByMessage(chatID, messageID)
	settings := b.getChatSettings(chatID)

	if mediaType == mediaTypeArtistAsset {
		switch mode {
		case transferModeOneByOne:
			_ = b.editMessageText(chatID, messageID, "Artist assets export mode: one by one.", nil)
			b.enqueueArtistAssetsTask(chatID, replyToID, mediaID, pending.Storefront, transferModeOneByOne)
		case transferModeZip:
			_ = b.editMessageText(chatID, messageID, "Artist assets export mode: ZIP.", nil)
			b.enqueueArtistAssetsTask(chatID, replyToID, mediaID, pending.Storefront, transferModeZip)
		default:
			_ = b.editMessageText(chatID, messageID, "Unknown artist assets export mode.", nil)
		}
		return
	}

	if mediaType == mediaTypeAlbumLyrics {
		switch mode {
		case transferModeOneByOne:
			_ = b.editMessageText(chatID, messageID, "Lyrics export mode: one by one.", nil)
			b.enqueueAlbumLyricsTask(chatID, replyToID, mediaID, pending.Storefront, transferModeOneByOne)
		case transferModeZip:
			_ = b.editMessageText(chatID, messageID, "Lyrics export mode: ZIP.", nil)
			b.enqueueAlbumLyricsTask(chatID, replyToID, mediaID, pending.Storefront, transferModeZip)
		default:
			_ = b.editMessageText(chatID, messageID, "Unknown lyrics export mode.", nil)
		}
		return
	}

	switch mode {
	case transferModeOneByOne:
		if pending.ForceRefresh {
			_ = b.editMessageText(chatID, messageID, "Transfer mode: one by one (refresh).", nil)
		} else {
			_ = b.editMessageText(chatID, messageID, "Transfer mode: one by one.", nil)
		}
		if mediaType == mediaTypeSong {
			b.enqueueSongDownload(chatID, mediaID, pending.Storefront, replyToID, transferModeOneByOne, pending.ForceRefresh)
			return
		}
		b.enqueueCollectionDownload(chatID, mediaType, mediaID, pending.Storefront, replyToID, transferModeOneByOne, pending.ForceRefresh)
	case transferModeZip:
		if mediaType == mediaTypeSong {
			if !pending.ForceRefresh && b.trySendCachedBundleZip(chatID, mediaType, mediaID, replyToID, settings) {
				_ = b.editMessageText(chatID, messageID, "Transfer mode: ZIP (cached).", nil)
				return
			}
			if pending.ForceRefresh {
				_ = b.editMessageText(chatID, messageID, "Transfer mode: ZIP (refresh).", nil)
			} else {
				_ = b.editMessageText(chatID, messageID, "Transfer mode: ZIP.", nil)
			}
			b.enqueueSongDownload(chatID, mediaID, pending.Storefront, replyToID, transferModeZip, pending.ForceRefresh)
			return
		}
		if !pending.ForceRefresh && b.trySendCachedBundleZip(chatID, mediaType, mediaID, replyToID, settings) {
			_ = b.editMessageText(chatID, messageID, "Transfer mode: ZIP (cached).", nil)
			return
		}
		if pending.ForceRefresh {
			_ = b.editMessageText(chatID, messageID, "Transfer mode: ZIP (refresh).", nil)
		} else {
			_ = b.editMessageText(chatID, messageID, "Transfer mode: ZIP.", nil)
		}
		b.enqueueCollectionDownload(chatID, mediaType, mediaID, pending.Storefront, replyToID, transferModeZip, pending.ForceRefresh)
	default:
		_ = b.editMessageText(chatID, messageID, "Unknown transfer mode.", nil)
	}
}

func (b *TelegramBot) handlePage(chatID int64, messageID int, delta int) {
	pending, ok := b.getPending(chatID, messageID)
	if !ok {
		return
	}
	if pending.Query == "" {
		return
	}
	newOffset := pending.Offset + delta*b.searchLimit
	if newOffset < 0 {
		return
	}
	var (
		items   []apputils.SearchResultItem
		hasNext bool
		err     error
		message string
	)
	switch pending.Kind {
	case "song", "album", "artist":
		items, hasNext, err = b.fetchSearchPage(pending.Kind, pending.Query, newOffset)
		if err != nil {
			_ = b.editMessageText(chatID, messageID, fmt.Sprintf("Search failed: %v", err), nil)
			return
		}
		if len(items) == 0 {
			return
		}
		message = apputils.FormatSearchResults(pending.Kind, pending.Query, items)
	case "artist_album":
		storefront := pending.Storefront
		if storefront == "" {
			storefront = Config.Storefront
		}
		items, hasNext, err = apputils.FetchArtistAlbums(storefront, pending.Query, b.appleToken, b.searchLimit, newOffset, b.searchLanguage())
		if err != nil {
			_ = b.editMessageText(chatID, messageID, fmt.Sprintf("Failed to load artist albums: %v", err), nil)
			return
		}
		if len(items) == 0 {
			return
		}
		message = apputils.FormatArtistAlbums(pending.Title, items)
	case "artist_mv":
		storefront := pending.Storefront
		if storefront == "" {
			storefront = Config.Storefront
		}
		items, hasNext, err = apputils.FetchArtistMusicVideos(storefront, pending.Query, b.appleToken, b.searchLimit, newOffset, b.searchLanguage())
		if err != nil {
			_ = b.editMessageText(chatID, messageID, fmt.Sprintf("Failed to load artist music videos: %v", err), nil)
			return
		}
		if len(items) == 0 {
			return
		}
		message = apputils.FormatArtistMusicVideos(pending.Title, items)
	default:
		return
	}
	_ = b.editMessageText(chatID, messageID, message, buildInlineKeyboard(len(items), newOffset > 0, hasNext))
	b.setPending(chatID, pending.Kind, pending.Query, pending.Storefront, newOffset, items, hasNext, pending.ReplyToMessageID, messageID, pending.Title)
}

func makeDownloadInflightKey(chatID int64, mediaType string, mediaID string, storefront string, transferMode string, settings ChatDownloadSettings) string {
	normalized := normalizeChatSettings(settings)
	normalizedTransfer := normalizeTransferModeForMedia(transferMode, mediaType, mediaType == mediaTypeSong || mediaType == mediaTypeMusicVideo)
	return strings.Join([]string{
		strconv.FormatInt(chatID, 10),
		strings.ToLower(strings.TrimSpace(mediaType)),
		strings.TrimSpace(mediaID),
		strings.TrimSpace(storefront),
		normalizedTransfer,
		normalized.Format,
		normalized.AACType,
		normalized.MVAudioType,
		strconv.FormatBool(normalized.SongZip),
		strconv.FormatBool(normalized.AutoLyrics),
		strconv.FormatBool(normalized.AutoCover),
		strconv.FormatBool(normalized.AutoAnimated),
		normalized.LyricsFormat,
	}, "|")
}

func (b *TelegramBot) acquireInflightDownload(key string) bool {
	if b == nil {
		return false
	}
	trimmed := strings.TrimSpace(key)
	if trimmed == "" {
		return true
	}
	b.inflightMu.Lock()
	defer b.inflightMu.Unlock()
	if _, exists := b.inflightDownloads[trimmed]; exists {
		return false
	}
	b.inflightDownloads[trimmed] = struct{}{}
	b.requestStateSave()
	return true
}

func (b *TelegramBot) releaseInflightDownload(key string) {
	if b == nil {
		return
	}
	trimmed := strings.TrimSpace(key)
	if trimmed == "" {
		return
	}
	b.inflightMu.Lock()
	delete(b.inflightDownloads, trimmed)
	b.inflightMu.Unlock()
	b.requestStateSave()
}

func (b *TelegramBot) waitTelegramSend(ctx context.Context, chatID int64) error {
	if b == nil || b.sendLimiter == nil {
		return nil
	}
	return b.sendLimiter.wait(ctx, chatID)
}

func (b *TelegramBot) applyTelegramRetryAfter(duration time.Duration) {
	if b == nil || duration <= 0 {
		return
	}
	if b.sendLimiter != nil {
		b.sendLimiter.blockFor(duration)
	}
	appRuntimeMetrics.recordTelegramRetryAfter()
}

func (b *TelegramBot) noteTelegramRateLimit(err error) {
	if err == nil {
		return
	}
	retryAfter, ok := parseTelegramRetryAfter(err)
	if !ok {
		return
	}
	if retryAfter < time.Second {
		retryAfter = time.Second
	}
	b.applyTelegramRetryAfter(retryAfter)
}

func (b *TelegramBot) queueDownloadSong(chatID int64, songID string) {
	b.queueDownloadSongWithStorefrontOptions(chatID, songID, Config.Storefront, 0, false)
}

func (b *TelegramBot) queueDownloadSongWithReply(chatID int64, songID string, replyToID int) {
	b.queueDownloadSongWithStorefrontOptions(chatID, songID, Config.Storefront, replyToID, false)
}

func (b *TelegramBot) queueDownloadSongWithStorefront(chatID int64, songID string, storefront string, replyToID int) {
	b.queueDownloadSongWithStorefrontOptions(chatID, songID, storefront, replyToID, false)
}

func (b *TelegramBot) queueDownloadSongWithStorefrontOptions(chatID int64, songID string, storefront string, replyToID int, forceRefresh bool) {
	if songID == "" {
		_ = b.sendMessage(chatID, "Song ID is empty.", nil)
		return
	}
	if storefront == "" {
		storefront = Config.Storefront
	}
	settings := b.getChatSettings(chatID)
	mode := transferModeOneByOne
	if settings.SongZip {
		mode = transferModeZip
	}
	b.enqueueSongDownload(chatID, songID, storefront, replyToID, mode, forceRefresh)
}

func (b *TelegramBot) enqueueSongDownload(chatID int64, songID string, storefront string, replyToID int, transferMode string, forceRefresh bool) {
	if songID == "" {
		_ = b.sendMessage(chatID, "Song ID is empty.", nil)
		return
	}
	settings := b.getChatSettings(chatID)
	transferMode = normalizeTransferModeForMedia(transferMode, mediaTypeSong, true)
	if !forceRefresh && transferMode == transferModeZip && b.trySendCachedBundleZip(chatID, mediaTypeSong, songID, replyToID, settings) {
		return
	}
	if !forceRefresh && transferMode == transferModeOneByOne && b.trySendCachedTrack(chatID, replyToID, songID, settings.Format) {
		b.sendCachedSongAutoExtras(chatID, replyToID, songID, storefront, settings)
		return
	}
	if storefront == "" {
		storefront = Config.Storefront
	}
	inflightKey := makeDownloadInflightKey(chatID, mediaTypeSong, songID, storefront, transferMode, settings)
	if !b.acquireInflightDownload(inflightKey) {
		_ = b.sendMessageWithReply(chatID, "Same song task is already running for this chat. Please wait.", nil, replyToID)
		return
	}
	if queued := b.enqueueDownload(chatID, replyToID, true, forceRefresh, settings, transferMode, mediaTypeSong, songID, storefront, inflightKey, func(session *DownloadSession) error {
		return ripSong(session, songID, b.appleToken, storefront, session.Config.MediaUserToken)
	}); !queued {
		b.releaseInflightDownload(inflightKey)
	}
}

func hasSongAutoExtras(settings ChatDownloadSettings) bool {
	normalized := normalizeChatSettings(settings)
	return normalized.AutoLyrics || normalized.AutoCover || normalized.AutoAnimated
}

func (b *TelegramBot) sendCachedSongAutoExtras(chatID int64, replyToID int, songID string, storefront string, settings ChatDownloadSettings) {
	normalized := normalizeChatSettings(settings)
	if !hasSongAutoExtras(normalized) {
		return
	}
	if storefront == "" {
		storefront = Config.Storefront
	}

	baseName := "song-" + songID
	if meta, err := b.fetchTrackMeta(songID); err == nil {
		if title := composeArtistTitle(meta.Performer, meta.Title); strings.TrimSpace(title) != "" {
			baseName = title
		}
	}

	if normalized.AutoLyrics {
		outputFormat := normalizeLyricsOutputFormat(normalized.LyricsFormat)
		if outputFormat == "" {
			outputFormat = defaultTelegramLyricsFormat
		}
		content, _, err := b.fetchLyricsOnly(songID, storefront, outputFormat)
		if err != nil {
			fmt.Printf("cached song extra lyrics skipped (%s): %v\n", songID, err)
		} else if strings.TrimSpace(content) != "" {
			displayName := sanitizeFileBaseName(baseName) + ".lyrics." + outputFormat
			if err := b.sendTextAsDocument(chatID, replyToID, displayName, outputFormat, content); err != nil {
				fmt.Printf("send cached lyrics error (%s): %v\n", songID, err)
			}
		}
	}

	needArtwork := normalized.AutoCover || normalized.AutoAnimated
	if !needArtwork {
		return
	}
	info, err := b.fetchArtwork(&AppleURLTarget{
		MediaType:  mediaTypeSong,
		ID:         songID,
		Storefront: storefront,
	})
	if err != nil {
		fmt.Printf("cached song extra artwork skipped (%s): %v\n", songID, err)
		return
	}
	displayBase := sanitizeFileBaseName(firstNonEmpty(info.DisplayName, baseName))

	if normalized.AutoCover && strings.TrimSpace(info.CoverURL) != "" {
		coverPath, tmpDir, err := renderCoverToTemp(info.CoverURL)
		if err != nil {
			fmt.Printf("cached song extra cover skipped (%s): %v\n", songID, err)
		} else {
			displayName := displayBase + "-cover" + strings.ToLower(filepath.Ext(coverPath))
			if err := b.sendDocumentFile(chatID, coverPath, displayName, replyToID, nil, ""); err != nil {
				fmt.Printf("send cached cover error (%s): %v\n", songID, err)
			}
			_ = os.RemoveAll(tmpDir)
		}
	}

	if normalized.AutoAnimated && strings.TrimSpace(info.MotionURL) != "" {
		if _, err := exec.LookPath("ffmpeg"); err != nil {
			fmt.Printf("cached song extra animated skipped (%s): ffmpeg not found\n", songID)
			return
		}
		tmp, err := os.CreateTemp("", "amdl-cached-song-animated-*.mp4")
		if err != nil {
			fmt.Printf("cached song extra animated skipped (%s): %v\n", songID, err)
			return
		}
		tmpPath := tmp.Name()
		_ = tmp.Close()
		defer os.Remove(tmpPath)
		if err := b.saveAnimatedCover(info.MotionURL, tmpPath); err != nil {
			fmt.Printf("cached song extra animated skipped (%s): %v\n", songID, err)
			return
		}
		if err := b.sendVideoFile(chatID, tmpPath, replyToID, "", nil, ""); err != nil {
			displayName := displayBase + "-animated-cover.mp4"
			if docErr := b.sendDocumentFile(chatID, tmpPath, displayName, replyToID, nil, ""); docErr != nil {
				fmt.Printf("send cached animated cover error (%s): %v; fallback: %v\n", songID, err, docErr)
			}
		}
	}
}

func (b *TelegramBot) queueDownloadAlbum(chatID int64, albumID string) {
	b.queueDownloadAlbumWithStorefrontOptions(chatID, albumID, Config.Storefront, 0, false)
}

func (b *TelegramBot) queueDownloadAlbumWithReply(chatID int64, albumID string, replyToID int) {
	b.queueDownloadAlbumWithStorefrontOptions(chatID, albumID, Config.Storefront, replyToID, false)
}

func (b *TelegramBot) queueDownloadAlbumWithStorefront(chatID int64, albumID string, storefront string, replyToID int) {
	b.queueDownloadAlbumWithStorefrontOptions(chatID, albumID, storefront, replyToID, false)
}

func (b *TelegramBot) queueDownloadAlbumWithStorefrontOptions(chatID int64, albumID string, storefront string, replyToID int, forceRefresh bool) {
	if albumID == "" {
		_ = b.sendMessage(chatID, "Album ID is empty.", nil)
		return
	}
	b.promptMediaTransfer(chatID, mediaTypeAlbum, albumID, storefront, "", replyToID, forceRefresh)
}

func (b *TelegramBot) queueDownloadPlaylist(chatID int64, playlistID string) {
	b.queueDownloadPlaylistWithStorefrontOptions(chatID, playlistID, Config.Storefront, 0, false)
}

func (b *TelegramBot) queueDownloadPlaylistWithReply(chatID int64, playlistID string, replyToID int) {
	b.queueDownloadPlaylistWithStorefrontOptions(chatID, playlistID, Config.Storefront, replyToID, false)
}

func (b *TelegramBot) queueDownloadPlaylistWithStorefront(chatID int64, playlistID string, storefront string, replyToID int) {
	b.queueDownloadPlaylistWithStorefrontOptions(chatID, playlistID, storefront, replyToID, false)
}

func (b *TelegramBot) queueDownloadPlaylistWithStorefrontOptions(chatID int64, playlistID string, storefront string, replyToID int, forceRefresh bool) {
	if playlistID == "" {
		_ = b.sendMessage(chatID, "Playlist ID is empty.", nil)
		return
	}
	b.promptMediaTransfer(chatID, mediaTypePlaylist, playlistID, storefront, "", replyToID, forceRefresh)
}

func (b *TelegramBot) queueDownloadStation(chatID int64, stationID string) {
	b.queueDownloadStationWithStorefront(chatID, stationID, Config.Storefront, 0, false)
}

func (b *TelegramBot) queueDownloadStationWithReply(chatID int64, stationID string, replyToID int) {
	b.queueDownloadStationWithStorefront(chatID, stationID, Config.Storefront, replyToID, false)
}

func (b *TelegramBot) queueDownloadStationWithStorefront(chatID int64, stationID string, storefront string, replyToID int, forceRefresh bool) {
	if stationID == "" {
		_ = b.sendMessage(chatID, "Station ID is empty.", nil)
		return
	}
	if len(strings.TrimSpace(Config.MediaUserToken)) <= 50 {
		_ = b.sendMessageWithReply(chatID, "Station download requires media-user-token in config.yaml.", nil, replyToID)
		return
	}
	b.promptMediaTransfer(chatID, mediaTypeStation, stationID, storefront, "", replyToID, forceRefresh)
}

func (b *TelegramBot) queueDownloadMusicVideo(chatID int64, mvID string) {
	b.queueDownloadMusicVideoWithStorefront(chatID, mvID, Config.Storefront, 0, false)
}

func (b *TelegramBot) queueDownloadMusicVideoWithReply(chatID int64, mvID string, replyToID int) {
	b.queueDownloadMusicVideoWithStorefront(chatID, mvID, Config.Storefront, replyToID, false)
}

func (b *TelegramBot) queueDownloadMusicVideoWithStorefront(chatID int64, mvID string, storefront string, replyToID int, forceRefresh bool) {
	if mvID == "" {
		_ = b.sendMessage(chatID, "Music Video ID is empty.", nil)
		return
	}
	if len(strings.TrimSpace(Config.MediaUserToken)) <= 50 {
		_ = b.sendMessageWithReply(chatID, "MV download requires media-user-token in config.yaml.", nil, replyToID)
		return
	}
	if _, err := exec.LookPath("mp4decrypt"); err != nil {
		_ = b.sendMessageWithReply(chatID, "MV download requires mp4decrypt in PATH.", nil, replyToID)
		return
	}
	settings := b.getChatSettings(chatID)
	if !forceRefresh && b.trySendCachedMusicVideo(chatID, replyToID, mvID, settings) {
		return
	}
	if storefront == "" {
		storefront = Config.Storefront
	}
	saveDir := strings.TrimSpace(Config.AlacSaveFolder)
	if saveDir == "" {
		saveDir = "AM-DL downloads"
	}
	inflightKey := makeDownloadInflightKey(chatID, mediaTypeMusicVideo, mvID, storefront, transferModeOneByOne, settings)
	if !b.acquireInflightDownload(inflightKey) {
		_ = b.sendMessageWithReply(chatID, "Same MV task is already running for this chat. Please wait.", nil, replyToID)
		return
	}
	if queued := b.enqueueDownload(chatID, replyToID, true, forceRefresh, settings, transferModeOneByOne, mediaTypeMusicVideo, mvID, storefront, inflightKey, func(session *DownloadSession) error {
		return mvDownloader(session, mvID, saveDir, b.appleToken, storefront, session.Config.MediaUserToken, nil)
	}); !queued {
		b.releaseInflightDownload(inflightKey)
	}
}

func (b *TelegramBot) promptMediaTransfer(chatID int64, mediaType string, mediaID string, storefront string, mediaName string, replyToID int, forceRefresh bool) {
	if mediaID == "" {
		_ = b.sendMessage(chatID, "Media ID is empty.", nil)
		return
	}
	if storefront == "" {
		storefront = Config.Storefront
	}
	message := "Choose transfer method:"
	if mediaType == mediaTypeAlbumLyrics {
		message = "Choose lyrics export method:"
	} else if mediaType == mediaTypeArtistAsset {
		message = "Choose artist assets export method:"
	}
	messageID, err := b.sendMessageWithReplyReturn(chatID, message, buildTransferKeyboard(), replyToID)
	if err != nil {
		return
	}
	b.setPendingTransfer(chatID, mediaType, mediaID, mediaName, storefront, replyToID, messageID, forceRefresh)
}

func (b *TelegramBot) enqueueCollectionDownload(chatID int64, mediaType string, mediaID string, storefront string, replyToID int, transferMode string, forceRefresh bool) {
	if mediaID == "" {
		_ = b.sendMessage(chatID, "Media ID is empty.", nil)
		return
	}
	settings := b.getChatSettings(chatID)
	if storefront == "" {
		storefront = Config.Storefront
	}
	transferMode = normalizeTransferModeForMedia(transferMode, mediaType, false)
	inflightKey := makeDownloadInflightKey(chatID, mediaType, mediaID, storefront, transferMode, settings)
	if !b.acquireInflightDownload(inflightKey) {
		_ = b.sendMessageWithReply(chatID, "Same download task is already running for this chat. Please wait.", nil, replyToID)
		return
	}
	enqueueWithRollback := func(fn func(session *DownloadSession) error) {
		if queued := b.enqueueDownload(chatID, replyToID, false, forceRefresh, settings, transferMode, mediaType, mediaID, storefront, inflightKey, fn); !queued {
			b.releaseInflightDownload(inflightKey)
		}
	}
	switch mediaType {
	case mediaTypeAlbum:
		enqueueWithRollback(func(session *DownloadSession) error {
			return ripAlbum(session, mediaID, b.appleToken, storefront, session.Config.MediaUserToken, "")
		})
	case mediaTypePlaylist:
		enqueueWithRollback(func(session *DownloadSession) error {
			return ripPlaylist(session, mediaID, b.appleToken, storefront, session.Config.MediaUserToken)
		})
	case mediaTypeStation:
		enqueueWithRollback(func(session *DownloadSession) error {
			return ripStation(session, mediaID, b.appleToken, storefront, session.Config.MediaUserToken)
		})
	default:
		b.releaseInflightDownload(inflightKey)
		_ = b.sendMessageWithReply(chatID, "Unsupported collection type for transfer.", nil, replyToID)
	}
}

func (b *TelegramBot) buildQueuedRequestRunner(req *downloadRequest) error {
	if b == nil || req == nil {
		return fmt.Errorf("invalid request")
	}
	req.taskType = normalizeTelegramTaskType(req.taskType)
	req.settings = normalizeChatSettings(req.settings)
	if strings.TrimSpace(req.storefront) == "" {
		req.storefront = Config.Storefront
	}
	if strings.TrimSpace(req.mediaType) == "" || strings.TrimSpace(req.mediaID) == "" {
		return fmt.Errorf("invalid request: missing media type/id")
	}
	if req.run != nil {
		return nil
	}
	switch req.taskType {
	case telegramTaskDownload:
		req.transferMode = normalizeTransferModeForMedia(req.transferMode, req.mediaType, req.single)
		if req.fn == nil {
			fn, err := b.buildDownloadWorkerFn(req.mediaType, req.mediaID, req.storefront)
			if err != nil {
				return err
			}
			req.fn = fn
		}
		req.run = func(bot *TelegramBot) error {
			bot.runDownload(req.chatID, req.fn, req.single, req.forceRefresh, req.replyToID, req.settings, req.transferMode, req.mediaType, req.mediaID)
			return nil
		}
	case telegramTaskCover:
		req.single = true
		req.transferMode = transferModeOneByOne
		target := &AppleURLTarget{MediaType: req.mediaType, ID: req.mediaID, Storefront: req.storefront}
		req.run = func(bot *TelegramBot) error {
			bot.handleCoverOnly(req.chatID, req.replyToID, target, false)
			return nil
		}
	case telegramTaskAnimatedCover:
		req.single = true
		req.transferMode = transferModeOneByOne
		target := &AppleURLTarget{MediaType: req.mediaType, ID: req.mediaID, Storefront: req.storefront}
		req.run = func(bot *TelegramBot) error {
			bot.handleAnimatedCoverOnly(req.chatID, req.replyToID, target)
			return nil
		}
	case telegramTaskSongLyrics:
		req.single = true
		req.transferMode = transferModeOneByOne
		target := &AppleURLTarget{MediaType: req.mediaType, ID: req.mediaID, Storefront: req.storefront}
		settings := req.settings
		req.run = func(bot *TelegramBot) error {
			bot.handleLyricsOnlyWithSettings(req.chatID, req.replyToID, target, settings)
			return nil
		}
	case telegramTaskAlbumLyrics:
		req.single = false
		if req.transferMode != transferModeZip {
			req.transferMode = transferModeOneByOne
		}
		settings := req.settings
		req.run = func(bot *TelegramBot) error {
			bot.exportAlbumLyricsWithSettings(req.chatID, req.replyToID, req.mediaID, req.storefront, req.transferMode, settings)
			return nil
		}
	case telegramTaskArtistAssets:
		req.single = false
		if req.transferMode != transferModeZip {
			req.transferMode = transferModeOneByOne
		}
		req.run = func(bot *TelegramBot) error {
			bot.exportArtistAssets(req.chatID, req.replyToID, req.mediaID, req.storefront, req.transferMode)
			return nil
		}
	default:
		return fmt.Errorf("unsupported task type: %s", req.taskType)
	}
	return nil
}

func (b *TelegramBot) enqueueTaskRequest(req *downloadRequest, blockedMessage string, queueFullMessage string) bool {
	if req == nil {
		return false
	}
	if err := b.buildQueuedRequestRunner(req); err != nil {
		if req.chatID != 0 {
			_ = b.sendMessageWithReply(req.chatID, fmt.Sprintf("Failed: %v", err), nil, req.replyToID)
		}
		return false
	}
	if b.resourceGuard != nil {
		if ok, reason := b.resourceGuard.allow(); !ok {
			msg := strings.TrimSpace(blockedMessage)
			if msg == "" {
				msg = "Resource pressure detected. New tasks are temporarily blocked."
			}
			if strings.TrimSpace(reason) != "" {
				msg = msg + " " + reason
			}
			_ = b.sendMessageWithReply(req.chatID, msg, nil, req.replyToID)
			return false
		}
	}
	b.queueMu.Lock()
	queueLen := len(b.downloadQueue)
	queueCap := cap(b.downloadQueue)
	activeWorkers := b.activeWorkers
	workerLimit := b.workerLimit
	queueFull := queueLen >= queueCap
	b.queueMu.Unlock()

	if queueFull {
		msg := strings.TrimSpace(queueFullMessage)
		if msg == "" {
			msg = "Task queue is full. Please try again later."
		}
		_ = b.sendMessageWithReply(req.chatID, msg, nil, req.replyToID)
		return false
	}
	b.trackQueuedRequest(req)
	select {
	case b.downloadQueue <- req:
	default:
		b.untrackRequest(req.requestID)
		msg := strings.TrimSpace(queueFullMessage)
		if msg == "" {
			msg = "Task queue is full. Please try again later."
		}
		_ = b.sendMessageWithReply(req.chatID, msg, nil, req.replyToID)
		return false
	}
	if queueLen > 0 || activeWorkers >= workerLimit {
		_ = b.sendMessageWithReply(req.chatID, fmt.Sprintf("Queued. Waiting: %d, running: %d/%d", queueLen+1, activeWorkers, workerLimit), nil, req.replyToID)
	}
	return true
}

func (b *TelegramBot) enqueueDownload(chatID int64, replyToID int, single bool, forceRefresh bool, settings ChatDownloadSettings, transferMode string, mediaType string, mediaID string, storefront string, inflightKey string, fn func(session *DownloadSession) error) bool {
	req := &downloadRequest{
		chatID:       chatID,
		replyToID:    replyToID,
		single:       single,
		forceRefresh: forceRefresh,
		taskType:     telegramTaskDownload,
		settings:     settings,
		transferMode: transferMode,
		mediaType:    mediaType,
		mediaID:      mediaID,
		storefront:   storefront,
		inflightKey:  inflightKey,
		requestID:    b.nextRequestID(),
		fn:           fn,
	}
	return b.enqueueTaskRequest(req, "Resource pressure detected. New download tasks are temporarily blocked.", "Download queue is full. Please try again later.")
}

func (b *TelegramBot) enqueueTelegramTask(req *downloadRequest) bool {
	return b.enqueueTaskRequest(req, "Resource pressure detected. New tasks are temporarily blocked.", "Task queue is full. Please try again later.")
}

func (b *TelegramBot) runQueuedRequest(req *downloadRequest) {
	if req == nil {
		return
	}
	if err := b.buildQueuedRequestRunner(req); err != nil {
		if req.chatID != 0 {
			_ = b.sendMessageWithReply(req.chatID, fmt.Sprintf("Failed: %v", err), nil, req.replyToID)
		}
		return
	}
	if req.run == nil {
		if req.chatID != 0 {
			_ = b.sendMessageWithReply(req.chatID, "Failed: task runner is unavailable", nil, req.replyToID)
		}
		return
	}
	if err := req.run(b); err != nil && req.chatID != 0 {
		_ = b.sendMessageWithReply(req.chatID, fmt.Sprintf("Failed: %v", err), nil, req.replyToID)
	}
}

func (b *TelegramBot) trySendCachedTrack(chatID int64, replyToID int, trackID string, format string) bool {
	entry, ok := b.getCachedAudio(trackID, b.maxFileBytes, format)
	if !ok {
		return false
	}
	if err := b.sendAudioByFileID(chatID, entry, replyToID, trackID); err != nil {
		b.deleteCachedAudio(trackID, entry.Format, entry.Compressed)
		return false
	}
	return true
}

func (b *TelegramBot) trySendCachedBundleZip(chatID int64, mediaType string, mediaID string, replyToID int, settings ChatDownloadSettings) bool {
	if mediaID == "" || mediaType == "" {
		return false
	}
	key := b.bundleZipCacheKey(mediaType, mediaID, settings)
	entry, ok := b.getCachedDocument(key)
	if !ok {
		return false
	}
	if err := b.sendDocumentByFileID(chatID, entry, replyToID); err != nil {
		b.deleteCachedDocument(key)
		return false
	}
	return true
}

func (b *TelegramBot) trySendCachedMusicVideo(chatID int64, replyToID int, mvID string, settings ChatDownloadSettings) bool {
	if mvID == "" {
		return false
	}
	videoKey := b.mvCacheKey(mvID, settings, "video")
	if entry, ok := b.getCachedVideo(videoKey); ok {
		if err := b.sendVideoByFileID(chatID, entry, replyToID); err == nil {
			return true
		}
		b.deleteCachedVideo(videoKey)
	}
	documentKey := b.mvCacheKey(mvID, settings, "document")
	if entry, ok := b.getCachedDocument(documentKey); ok {
		if err := b.sendDocumentByFileID(chatID, entry, replyToID); err == nil {
			return true
		}
		b.deleteCachedDocument(documentKey)
	}
	return false
}

func (b *TelegramBot) runDownload(chatID int64, fn func(session *DownloadSession) error, single bool, forceRefresh bool, replyToID int, settings ChatDownloadSettings, transferMode string, mediaType string, mediaID string) {
	var status *DownloadStatus
	defer func() {
		if rec := recover(); rec != nil {
			scope := fmt.Sprintf("telegram runDownload chat=%d media=%s:%s", chatID, mediaType, mediaID)
			_ = logRecoveredPanic(scope, rec)
			if status != nil {
				status.UpdateSync("任务异常已记录，已自动跳过当前任务。", 0, 0)
				status.Stop()
				return
			}
			_ = b.sendMessageWithReply(chatID, "任务异常已记录，已自动跳过当前任务。", nil, replyToID)
		}
	}()

	settings = normalizeChatSettings(settings)
	transferMode = normalizeTransferModeForMedia(transferMode, mediaType, single)
	format := settings.Format

	var err error
	status, err = newDownloadStatus(b, chatID, replyToID)
	if err != nil {
		_ = b.sendMessageWithReply(chatID, fmt.Sprintf("Failed to create status message: %v", err), nil, replyToID)
		return
	}
	defer status.Stop()

	progress := func(phase string, done, total int64) {
		status.Update(phase, done, total)
	}
	noFilesErr := errors.New("no files were downloaded")
	paths := []string{}
	primaryCount := 0
	var session *DownloadSession
	coreErr := func() error {
		// Keep core serialized to avoid heavy external tool contention.
		b.downloadCoreMu.Lock()
		defer b.downloadCoreMu.Unlock()

		session = newDownloadSession(Config)
		session.resetState()
		session.DlSelect = false
		session.ForceRedownload = forceRefresh
		if single {
			session.DlSong = true
		}
		session.Config.AacType = settings.AACType
		session.Config.MVAudioType = settings.MVAudioType
		applyTelegramAudioEmbeddingPolicy(session, settings, mediaType)

		switch format {
		case telegramFormatAtmos:
			session.DlAtmos = true
		case telegramFormatAac:
			session.DlAac = true
		}

		session.Config.ConvertAfterDownload = false
		if format == telegramFormatFlac {
			session.Config.ConvertAfterDownload = true
			session.Config.ConvertFormat = telegramFormatFlac
			session.Config.ConvertKeepOriginal = false
			session.Config.ConvertSkipLossyToLossless = false
			if _, err := exec.LookPath(session.Config.FFmpegPath); err != nil {
				return fmt.Errorf("ffmpeg not found at '%s'", session.Config.FFmpegPath)
			}
		} else {
			session.Config.ConvertFormat = ""
		}

		session.ActiveProgress = progress

		status.Update("Downloading", 0, 0)
		if err := fn(session); err != nil {
			return err
		}

		paths = append([]string{}, session.LastDownloadedPaths...)
		primaryCount = len(paths)
		if len(paths) == 0 {
			return noFilesErr
		}
		if mediaType == mediaTypeSong || mediaType == mediaTypeAlbum {
			paths = b.augmentDownloadedPaths(paths, settings)
		}
		return nil
	}()
	if coreErr != nil {
		if errors.Is(coreErr, noFilesErr) {
			status.UpdateSync("No files were downloaded.", 0, 0)
			return
		}
		status.UpdateSync(fmt.Sprintf("Failed: %v", coreErr), 0, 0)
		return
	}
	if forceRefresh && mediaType != "" && mediaID != "" {
		// Refresh should only drop stale Telegram file_id caches after fresh output exists.
		b.purgeTargetCaches(&AppleURLTarget{MediaType: mediaType, ID: mediaID})
	}
	if b.cleanupTracker != nil {
		b.cleanupTracker.RecordPaths(paths)
	}

	if transferMode == transferModeZip {
		if status != nil {
			status.Update("Zipping", 0, 0)
		}
		zipPath, displayName, err := createZipFromPaths(paths)
		if err != nil {
			status.UpdateSync(fmt.Sprintf("Failed to create ZIP: %v", err), 0, 0)
			return
		}
		defer os.Remove(zipPath)
		cacheKey := ""
		if mediaID != "" && mediaType != "" {
			cacheKey = b.bundleZipCacheKey(mediaType, mediaID, settings)
		}
		if err := b.sendDocumentFile(chatID, zipPath, displayName, replyToID, status, cacheKey); err != nil {
			fmt.Println("send ZIP error:", sanitizeTelegramError(err, b.token))
			if strings.Contains(strings.ToLower(err.Error()), "zip exceeds telegram limit") {
				status.UpdateSync("ZIP exceeds Telegram limit, fallback to one-by-one transfer.", 0, 0)
			} else {
				status.UpdateSync(fmt.Sprintf("Failed to send ZIP: %v", err), 0, 0)
				return
			}
		} else {
			status.Stop()
			_ = b.deleteMessage(chatID, status.messageID)
			return
		}
	}
	sentAny := false
	for idx, path := range paths {
		if idx >= primaryCount && !sentAny {
			status.UpdateSync("Primary media upload failed. Skip extra files (lyrics/cover/animated).", 0, 0)
			break
		}
		if err := b.sendDownloadedPathWithRetry(session, chatID, path, replyToID, status, settings); err != nil {
			fmt.Printf("send file error (%s): %s\n", path, sanitizeTelegramError(err, b.token))
			status.Update(fmt.Sprintf("Failed to send %s: %v", filepath.Base(path), err), 0, 0)
			continue
		}
		sentAny = true
	}
	if sentAny {
		status.Stop()
		_ = b.deleteMessage(chatID, status.messageID)
	}
}

type downloadFileEntry struct {
	path    string
	size    int64
	modTime time.Time
	owner   string
	mode    string
}

func (b *TelegramBot) cleanupDownloadsIfNeeded() {
	if b.cleanupTracker == nil {
		return
	}
	b.cleanupTracker.cleanupOnce(false)
}

func telegramCleanupRoots() []sharedstorage.CleanupRoot {
	return sharedstorage.TelegramCleanupRoots(&Config)
}

func telegramCleanupRootPaths() []string {
	return sharedstorage.CleanupRootPaths(telegramCleanupRoots())
}

func scanDownloadFolder(root string, cacheFile string) (int64, []downloadFileEntry, error) {
	var totalSize int64
	entries := []downloadFileEntry{}
	cachePath := ""
	if cacheFile != "" {
		cachePath = filepath.Clean(cacheFile)
	}
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		if cachePath != "" && filepath.Clean(path) == cachePath {
			return nil
		}
		size := info.Size()
		totalSize += size
		entries = append(entries, downloadFileEntry{
			path:    path,
			size:    size,
			modTime: info.ModTime(),
		})
		return nil
	})
	if err != nil {
		return totalSize, entries, err
	}
	return totalSize, entries, nil
}

func createZipFromPaths(paths []string) (string, string, error) {
	if len(paths) == 0 {
		return "", "", fmt.Errorf("no files to zip")
	}
	displayName := zipDisplayName(paths)
	tmpDir := chooseZipTempDir(paths)
	tmp, err := os.CreateTemp(tmpDir, "amdl-*.zip")
	if err != nil && tmpDir != "" {
		tmp, err = os.CreateTemp("", "amdl-*.zip")
	}
	if err != nil {
		return "", "", err
	}
	tmpPath := tmp.Name()
	zipWriter := zip.NewWriter(tmp)
	fail := func(err error) (string, string, error) {
		_ = zipWriter.Close()
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return "", "", err
	}
	rootDir := commonZipRoot(paths)
	added := 0
	for _, path := range paths {
		info, err := os.Stat(path)
		if err != nil {
			return fail(err)
		}
		if info.IsDir() {
			continue
		}
		header, err := zip.FileInfoHeader(info)
		if err != nil {
			return fail(err)
		}
		relName := filepath.Base(path)
		if rootDir != "" {
			if rel, err := filepath.Rel(rootDir, path); err == nil && rel != "." && !strings.HasPrefix(rel, "..") {
				relName = rel
			}
		}
		header.Name = filepath.ToSlash(relName)
		header.Method = zip.Deflate
		writer, err := zipWriter.CreateHeader(header)
		if err != nil {
			return fail(err)
		}
		file, err := os.Open(path)
		if err != nil {
			return fail(err)
		}
		_, err = io.Copy(writer, file)
		file.Close()
		if err != nil {
			return fail(err)
		}
		added++
	}
	if err := zipWriter.Close(); err != nil {
		return fail(err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return "", "", err
	}
	if added == 0 {
		_ = os.Remove(tmpPath)
		return "", "", fmt.Errorf("no files to zip")
	}
	return tmpPath, displayName, nil
}

func chooseZipTempDir(paths []string) string {
	candidates := []string{}
	if envDir := strings.TrimSpace(os.Getenv("AMDL_TMPDIR")); envDir != "" {
		candidates = append(candidates, envDir)
	}
	if root := commonZipRoot(paths); root != "" {
		candidates = append(candidates, root)
	}
	candidates = append(candidates,
		strings.TrimSpace(Config.TelegramDownloadFolder),
		strings.TrimSpace(Config.AlacSaveFolder),
		strings.TrimSpace(Config.AtmosSaveFolder),
		strings.TrimSpace(Config.AacSaveFolder),
	)
	seen := make(map[string]struct{}, len(candidates))
	for _, dir := range candidates {
		if dir == "" {
			continue
		}
		clean := filepath.Clean(dir)
		if clean == "." || clean == string(filepath.Separator) {
			continue
		}
		if _, ok := seen[clean]; ok {
			continue
		}
		seen[clean] = struct{}{}
		info, err := os.Stat(clean)
		if err == nil && info.IsDir() {
			return clean
		}
		if os.IsNotExist(err) {
			if mkErr := os.MkdirAll(clean, 0755); mkErr == nil {
				return clean
			}
		}
	}
	return ""
}

func zipDisplayName(paths []string) string {
	root := commonZipRoot(paths)
	if root == "" {
		return "album.zip"
	}
	base := filepath.Base(root)
	if base == "" || base == "." || base == string(filepath.Separator) {
		return "album.zip"
	}
	return base + ".zip"
}

func commonZipRoot(paths []string) string {
	if len(paths) == 0 {
		return ""
	}
	root := filepath.Dir(paths[0])
	for _, path := range paths[1:] {
		dir := filepath.Dir(path)
		for !isParentDir(root, dir) {
			parent := filepath.Dir(root)
			if parent == root {
				return root
			}
			root = parent
		}
	}
	return root
}

func isParentDir(parent, child string) bool {
	if parent == "" || child == "" {
		return false
	}
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	return !strings.HasPrefix(rel, "..")
}

func fileExistsRegular(path string) bool {
	if strings.TrimSpace(path) == "" {
		return false
	}
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return !info.IsDir()
}

func (b *TelegramBot) augmentDownloadedPaths(paths []string, settings ChatDownloadSettings) []string {
	if len(paths) == 0 {
		return paths
	}
	normalized := normalizeChatSettings(settings)
	if !normalized.AutoLyrics && !normalized.AutoCover && !normalized.AutoAnimated {
		return paths
	}
	result := append([]string{}, paths...)
	seen := make(map[string]struct{}, len(result))
	for _, path := range result {
		seen[path] = struct{}{}
	}
	coverDone := make(map[string]struct{})
	animatedDone := make(map[string]struct{})
	appendFile := func(path string) {
		if !fileExistsRegular(path) {
			return
		}
		if _, ok := seen[path]; ok {
			return
		}
		seen[path] = struct{}{}
		result = append(result, path)
	}
	isAudioPath := func(path string) bool {
		switch strings.ToLower(filepath.Ext(path)) {
		case ".m4a", ".flac", ".mp3", ".aac", ".wav", ".opus":
			return true
		default:
			return false
		}
	}
	for _, path := range paths {
		dir := filepath.Dir(path)
		if normalized.AutoCover {
			if _, ok := coverDone[dir]; !ok {
				coverDone[dir] = struct{}{}
				appendFile(findCoverFile(dir))
			}
		}
		if normalized.AutoAnimated {
			if _, ok := animatedDone[dir]; !ok {
				animatedDone[dir] = struct{}{}
				appendFile(filepath.Join(dir, "square_animated_artwork.mp4"))
				appendFile(filepath.Join(dir, "tall_animated_artwork.mp4"))
			}
		}
		if normalized.AutoLyrics && isAudioPath(path) {
			ext := strings.ToLower(filepath.Ext(path))
			lyricsPath := strings.TrimSuffix(path, ext) + "." + normalized.LyricsFormat
			appendFile(lyricsPath)
		}
	}
	return result
}

func isRetryableUploadError(err error) bool {
	if err == nil {
		return false
	}
	if _, ok := parseTelegramRetryAfter(err); ok {
		return true
	}
	lower := strings.ToLower(err.Error())
	retryableHints := []string{
		"context deadline exceeded",
		"client.timeout exceeded",
		"tls handshake timeout",
		"connection reset by peer",
		"use of closed network connection",
		"broken pipe",
		"read/write on closed pipe",
		"i/o timeout",
		"eof",
		"unexpected eof",
		"bad gateway",
		"temporarily unavailable",
		"too many requests",
	}
	for _, hint := range retryableHints {
		if strings.Contains(lower, hint) {
			return true
		}
	}
	return false
}

func parseTelegramRetryAfter(err error) (time.Duration, bool) {
	if err == nil {
		return 0, false
	}
	msg := err.Error()
	for _, re := range []*regexp.Regexp{retryAfterJSONRe, retryAfterTextRe} {
		matches := re.FindStringSubmatch(msg)
		if len(matches) < 2 {
			continue
		}
		sec, convErr := strconv.Atoi(matches[1])
		if convErr != nil || sec <= 0 {
			continue
		}
		return time.Duration(sec) * time.Second, true
	}
	return 0, false
}

func isPipeClosedError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.ErrClosedPipe) {
		return true
	}
	lower := strings.ToLower(err.Error())
	return strings.Contains(lower, "read/write on closed pipe")
}

func combineStreamingRequestError(reqErr error, writeErr error) error {
	if reqErr == nil {
		return writeErr
	}
	if writeErr == nil || isPipeClosedError(writeErr) {
		return reqErr
	}
	return fmt.Errorf("%v (body-writer error: %v)", reqErr, writeErr)
}

func closeHTTPIdleConnections(client *http.Client) {
	if client == nil {
		return
	}
	if tr, ok := client.Transport.(*http.Transport); ok && tr != nil {
		tr.CloseIdleConnections()
	}
}

func newUploadWatchdog(timeout time.Duration) (context.Context, func(), func(), func() bool) {
	ctx, cancel := context.WithCancel(context.Background())
	var mu sync.Mutex
	lastProgress := time.Now()
	stalled := atomic.Bool{}
	doneCh := make(chan struct{})
	var doneOnce sync.Once

	touch := func() {
		mu.Lock()
		lastProgress = time.Now()
		mu.Unlock()
	}
	stop := func() {
		doneOnce.Do(func() {
			close(doneCh)
		})
	}

	go func() {
		runWithRecovery("telegram upload watchdog", nil, func() {
			ticker := time.NewTicker(uploadWatchdogInterval)
			defer ticker.Stop()
			for {
				select {
				case <-doneCh:
					return
				case <-ctx.Done():
					return
				case <-ticker.C:
					mu.Lock()
					idle := time.Since(lastProgress)
					mu.Unlock()
					if idle > timeout {
						stalled.Store(true)
						cancel()
						return
					}
				}
			}
		})
	}()

	return ctx, touch, stop, stalled.Load
}

func copyWithUploadProgress(dst io.Writer, src io.Reader, total int64, status *DownloadStatus, phase string, onProgress func()) (int64, error) {
	buf := make([]byte, uploadProgressBufferSize)
	var written int64
	lastUpdate := time.Time{}
	for {
		nr, er := src.Read(buf)
		if nr > 0 {
			nw, ew := dst.Write(buf[:nr])
			if nw > 0 {
				written += int64(nw)
				if onProgress != nil {
					onProgress()
				}
				now := time.Now()
				if status != nil {
					if total > 0 {
						if written >= total || lastUpdate.IsZero() || now.Sub(lastUpdate) >= 800*time.Millisecond {
							status.Update(phase, written, total)
							lastUpdate = now
						}
					} else {
						if lastUpdate.IsZero() || now.Sub(lastUpdate) >= 800*time.Millisecond {
							status.Update(phase, written, 0)
							lastUpdate = now
						}
					}
				}
			}
			if ew != nil {
				return written, ew
			}
			if nw != nr {
				return written, io.ErrShortWrite
			}
		}
		if er == io.EOF {
			if status != nil {
				if total > 0 {
					status.Update(phase, total, total)
				} else {
					status.Update(phase, written, 0)
				}
			}
			return written, nil
		}
		if er != nil {
			return written, er
		}
	}
}

func (b *TelegramBot) sendWithRetry(status *DownloadStatus, label string, maxAttempts int, fn func() error) error {
	if maxAttempts <= 0 {
		maxAttempts = 1
	}
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		lastErr = fn()
		if lastErr == nil {
			return nil
		}
		retryAfter, hasRetryAfter := parseTelegramRetryAfter(lastErr)
		if hasRetryAfter {
			b.applyTelegramRetryAfter(retryAfter)
		}
		if attempt == maxAttempts || (!isRetryableUploadError(lastErr) && !hasRetryAfter) {
			b.noteTelegramRateLimit(lastErr)
			return lastErr
		}
		if status != nil {
			phase := fmt.Sprintf("Upload interrupted, retrying (%d/%d)", attempt+1, maxAttempts)
			if strings.TrimSpace(label) != "" {
				phase = fmt.Sprintf("%s interrupted, retrying (%d/%d)", label, attempt+1, maxAttempts)
			}
			if hasRetryAfter {
				phase = fmt.Sprintf("%s rate limited, retry after %ds (%d/%d)", strings.TrimSpace(label), int(retryAfter.Seconds()), attempt+1, maxAttempts)
			}
			status.Update(phase, 0, 0)
		}
		closeHTTPIdleConnections(b.client)
		closeHTTPIdleConnections(b.pollClient)
		if hasRetryAfter {
			time.Sleep(retryAfter)
		} else {
			time.Sleep(time.Duration(attempt) * time.Second)
		}
	}
	return lastErr
}

func (b *TelegramBot) sendDownloadedPathWithRetry(session *DownloadSession, chatID int64, filePath string, replyToID int, status *DownloadStatus, settings ChatDownloadSettings) error {
	var finalErr error
	ext := strings.ToLower(filepath.Ext(filePath))
	switch ext {
	case ".m4a", ".flac", ".mp3", ".aac", ".wav", ".opus":
		audioErr := b.sendWithRetry(status, "Audio upload", 2, func() error {
			return b.sendAudioFile(session, chatID, filePath, replyToID, status, settings.Format)
		})
		if audioErr == nil {
			finalErr = nil
			break
		}
		if status != nil {
			status.Update("Audio upload failed, trying document fallback", 0, 0)
		}
		docErr := b.sendWithRetry(status, "Document upload", 1, func() error {
			return b.sendDocumentFile(chatID, filePath, filepath.Base(filePath), replyToID, status, "")
		})
		if docErr == nil {
			finalErr = nil
			break
		}
		finalErr = fmt.Errorf("sendAudio failed: %v; sendDocument fallback failed: %v", audioErr, docErr)
	case ".mp4", ".m4v", ".mov":
		finalErr = b.sendWithRetry(status, "Video upload", 2, func() error {
			return b.sendMusicVideoFile(session, chatID, filePath, replyToID, status, settings)
		})
	default:
		finalErr = b.sendWithRetry(status, "Document upload", 2, func() error {
			return b.sendDocumentFile(chatID, filePath, filepath.Base(filePath), replyToID, status, "")
		})
	}
	if finalErr != nil {
		appRuntimeMetrics.recordUploadFailure()
	} else {
		appRuntimeMetrics.recordUploadSuccess()
	}
	return finalErr
}

func formatMVCaption(meta AudioMeta, sizeBytes int64) string {
	sizeMB := float64(sizeBytes) / (1024.0 * 1024.0)
	title := strings.TrimSpace(meta.Title)
	performer := strings.TrimSpace(meta.Performer)
	if title == "" && performer == "" {
		return fmt.Sprintf("#AppleMusic #mv %.2fMB\nvia @musicdlam_bot", sizeMB)
	}
	if performer != "" && title != "" {
		return fmt.Sprintf("%s - %s\n#AppleMusic #mv %.2fMB\nvia @musicdlam_bot", performer, title, sizeMB)
	}
	if title != "" {
		return fmt.Sprintf("%s\n#AppleMusic #mv %.2fMB\nvia @musicdlam_bot", title, sizeMB)
	}
	return fmt.Sprintf("%s\n#AppleMusic #mv %.2fMB\nvia @musicdlam_bot", performer, sizeMB)
}

func (b *TelegramBot) sendMusicVideoFile(session *DownloadSession, chatID int64, filePath string, replyToID int, status *DownloadStatus, settings ChatDownloadSettings) error {
	if session == nil {
		session = newDownloadSession(Config)
	}
	info, err := os.Stat(filePath)
	if err != nil {
		return err
	}
	if info.Size() > b.maxFileBytes {
		return fmt.Errorf("video exceeds Telegram limit (%dMB). Lower mv-max or use smaller source", b.maxFileBytes/1024/1024)
	}
	meta, _ := session.getDownloadedMeta(filePath)
	videoCacheKey := ""
	documentCacheKey := ""
	if meta.TrackID != "" {
		videoCacheKey = b.mvCacheKey(meta.TrackID, settings, "video")
		documentCacheKey = b.mvCacheKey(meta.TrackID, settings, "document")
	}
	if status != nil {
		status.Update("Uploading video", 0, 0)
	}
	caption := formatMVCaption(meta, info.Size())
	if err := b.sendVideoFile(chatID, filePath, replyToID, caption, status, videoCacheKey); err == nil {
		return nil
	} else {
		if videoCacheKey != "" {
			b.deleteCachedVideo(videoCacheKey)
		}
		if status != nil {
			status.Update("Video upload failed, trying document fallback", 0, 0)
		}
		if docErr := b.sendDocumentFile(chatID, filePath, filepath.Base(filePath), replyToID, status, documentCacheKey); docErr == nil {
			return nil
		} else {
			return fmt.Errorf("sendVideo failed: %v; sendDocument fallback failed: %v", err, docErr)
		}
	}
}

func (b *TelegramBot) sendAudioFile(session *DownloadSession, chatID int64, filePath string, replyToID int, status *DownloadStatus, format string) error {
	if session == nil {
		session = newDownloadSession(Config)
	}
	if err := b.waitTelegramSend(context.Background(), chatID); err != nil {
		return err
	}
	format = normalizeTelegramFormat(format)
	if format == "" {
		format = defaultTelegramFormat
	}
	ext := strings.ToLower(filepath.Ext(filePath))
	switch format {
	case telegramFormatFlac:
		if ext != ".flac" {
			return fmt.Errorf("output is not FLAC: %s", filepath.Base(filePath))
		}
	case telegramFormatAlac, telegramFormatAac, telegramFormatAtmos:
		if ext != ".m4a" && ext != ".mp4" {
			return fmt.Errorf("output is not M4A/MP4: %s", filepath.Base(filePath))
		}
	}
	sendPath := filePath
	displayName := filepath.Base(filePath)
	thumbPath := ""
	compressedPath := ""
	compressed := false
	meta, hasMeta := session.getDownloadedMeta(filePath)
	cleanup := func() {
		if thumbPath != "" {
			_ = os.Remove(thumbPath)
		}
		if compressedPath != "" {
			_ = os.Remove(compressedPath)
		}
	}
	defer cleanup()

	info, err := os.Stat(sendPath)
	if err != nil {
		return err
	}
	if info.Size() > b.maxFileBytes {
		if format != telegramFormatFlac {
			return fmt.Errorf("%s file exceeds Telegram limit (%dMB). Use /settings flac, lower quality, or raise telegram-max-file-mb.", strings.ToUpper(format), b.maxFileBytes/1024/1024)
		}
		if status != nil {
			status.Update("Compressing", 0, 0)
		}
		compressedPath, err = b.compressFlacToSize(sendPath, b.maxFileBytes)
		if err != nil {
			return err
		}
		sendPath = compressedPath
		compressed = true
		info, err = os.Stat(sendPath)
		if err != nil {
			return err
		}
		if info.Size() > b.maxFileBytes {
			return fmt.Errorf("compressed file still too large: %s", filepath.Base(sendPath))
		}
	}
	file, err := os.Open(sendPath)
	if err != nil {
		return err
	}
	defer file.Close()

	sizeBytes := info.Size()
	durationMillis := int64(0)
	if hasMeta {
		durationMillis = meta.DurationMillis
	}
	bitrateKbps := calcBitrateKbps(sizeBytes, durationMillis)
	if bitrateKbps <= 0 {
		if seconds, err := getAudioDurationSeconds(sendPath); err == nil && seconds > 0 {
			durationMillis = int64(seconds * 1000.0)
			bitrateKbps = calcBitrateKbps(sizeBytes, durationMillis)
		}
	}
	caption := formatTelegramCaption(sizeBytes, bitrateKbps, format)
	if status != nil {
		status.Update("Uploading audio", 0, sizeBytes)
	}
	coverPath := findCoverFile(filepath.Dir(filePath))
	if coverPath != "" {
		if path, err := makeTelegramThumb(coverPath); err == nil {
			thumbPath = path
		}
	}

	pr, pw := io.Pipe()
	writer := multipart.NewWriter(pw)
	contentType := writer.FormDataContentType()
	writeErrCh := make(chan error, 1)
	ctx, touchProgress, stopWatchdog, watchdogStalled := newUploadWatchdog(uploadNoProgressTimeout)
	defer stopWatchdog()

	req, err := http.NewRequestWithContext(ctx, "POST", b.apiURL("sendAudio"), pr)
	if err != nil {
		_ = pw.CloseWithError(err)
		return err
	}
	req.Header.Set("Content-Type", contentType)
	go func() {
		defer stopWatchdog()
		defer func() {
			if rec := recover(); rec != nil {
				panicErr := logRecoveredPanic("telegram sendAudio multipart writer", rec)
				_ = pw.CloseWithError(panicErr)
				writeErrCh <- panicErr
			}
		}()
		err := func() error {
			if err := writer.WriteField("chat_id", strconv.FormatInt(chatID, 10)); err != nil {
				return err
			}
			if replyToID > 0 {
				if err := writer.WriteField("reply_to_message_id", strconv.Itoa(replyToID)); err != nil {
					return err
				}
			}
			if caption != "" {
				if err := writer.WriteField("caption", caption); err != nil {
					return err
				}
			}
			if hasMeta {
				if meta.Title != "" {
					if err := writer.WriteField("title", meta.Title); err != nil {
						return err
					}
				}
				if meta.Performer != "" {
					if err := writer.WriteField("performer", meta.Performer); err != nil {
						return err
					}
				}
			}
			part, err := writer.CreateFormFile("audio", displayName)
			if err != nil {
				return err
			}
			if _, err := copyWithUploadProgress(part, file, sizeBytes, status, "Uploading audio", touchProgress); err != nil {
				return err
			}
			if thumbPath != "" {
				thumbFile, err := os.Open(thumbPath)
				if err == nil {
					defer thumbFile.Close()
					thumbPart, err := writer.CreateFormFile("thumbnail", filepath.Base(thumbPath))
					if err == nil {
						if _, err := io.Copy(thumbPart, thumbFile); err != nil {
							return err
						}
					}
				}
			}
			return writer.Close()
		}()
		if err != nil {
			_ = pw.CloseWithError(err)
		} else {
			_ = pw.Close()
		}
		writeErrCh <- err
	}()
	resp, err := b.client.Do(req)
	if err != nil {
		_ = pw.CloseWithError(err)
		writeErr := <-writeErrCh
		if watchdogStalled() {
			return fmt.Errorf("audio upload stalled: no progress for %s", uploadNoProgressTimeout)
		}
		return combineStreamingRequestError(err, writeErr)
	}
	defer resp.Body.Close()
	writeErr := <-writeErrCh
	if writeErr != nil {
		return writeErr
	}
	responseBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		msg := strings.TrimSpace(string(responseBody))
		if msg == "" {
			msg = resp.Status
		}
		err = fmt.Errorf("telegram sendAudio failed: %s", msg)
		b.noteTelegramRateLimit(err)
		return err
	}
	apiResp := sendAudioResponse{}
	if err := json.Unmarshal(responseBody, &apiResp); err != nil {
		return err
	}
	if !apiResp.OK {
		err = fmt.Errorf("telegram sendAudio error: %s", apiResp.Description)
		b.noteTelegramRateLimit(err)
		return err
	}
	if hasMeta && meta.TrackID != "" && apiResp.Result.Audio.FileID != "" {
		b.storeCachedAudio(meta.TrackID, CachedAudio{
			FileID:         apiResp.Result.Audio.FileID,
			FileSize:       apiResp.Result.Audio.FileSize,
			Compressed:     compressed,
			Format:         format,
			SizeBytes:      sizeBytes,
			BitrateKbps:    bitrateKbps,
			DurationMillis: durationMillis,
			Title:          meta.Title,
			Performer:      meta.Performer,
		})
	}
	return nil
}

func (b *TelegramBot) sendDocumentFile(chatID int64, filePath string, displayName string, replyToID int, status *DownloadStatus, cacheKey string) error {
	if displayName == "" {
		displayName = filepath.Base(filePath)
	}
	if err := b.waitTelegramSend(context.Background(), chatID); err != nil {
		return err
	}
	info, err := os.Stat(filePath)
	if err != nil {
		return err
	}
	if info.Size() > b.maxFileBytes {
		if strings.HasSuffix(strings.ToLower(displayName), ".zip") {
			return fmt.Errorf("ZIP exceeds Telegram limit (%dMB)", b.maxFileBytes/1024/1024)
		}
		return fmt.Errorf("file exceeds Telegram limit (%dMB)", b.maxFileBytes/1024/1024)
	}
	uploadPhase := "Uploading document"
	if status != nil {
		if strings.HasSuffix(strings.ToLower(displayName), ".zip") {
			uploadPhase = "Uploading ZIP"
		}
		status.Update(uploadPhase, 0, info.Size())
	}

	pr, pw := io.Pipe()
	writer := multipart.NewWriter(pw)
	contentType := writer.FormDataContentType()
	writeErrCh := make(chan error, 1)
	ctx, touchProgress, stopWatchdog, watchdogStalled := newUploadWatchdog(uploadNoProgressTimeout)
	defer stopWatchdog()

	req, err := http.NewRequestWithContext(ctx, "POST", b.apiURL("sendDocument"), pr)
	if err != nil {
		_ = pw.CloseWithError(err)
		return err
	}
	req.Header.Set("Content-Type", contentType)
	go func() {
		defer stopWatchdog()
		defer func() {
			if rec := recover(); rec != nil {
				panicErr := logRecoveredPanic("telegram sendDocument multipart writer", rec)
				_ = pw.CloseWithError(panicErr)
				writeErrCh <- panicErr
			}
		}()
		err := func() error {
			if err := writer.WriteField("chat_id", strconv.FormatInt(chatID, 10)); err != nil {
				return err
			}
			if replyToID > 0 {
				if err := writer.WriteField("reply_to_message_id", strconv.Itoa(replyToID)); err != nil {
					return err
				}
			}
			part, err := writer.CreateFormFile("document", displayName)
			if err != nil {
				return err
			}
			file, err := os.Open(filePath)
			if err != nil {
				return err
			}
			defer file.Close()
			if _, err := copyWithUploadProgress(part, file, info.Size(), status, uploadPhase, touchProgress); err != nil {
				return err
			}
			return writer.Close()
		}()
		if err != nil {
			_ = pw.CloseWithError(err)
		} else {
			_ = pw.Close()
		}
		writeErrCh <- err
	}()
	resp, err := b.client.Do(req)
	if err != nil {
		_ = pw.CloseWithError(err)
		writeErr := <-writeErrCh
		if watchdogStalled() {
			return fmt.Errorf("document upload stalled: no progress for %s", uploadNoProgressTimeout)
		}
		return combineStreamingRequestError(err, writeErr)
	}
	defer resp.Body.Close()
	writeErr := <-writeErrCh
	if writeErr != nil {
		return writeErr
	}
	if resp.StatusCode != http.StatusOK {
		responseBody, _ := io.ReadAll(resp.Body)
		err = fmt.Errorf("telegram sendDocument failed: %s", strings.TrimSpace(string(responseBody)))
		b.noteTelegramRateLimit(err)
		return err
	}
	apiResp := sendDocumentResponse{}
	if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
		return err
	}
	if !apiResp.OK {
		err = fmt.Errorf("telegram sendDocument error: %s", apiResp.Description)
		b.noteTelegramRateLimit(err)
		return err
	}
	if cacheKey != "" && apiResp.Result.Document.FileID != "" {
		b.storeCachedDocument(cacheKey, CachedDocument{
			FileID:   apiResp.Result.Document.FileID,
			FileSize: apiResp.Result.Document.FileSize,
		})
	}
	return nil
}

func (b *TelegramBot) sendDocumentByFileID(chatID int64, entry CachedDocument, replyToID int) error {
	if entry.FileID == "" {
		return fmt.Errorf("document file_id is empty")
	}
	if err := b.waitTelegramSend(context.Background(), chatID); err != nil {
		return err
	}
	payload := map[string]any{
		"chat_id":  chatID,
		"document": entry.FileID,
	}
	if replyToID > 0 {
		payload["reply_to_message_id"] = replyToID
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequest("POST", b.apiURL("sendDocument"), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		responseBody, _ := io.ReadAll(resp.Body)
		err = fmt.Errorf("telegram sendDocument failed: %s", strings.TrimSpace(string(responseBody)))
		b.noteTelegramRateLimit(err)
		return err
	}
	apiResp := apiResponse{}
	if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
		return err
	}
	if !apiResp.OK {
		err = fmt.Errorf("telegram sendDocument error: %s", apiResp.Description)
		b.noteTelegramRateLimit(err)
		return err
	}
	return nil
}

func (b *TelegramBot) sendVideoFile(chatID int64, filePath string, replyToID int, caption string, status *DownloadStatus, cacheKey string) error {
	if err := b.waitTelegramSend(context.Background(), chatID); err != nil {
		return err
	}
	info, err := os.Stat(filePath)
	if err != nil {
		return err
	}
	if info.Size() > b.maxFileBytes {
		return fmt.Errorf("video exceeds Telegram limit (%dMB)", b.maxFileBytes/1024/1024)
	}
	if status != nil {
		status.Update("Uploading video", 0, info.Size())
	}

	pr, pw := io.Pipe()
	writer := multipart.NewWriter(pw)
	contentType := writer.FormDataContentType()
	writeErrCh := make(chan error, 1)
	ctx, touchProgress, stopWatchdog, watchdogStalled := newUploadWatchdog(uploadNoProgressTimeout)
	defer stopWatchdog()

	req, err := http.NewRequestWithContext(ctx, "POST", b.apiURL("sendVideo"), pr)
	if err != nil {
		_ = pw.CloseWithError(err)
		return err
	}
	req.Header.Set("Content-Type", contentType)
	go func() {
		defer stopWatchdog()
		defer func() {
			if rec := recover(); rec != nil {
				panicErr := logRecoveredPanic("telegram sendVideo multipart writer", rec)
				_ = pw.CloseWithError(panicErr)
				writeErrCh <- panicErr
			}
		}()
		err := func() error {
			if err := writer.WriteField("chat_id", strconv.FormatInt(chatID, 10)); err != nil {
				return err
			}
			if replyToID > 0 {
				if err := writer.WriteField("reply_to_message_id", strconv.Itoa(replyToID)); err != nil {
					return err
				}
			}
			if caption != "" {
				if err := writer.WriteField("caption", caption); err != nil {
					return err
				}
			}
			if err := writer.WriteField("supports_streaming", "true"); err != nil {
				return err
			}
			part, err := writer.CreateFormFile("video", filepath.Base(filePath))
			if err != nil {
				return err
			}
			file, err := os.Open(filePath)
			if err != nil {
				return err
			}
			defer file.Close()
			if _, err := copyWithUploadProgress(part, file, info.Size(), status, "Uploading video", touchProgress); err != nil {
				return err
			}
			return writer.Close()
		}()
		if err != nil {
			_ = pw.CloseWithError(err)
		} else {
			_ = pw.Close()
		}
		writeErrCh <- err
	}()
	resp, err := b.client.Do(req)
	if err != nil {
		_ = pw.CloseWithError(err)
		writeErr := <-writeErrCh
		if watchdogStalled() {
			return fmt.Errorf("video upload stalled: no progress for %s", uploadNoProgressTimeout)
		}
		return combineStreamingRequestError(err, writeErr)
	}
	defer resp.Body.Close()
	writeErr := <-writeErrCh
	if writeErr != nil {
		return writeErr
	}
	responseBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		err = fmt.Errorf("telegram sendVideo failed: %s", strings.TrimSpace(string(responseBody)))
		b.noteTelegramRateLimit(err)
		return err
	}
	apiResp := sendVideoResponse{}
	if err := json.Unmarshal(responseBody, &apiResp); err != nil {
		return err
	}
	if !apiResp.OK {
		err = fmt.Errorf("telegram sendVideo error: %s", apiResp.Description)
		b.noteTelegramRateLimit(err)
		return err
	}
	if cacheKey != "" && apiResp.Result.Video.FileID != "" {
		b.storeCachedVideo(cacheKey, CachedVideo{
			FileID:   apiResp.Result.Video.FileID,
			FileSize: apiResp.Result.Video.FileSize,
		})
	}
	return nil
}

func (b *TelegramBot) sendVideoByFileID(chatID int64, entry CachedVideo, replyToID int) error {
	if entry.FileID == "" {
		return fmt.Errorf("video file_id is empty")
	}
	if err := b.waitTelegramSend(context.Background(), chatID); err != nil {
		return err
	}
	payload := map[string]any{
		"chat_id": chatID,
		"video":   entry.FileID,
	}
	if replyToID > 0 {
		payload["reply_to_message_id"] = replyToID
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequest("POST", b.apiURL("sendVideo"), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		responseBody, _ := io.ReadAll(resp.Body)
		err = fmt.Errorf("telegram sendVideo failed: %s", strings.TrimSpace(string(responseBody)))
		b.noteTelegramRateLimit(err)
		return err
	}
	apiResp := apiResponse{}
	if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
		return err
	}
	if !apiResp.OK {
		err = fmt.Errorf("telegram sendVideo error: %s", apiResp.Description)
		b.noteTelegramRateLimit(err)
		return err
	}
	return nil
}

type DownloadStatus struct {
	bot         *TelegramBot
	chatID      int64
	messageID   int
	lastPhase   string
	lastPercent int
	lastText    string
	lastUpdate  time.Time
	mu          sync.Mutex
	latestPhase string
	latestDone  int64
	latestTotal int64
	dirty       bool
	updateCh    chan struct{}
	stopCh      chan struct{}
	stopOnce    sync.Once
}

func newDownloadStatus(bot *TelegramBot, chatID int64, replyToID int) (*DownloadStatus, error) {
	messageID, err := bot.sendMessageWithReplyReturn(chatID, "Starting download...", nil, replyToID)
	if err != nil {
		return nil, err
	}
	status := &DownloadStatus{
		bot:       bot,
		chatID:    chatID,
		messageID: messageID,
		updateCh:  make(chan struct{}, 1),
		stopCh:    make(chan struct{}),
	}
	go func() {
		runWithRecovery("telegram download status loop", nil, func() {
			status.loop()
		})
	}()
	return status, nil
}

func (s *DownloadStatus) Stop() {
	if s == nil || s.bot == nil {
		return
	}
	s.stopOnce.Do(func() {
		close(s.stopCh)
	})
}

func (s *DownloadStatus) Update(phase string, done, total int64) {
	if s == nil || s.bot == nil {
		return
	}
	s.mu.Lock()
	s.setLatestLocked(phase, done, total)
	s.mu.Unlock()
	select {
	case s.updateCh <- struct{}{}:
	default:
	}
}

func (s *DownloadStatus) UpdateSync(phase string, done, total int64) {
	if s == nil || s.bot == nil {
		return
	}
	s.mu.Lock()
	s.setLatestLocked(phase, done, total)
	s.mu.Unlock()
	s.flush(true)
}

func (s *DownloadStatus) setLatestLocked(phase string, done, total int64) {
	normalizedPhase := strings.TrimSpace(phase)
	if normalizedPhase == "" {
		normalizedPhase = "Working"
	}
	s.latestPhase = normalizedPhase
	s.latestDone = done
	s.latestTotal = total
	s.dirty = true
}

func (s *DownloadStatus) loop() {
	ticker := time.NewTicker(1500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-s.updateCh:
			s.flush(false)
		case <-ticker.C:
			s.flush(false)
		case <-s.stopCh:
			return
		}
	}
}

func (s *DownloadStatus) flush(force bool) {
	if s == nil || s.bot == nil {
		return
	}
	s.mu.Lock()
	if !s.dirty && !force {
		s.mu.Unlock()
		return
	}
	phase := s.latestPhase
	done := s.latestDone
	total := s.latestTotal
	s.dirty = false
	lastPhase := s.lastPhase
	lastPercent := s.lastPercent
	lastText := s.lastText
	lastUpdate := s.lastUpdate
	s.mu.Unlock()

	percent := -1
	if total > 0 {
		percent = int(float64(done) / float64(total) * 100)
		if percent < 0 {
			percent = 0
		}
		if percent > 100 {
			percent = 100
		}
	}

	text := formatProgressText(phase, done, total, percent)
	now := time.Now()
	phaseChanged := phase != lastPhase
	percentChanged := percent != lastPercent && percent >= 0
	if !force {
		if text == lastText {
			return
		}
		if !phaseChanged && !percentChanged && now.Sub(lastUpdate) < 2*time.Second {
			return
		}
	}

	if err := s.bot.editMessageText(s.chatID, s.messageID, text, nil); err != nil {
		s.mu.Lock()
		s.dirty = true
		s.mu.Unlock()
		return
	}
	s.mu.Lock()
	s.lastPhase = phase
	s.lastPercent = percent
	s.lastText = text
	s.lastUpdate = now
	s.mu.Unlock()
}

func formatProgressText(phase string, done, total int64, percent int) string {
	if total > 0 {
		if percent < 0 {
			percent = 0
		}
		return fmt.Sprintf("%s: %s / %s (%d%%)", phase, formatBytes(done), formatBytes(total), percent)
	}
	if done > 0 {
		return fmt.Sprintf("%s: %s", phase, formatBytes(done))
	}
	return phase
}

func formatBytes(value int64) string {
	if value < 1024 {
		return fmt.Sprintf("%dB", value)
	}
	units := []string{"B", "KB", "MB", "GB", "TB"}
	size := float64(value)
	unitIndex := 0
	for size >= 1024 && unitIndex < len(units)-1 {
		size /= 1024
		unitIndex++
	}
	precision := 1
	if unitIndex >= 2 {
		precision = 2
	}
	return fmt.Sprintf("%.*f%s", precision, size, units[unitIndex])
}

func calcBitrateKbps(sizeBytes int64, durationMillis int64) float64 {
	if sizeBytes <= 0 || durationMillis <= 0 {
		return 0
	}
	seconds := float64(durationMillis) / 1000.0
	if seconds <= 0 {
		return 0
	}
	return (float64(sizeBytes) * 8.0) / (seconds * 1000.0)
}

func formatTelegramCaption(sizeBytes int64, bitrateKbps float64, format string) string {
	sizeMB := float64(sizeBytes) / (1024.0 * 1024.0)
	if sizeMB < 0 {
		sizeMB = 0
	}
	if bitrateKbps < 0 {
		bitrateKbps = 0
	}
	tag := normalizeTelegramFormat(format)
	if tag == "" {
		tag = defaultTelegramFormat
	}
	return fmt.Sprintf("#AppleMusic #%s 文件大小%.2fMB %.2fkbps\nvia @musicdlam_bot", tag, sizeMB, bitrateKbps)
}

func extractInlineTrackID(query string) string {
	trimmed := strings.TrimSpace(query)
	if trimmed == "" {
		return ""
	}
	lower := strings.ToLower(trimmed)
	if strings.HasPrefix(lower, "/songid") {
		fields := strings.Fields(trimmed)
		if len(fields) >= 2 {
			return strings.TrimSpace(fields[1])
		}
		return ""
	}
	if strings.HasPrefix(lower, "songid") {
		fields := strings.Fields(trimmed)
		if len(fields) >= 2 {
			return strings.TrimSpace(fields[1])
		}
		return ""
	}
	if strings.HasPrefix(lower, "song:") {
		return strings.TrimSpace(trimmed[5:])
	}
	return strings.TrimSpace(trimmed)
}

func findCoverFile(dir string) string {
	candidates := []string{
		"cover.jpg",
		"cover.png",
		"folder.jpg",
		"folder.png",
	}
	for _, name := range candidates {
		path := filepath.Join(dir, name)
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			return path
		}
	}
	return ""
}

func makeTelegramThumb(coverPath string) (string, error) {
	tmp, err := os.CreateTemp("", "amdl-thumb-*.jpg")
	if err != nil {
		return "", err
	}
	tmpPath := tmp.Name()
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return "", err
	}
	args := []string{
		"-y", "-i", coverPath,
		"-vf", "scale=320:320:force_original_aspect_ratio=decrease",
		"-frames:v", "1",
		"-q:v", "5",
		tmpPath,
	}
	outputResult, err := runExternalCommand(context.Background(), Config.FFmpegPath, args...)
	if err != nil {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("ffmpeg thumb failed: %v: %s", err, strings.TrimSpace(outputResult.Combined))
	}
	if info, err := os.Stat(tmpPath); err == nil && info.Size() > 200*1024 {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("thumb too large")
	}
	return tmpPath, nil
}

func (b *TelegramBot) compressFlacToSize(srcPath string, maxBytes int64) (string, error) {
	outPath, err := makeTempFlacPath()
	if err != nil {
		return "", err
	}
	coverPath := findCoverFile(filepath.Dir(srcPath))
	if err := runFlacCompress(srcPath, outPath, 0, 0, false, coverPath); err != nil {
		_ = os.Remove(outPath)
		return "", err
	}
	info, err := os.Stat(outPath)
	if err != nil {
		_ = os.Remove(outPath)
		return "", err
	}
	if info.Size() <= maxBytes {
		return outPath, nil
	}

	duration, err := getAudioDurationSeconds(srcPath)
	if err != nil {
		_ = os.Remove(outPath)
		return "", err
	}
	if duration <= 0 {
		_ = os.Remove(outPath)
		return "", fmt.Errorf("invalid duration for %s", filepath.Base(srcPath))
	}

	targetBitsPerSec := (float64(maxBytes) * 8.0 / duration) * 0.95
	sampleRate, channels := chooseResamplePlan(targetBitsPerSec)
	if err := runFlacCompress(srcPath, outPath, sampleRate, channels, true, coverPath); err != nil {
		_ = os.Remove(outPath)
		return "", err
	}

	info, err = os.Stat(outPath)
	if err != nil {
		_ = os.Remove(outPath)
		return "", err
	}
	if info.Size() > maxBytes {
		return "", fmt.Errorf("cannot compress below %dMB", maxBytes/1024/1024)
	}
	return outPath, nil
}

func runFlacCompress(srcPath, outPath string, sampleRate int, channels int, force16 bool, coverPath string) error {
	args := []string{"-y", "-i", srcPath}
	if coverPath != "" {
		args = append(args, "-i", coverPath)
		args = append(args,
			"-map", "0:a",
			"-map", "1:v",
			"-c:v", "mjpeg",
			"-disposition:v", "attached_pic",
		)
	} else {
		args = append(args, "-map", "0:a", "-map", "0:v?")
	}
	args = append(args,
		"-c:a", "flac",
		"-compression_level", "12",
	)
	if force16 {
		args = append(args, "-sample_fmt", "s16")
	}
	if sampleRate > 0 {
		args = append(args, "-ar", strconv.Itoa(sampleRate))
	}
	if channels > 0 {
		args = append(args, "-ac", strconv.Itoa(channels))
	}
	args = append(args, outPath)
	outputResult, err := runExternalCommand(context.Background(), Config.FFmpegPath, args...)
	if err != nil {
		return fmt.Errorf("ffmpeg compress failed: %v: %s", err, strings.TrimSpace(outputResult.Combined))
	}
	return nil
}

func chooseResamplePlan(targetBitsPerSec float64) (int, int) {
	channels := 2
	targetRate := targetBitsPerSec / float64(16*channels)
	if targetRate < 12000 {
		channels = 1
		targetRate = targetBitsPerSec / float64(16*channels)
	}
	return pickSampleRate(targetRate), channels
}

func pickSampleRate(target float64) int {
	rates := []int{48000, 44100, 32000, 24000, 22050, 16000, 12000, 11025, 8000}
	for _, rate := range rates {
		if float64(rate) <= target {
			return rate
		}
	}
	return rates[len(rates)-1]
}

func makeTempFlacPath() (string, error) {
	tmp, err := os.CreateTemp("", "amdl-*.flac")
	if err != nil {
		return "", err
	}
	path := tmp.Name()
	if err := tmp.Close(); err != nil {
		_ = os.Remove(path)
		return "", err
	}
	return path, nil
}

func getAudioDurationSeconds(path string) (float64, error) {
	if _, err := exec.LookPath("ffprobe"); err == nil {
		result, err := runExternalCommand(context.Background(), "ffprobe", "-v", "error", "-show_entries", "format=duration", "-of", "default=noprint_wrappers=1:nokey=1", path)
		if err == nil {
			value := strings.TrimSpace(result.Stdout)
			if value != "" {
				if secs, err := strconv.ParseFloat(value, 64); err == nil && secs > 0 {
					return secs, nil
				}
			}
		}
	}

	outResult, _ := runExternalCommand(context.Background(), Config.FFmpegPath, "-i", path)
	re := regexp.MustCompile(`Duration:\s+(\d+):(\d+):(\d+(?:\.\d+)?)`)
	match := re.FindStringSubmatch(outResult.Combined)
	if len(match) != 4 {
		return 0, fmt.Errorf("failed to read duration from ffmpeg output")
	}
	hours, _ := strconv.ParseFloat(match[1], 64)
	minutes, _ := strconv.ParseFloat(match[2], 64)
	seconds, _ := strconv.ParseFloat(match[3], 64)
	return hours*3600 + minutes*60 + seconds, nil
}

func (b *TelegramBot) sendMessage(chatID int64, text string, markup any) error {
	return b.sendMessageWithReply(chatID, text, markup, 0)
}

func (b *TelegramBot) sendMessageWithReply(chatID int64, text string, markup any, replyToID int) error {
	_, err := b.sendMessageWithReplyReturn(chatID, text, markup, replyToID)
	return err
}

func (b *TelegramBot) sendMessageWithReplyReturn(chatID int64, text string, markup any, replyToID int) (int, error) {
	payload := map[string]any{
		"chat_id": chatID,
		"text":    text,
	}
	if markup != nil {
		payload["reply_markup"] = markup
	}
	if replyToID > 0 {
		payload["reply_to_message_id"] = replyToID
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequest("POST", b.apiURL("sendMessage"), bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		err = fmt.Errorf("telegram sendMessage failed: %s", resp.Status)
		b.noteTelegramRateLimit(err)
		return 0, err
	}
	apiResp := sendMessageResponse{}
	if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
		return 0, err
	}
	if !apiResp.OK {
		err = fmt.Errorf("telegram sendMessage error: %s", apiResp.Description)
		b.noteTelegramRateLimit(err)
		return 0, err
	}
	return apiResp.Result.MessageID, nil
}

func (b *TelegramBot) sendAudioByFileID(chatID int64, entry CachedAudio, replyToID int, trackID string) error {
	entry = b.enrichCachedAudio(trackID, entry)
	if err := b.waitTelegramSend(context.Background(), chatID); err != nil {
		return err
	}
	sizeBytes := entry.SizeBytes
	if sizeBytes <= 0 {
		sizeBytes = entry.FileSize
	}
	bitrateKbps := entry.BitrateKbps
	format := normalizeTelegramFormat(entry.Format)
	if format == "" {
		format = defaultTelegramFormat
	}
	caption := formatTelegramCaption(sizeBytes, bitrateKbps, format)
	payload := map[string]any{
		"chat_id": chatID,
		"audio":   entry.FileID,
		"caption": caption,
	}
	if entry.Title != "" {
		payload["title"] = entry.Title
	}
	if entry.Performer != "" {
		payload["performer"] = entry.Performer
	}
	if replyToID > 0 {
		payload["reply_to_message_id"] = replyToID
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequest("POST", b.apiURL("sendAudio"), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		responseBody, _ := io.ReadAll(resp.Body)
		err = fmt.Errorf("telegram sendAudio failed: %s", strings.TrimSpace(string(responseBody)))
		b.noteTelegramRateLimit(err)
		return err
	}
	apiResp := sendAudioResponse{}
	if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
		return err
	}
	if !apiResp.OK {
		err = fmt.Errorf("telegram sendAudio error: %s", apiResp.Description)
		b.noteTelegramRateLimit(err)
		return err
	}
	return nil
}

func (b *TelegramBot) answerCallbackQuery(callbackID string) error {
	if callbackID == "" {
		return nil
	}
	payload := map[string]any{
		"callback_query_id": callbackID,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequest("POST", b.apiURL("answerCallbackQuery"), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil
}

func (b *TelegramBot) answerInlineQuery(inlineQueryID string, results any, personal bool) error {
	if inlineQueryID == "" {
		return nil
	}
	payload := map[string]any{
		"inline_query_id": inlineQueryID,
		"results":         results,
		"is_personal":     personal,
		"cache_time":      0,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequest("POST", b.apiURL("answerInlineQuery"), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil
}

func (b *TelegramBot) editMessageText(chatID int64, messageID int, text string, markup any) error {
	if messageID == 0 {
		return nil
	}
	payload := map[string]any{
		"chat_id":    chatID,
		"message_id": messageID,
		"text":       text,
	}
	if markup != nil {
		payload["reply_markup"] = markup
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequest("POST", b.apiURL("editMessageText"), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		apiResp := apiResponse{}
		if err := json.Unmarshal(responseBody, &apiResp); err == nil {
			if strings.Contains(apiResp.Description, "message is not modified") {
				return nil
			}
			if apiResp.Description != "" {
				err = fmt.Errorf("telegram editMessageText error: %s", apiResp.Description)
				b.noteTelegramRateLimit(err)
				return err
			}
		}
		err = fmt.Errorf("telegram editMessageText failed: %s", strings.TrimSpace(string(responseBody)))
		b.noteTelegramRateLimit(err)
		return err
	}
	apiResp := apiResponse{}
	if err := json.Unmarshal(responseBody, &apiResp); err != nil {
		return err
	}
	if !apiResp.OK {
		if strings.Contains(apiResp.Description, "message is not modified") {
			return nil
		}
		err = fmt.Errorf("telegram editMessageText error: %s", apiResp.Description)
		b.noteTelegramRateLimit(err)
		return err
	}
	return nil
}

func (b *TelegramBot) deleteMessage(chatID int64, messageID int) error {
	if messageID == 0 {
		return nil
	}
	payload := map[string]any{
		"chat_id":    chatID,
		"message_id": messageID,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequest("POST", b.apiURL("deleteMessage"), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil
}

func (b *TelegramBot) getUpdates(offset int) ([]Update, error) {
	return b.getUpdatesWithOptions(offset, 30, 0)
}

func (b *TelegramBot) getUpdatesWithOptions(offset int, timeoutSec int, limit int) ([]Update, error) {
	req, err := http.NewRequest("GET", b.apiURL("getUpdates"), nil)
	if err != nil {
		return nil, err
	}
	query := req.URL.Query()
	if timeoutSec < 0 {
		timeoutSec = 0
	}
	query.Set("timeout", strconv.Itoa(timeoutSec))
	if limit > 0 {
		query.Set("limit", strconv.Itoa(limit))
	}
	if offset > 0 {
		query.Set("offset", strconv.Itoa(offset))
	}
	req.URL.RawQuery = query.Encode()
	pollClient := b.pollClient
	if pollClient == nil {
		pollClient = b.client
	}
	resp, err := pollClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		responseBody, _ := io.ReadAll(resp.Body)
		apiResp := apiResponse{}
		if err := json.Unmarshal(responseBody, &apiResp); err == nil && strings.TrimSpace(apiResp.Description) != "" {
			return nil, fmt.Errorf("getUpdates failed: %s (%s)", resp.Status, apiResp.Description)
		}
		return nil, fmt.Errorf("getUpdates failed: %s", resp.Status)
	}
	var data getUpdatesResponse
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, err
	}
	if !data.OK {
		return nil, fmt.Errorf("getUpdates error: %s", data.Description)
	}
	return data.Result, nil
}

func (b *TelegramBot) apiURL(method string) string {
	return fmt.Sprintf("%s/bot%s/%s", b.apiBase, b.token, method)
}

func (b *TelegramBot) isAllowedChat(chatID int64) bool {
	if len(b.allowedChats) == 0 {
		return true
	}
	return b.allowedChats[chatID]
}

func (b *TelegramBot) setPending(chatID int64, kind string, query string, storefront string, offset int, items []apputils.SearchResultItem, hasNext bool, replyToID int, resultsMessageID int, title string) {
	b.pendingMu.Lock()
	if b.pending == nil {
		b.pending = make(map[int64]map[int]*PendingSelection)
	}
	if b.pending[chatID] == nil {
		b.pending[chatID] = make(map[int]*PendingSelection)
	}
	b.pending[chatID][resultsMessageID] = &PendingSelection{
		Kind:             kind,
		Query:            query,
		Title:            title,
		Storefront:       storefront,
		Offset:           offset,
		HasNext:          hasNext,
		Items:            items,
		CreatedAt:        time.Now(),
		ReplyToMessageID: replyToID,
		ResultsMessageID: resultsMessageID,
	}
	b.pendingMu.Unlock()
	b.requestStateSave()
}

func (b *TelegramBot) getPending(chatID int64, messageID int) (*PendingSelection, bool) {
	b.pendingMu.Lock()
	defer b.pendingMu.Unlock()
	chatPending, ok := b.pending[chatID]
	if !ok {
		return nil, false
	}
	pending, ok := chatPending[messageID]
	return pending, ok
}

func (b *TelegramBot) clearPending(chatID int64) {
	b.pendingMu.Lock()
	delete(b.pending, chatID)
	b.pendingMu.Unlock()
	b.requestStateSave()
}

func (b *TelegramBot) clearPendingByMessage(chatID int64, messageID int) {
	if messageID == 0 {
		return
	}
	b.pendingMu.Lock()
	chatPending, ok := b.pending[chatID]
	if !ok {
		b.pendingMu.Unlock()
		return
	}
	delete(chatPending, messageID)
	if len(chatPending) == 0 {
		delete(b.pending, chatID)
	}
	b.pendingMu.Unlock()
	b.requestStateSave()
}

func (b *TelegramBot) setPendingTransfer(chatID int64, mediaType string, mediaID string, mediaName string, storefront string, replyToID int, messageID int, forceRefresh bool) {
	b.transferMu.Lock()
	if b.pendingTransfers == nil {
		b.pendingTransfers = make(map[int64]map[int]*PendingTransfer)
	}
	if b.pendingTransfers[chatID] == nil {
		b.pendingTransfers[chatID] = make(map[int]*PendingTransfer)
	}
	b.pendingTransfers[chatID][messageID] = &PendingTransfer{
		MediaType:        mediaType,
		MediaID:          mediaID,
		MediaName:        mediaName,
		Storefront:       storefront,
		ForceRefresh:     forceRefresh,
		ReplyToMessageID: replyToID,
		MessageID:        messageID,
		CreatedAt:        time.Now(),
	}
	b.transferMu.Unlock()
	b.requestStateSave()
}

func (b *TelegramBot) getPendingTransfer(chatID int64, messageID int) (*PendingTransfer, bool) {
	b.transferMu.Lock()
	defer b.transferMu.Unlock()
	chatPending, ok := b.pendingTransfers[chatID]
	if !ok {
		return nil, false
	}
	pending, ok := chatPending[messageID]
	return pending, ok
}

func (b *TelegramBot) clearPendingTransfer(chatID int64) {
	b.transferMu.Lock()
	delete(b.pendingTransfers, chatID)
	b.transferMu.Unlock()
	b.requestStateSave()
}

func (b *TelegramBot) clearPendingTransferByMessage(chatID int64, messageID int) {
	if messageID == 0 {
		return
	}
	b.transferMu.Lock()
	chatPending, ok := b.pendingTransfers[chatID]
	if !ok {
		b.transferMu.Unlock()
		return
	}
	delete(chatPending, messageID)
	if len(chatPending) == 0 {
		delete(b.pendingTransfers, chatID)
	}
	b.transferMu.Unlock()
	b.requestStateSave()
}

func (b *TelegramBot) setPendingArtistMode(chatID int64, artistID string, artistName string, storefront string, replyToID int, messageID int) {
	b.artistModeMu.Lock()
	if b.pendingArtistModes == nil {
		b.pendingArtistModes = make(map[int64]map[int]*PendingArtistMode)
	}
	if b.pendingArtistModes[chatID] == nil {
		b.pendingArtistModes[chatID] = make(map[int]*PendingArtistMode)
	}
	b.pendingArtistModes[chatID][messageID] = &PendingArtistMode{
		ArtistID:         artistID,
		ArtistName:       artistName,
		Storefront:       storefront,
		ReplyToMessageID: replyToID,
		MessageID:        messageID,
		CreatedAt:        time.Now(),
	}
	b.artistModeMu.Unlock()
	b.requestStateSave()
}

func (b *TelegramBot) getPendingArtistMode(chatID int64, messageID int) (*PendingArtistMode, bool) {
	b.artistModeMu.Lock()
	defer b.artistModeMu.Unlock()
	chatPending, ok := b.pendingArtistModes[chatID]
	if !ok {
		return nil, false
	}
	pending, ok := chatPending[messageID]
	return pending, ok
}

func (b *TelegramBot) clearPendingArtistMode(chatID int64) {
	b.artistModeMu.Lock()
	delete(b.pendingArtistModes, chatID)
	b.artistModeMu.Unlock()
	b.requestStateSave()
}

func (b *TelegramBot) clearPendingArtistModeByMessage(chatID int64, messageID int) {
	if messageID == 0 {
		return
	}
	b.artistModeMu.Lock()
	chatPending, ok := b.pendingArtistModes[chatID]
	if !ok {
		b.artistModeMu.Unlock()
		return
	}
	delete(chatPending, messageID)
	if len(chatPending) == 0 {
		delete(b.pendingArtistModes, chatID)
	}
	b.artistModeMu.Unlock()
	b.requestStateSave()
}

func parseCommand(text string) (string, []string, bool) {
	if !strings.HasPrefix(text, "/") {
		return "", nil, false
	}
	parts := strings.Fields(text)
	if len(parts) == 0 {
		return "", nil, false
	}
	cmd := strings.TrimPrefix(parts[0], "/")
	if idx := strings.Index(cmd, "@"); idx != -1 {
		cmd = cmd[:idx]
	}
	return strings.ToLower(cmd), parts[1:], true
}

func buildInlineKeyboard(count int, hasPrev bool, hasNext bool) InlineKeyboardMarkup {
	rowSize := 4
	rows := [][]InlineKeyboardButton{}
	row := []InlineKeyboardButton{}
	for i := 1; i <= count; i++ {
		row = append(row, InlineKeyboardButton{
			Text:         strconv.Itoa(i),
			CallbackData: fmt.Sprintf("sel:%d", i),
		})
		if len(row) == rowSize {
			rows = append(rows, row)
			row = []InlineKeyboardButton{}
		}
	}
	if len(row) > 0 {
		rows = append(rows, row)
	}
	navRow := []InlineKeyboardButton{}
	if hasPrev {
		navRow = append(navRow, InlineKeyboardButton{
			Text:         "Prev",
			CallbackData: "page:-1",
		})
	}
	if hasNext {
		navRow = append(navRow, InlineKeyboardButton{
			Text:         "Next",
			CallbackData: "page:1",
		})
	}
	if len(navRow) > 0 {
		rows = append(rows, navRow)
	}
	rows = append(rows, []InlineKeyboardButton{
		{Text: "取消并删除", CallbackData: "panel_cancel"},
	})
	return InlineKeyboardMarkup{
		InlineKeyboard: rows,
	}
}

func buildTransferKeyboard() InlineKeyboardMarkup {
	return InlineKeyboardMarkup{
		InlineKeyboard: [][]InlineKeyboardButton{
			{
				{Text: "Transfer one by one", CallbackData: "transfer:one"},
				{Text: "ZIP", CallbackData: "transfer:zip"},
			},
			{
				{Text: "取消并删除", CallbackData: "panel_cancel"},
			},
		},
	}
}

func buildArtistModeKeyboard() InlineKeyboardMarkup {
	return InlineKeyboardMarkup{
		InlineKeyboard: [][]InlineKeyboardButton{
			{
				{Text: "Albums", CallbackData: "artist_rel:albums"},
				{Text: "Music Videos", CallbackData: "artist_rel:music-videos"},
			},
			{
				{Text: "取消并删除", CallbackData: "panel_cancel"},
			},
		},
	}
}

func settingButtonText(label string, active bool) string {
	if active {
		return "✓ " + label
	}
	return label
}

func (b *TelegramBot) formatSettingsText(settings ChatDownloadSettings) string {
	normalized := normalizeChatSettings(settings)
	songTransfer := "one-by-one"
	if normalized.SongZip {
		songTransfer = "zip"
	}
	workers := b.getTaskWorkerLimit()
	return fmt.Sprintf("Download settings:\n- Format: %s\n- AAC type: %s\n- MV audio: %s\n- Lyrics format: %s\n- Song transfer: %s\n- Task workers(global): %d\n- Auto extra: lyrics=%t cover=%t animated=%t",
		strings.ToUpper(normalized.Format),
		normalized.AACType,
		normalized.MVAudioType,
		strings.ToUpper(normalized.LyricsFormat),
		songTransfer,
		workers,
		normalized.AutoLyrics,
		normalized.AutoCover,
		normalized.AutoAnimated,
	)
}

func (b *TelegramBot) buildSettingsKeyboard(settings ChatDownloadSettings) InlineKeyboardMarkup {
	normalized := normalizeChatSettings(settings)
	format := normalized.Format
	aacType := normalized.AACType
	mvAudioType := normalized.MVAudioType
	lyricsFormat := normalized.LyricsFormat
	workers := b.getTaskWorkerLimit()
	return InlineKeyboardMarkup{
		InlineKeyboard: [][]InlineKeyboardButton{
			{
				{Text: settingButtonText("ALAC", format == telegramFormatAlac), CallbackData: "setting_format:alac"},
				{Text: settingButtonText("FLAC", format == telegramFormatFlac), CallbackData: "setting_format:flac"},
			},
			{
				{Text: settingButtonText("AAC", format == telegramFormatAac), CallbackData: "setting_format:aac"},
				{Text: settingButtonText("ATMOS", format == telegramFormatAtmos), CallbackData: "setting_format:atmos"},
			},
			{
				{Text: settingButtonText("AAC-LC", aacType == "aac-lc"), CallbackData: "setting_aac:aac-lc"},
				{Text: settingButtonText("AAC", aacType == "aac"), CallbackData: "setting_aac:aac"},
			},
			{
				{Text: settingButtonText("Binaural", aacType == "aac-binaural"), CallbackData: "setting_aac:aac-binaural"},
				{Text: settingButtonText("Downmix", aacType == "aac-downmix"), CallbackData: "setting_aac:aac-downmix"},
			},
			{
				{Text: settingButtonText("MV Atmos", mvAudioType == "atmos"), CallbackData: "setting_mv_audio:atmos"},
				{Text: settingButtonText("MV AC3", mvAudioType == "ac3"), CallbackData: "setting_mv_audio:ac3"},
				{Text: settingButtonText("MV AAC", mvAudioType == "aac"), CallbackData: "setting_mv_audio:aac"},
			},
			{
				{Text: settingButtonText("Lyrics LRC", lyricsFormat == "lrc"), CallbackData: "setting_lyrics_format:lrc"},
				{Text: settingButtonText("Lyrics TTML", lyricsFormat == "ttml"), CallbackData: "setting_lyrics_format:ttml"},
			},
			{
				{Text: settingButtonText("Song ZIP", normalized.SongZip), CallbackData: "setting_song_zip"},
			},
			{
				{Text: settingButtonText("线程 1", workers == 1), CallbackData: "setting_worker:1"},
				{Text: settingButtonText("线程 2", workers == 2), CallbackData: "setting_worker:2"},
			},
			{
				{Text: settingButtonText("线程 3", workers == 3), CallbackData: "setting_worker:3"},
				{Text: settingButtonText("线程 4", workers == 4), CallbackData: "setting_worker:4"},
			},
			{
				{Text: settingButtonText("Auto Lyrics", normalized.AutoLyrics), CallbackData: "setting_auto:lyrics"},
				{Text: settingButtonText("Auto Cover", normalized.AutoCover), CallbackData: "setting_auto:cover"},
				{Text: settingButtonText("Auto Animated", normalized.AutoAnimated), CallbackData: "setting_auto:animated"},
			},
			{
				{Text: "取消并删除", CallbackData: "panel_cancel"},
			},
		},
	}
}

func botHelpText() string {
	return strings.TrimSpace(`
命令列表（短命令）：
/h 帮助
/i 查看当前会话ID（chat_id）；也可按资源ID下载
/sg <关键词> 搜索歌曲
/sa <关键词> 搜索专辑
/sr <关键词> 搜索艺人
/s <类型> <关键词> 统一搜索
/u <Apple Music 链接> 解析并下载链接
/rf <Apple Music 链接> 强制重下并重传（清缓存，跳过本地复用）
/ap <艺人-url|艺人-id> 导出艺人主页图+专辑封面+动态封面（逐个/ZIP）
/cv <url|type id> 仅下载封面
/ac <url|type id> 仅下载动态封面
/ly <song|album> 导出歌词文件（格式由设置决定）
/st [值] 查看或修改下载设置（音质/AAC/MV/歌词/歌曲ZIP/任务线程/自动附加）

参数说明：
- /s 的 <类型>：song | album | artist
- /cv 的 type：song | album | playlist | station | mv | artist
- /ac 的 type：song | album | playlist | station
- /st 任务线程：worker1 | worker2 | worker3 | worker4（默认 worker1）

也支持直接发送 Apple Music 链接（自动识别）：
song | album | playlist | artist | station | music-video
`)
}

func formatChatIDText(chatID int64) string {
	return fmt.Sprintf(
		"Session ID (chat_id): %d\n"+
			"Use this value in config.yaml whitelist:\n"+
			"telegram-allowed-chat-ids:\n"+
			"  - %d",
		chatID,
		chatID,
	)
}
