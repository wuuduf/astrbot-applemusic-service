package main

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	apputils "github.com/wuuduf/astrbot-applemusic-service/utils"
	"github.com/wuuduf/astrbot-applemusic-service/utils/ampapi"
	"github.com/wuuduf/astrbot-applemusic-service/utils/lyrics"
	"github.com/wuuduf/astrbot-applemusic-service/utils/structs"
)

const (
	defaultAstrBotAPIListen = "127.0.0.1:27198"
	maxAstrBotJobHistory    = 300
	astrbotArtifactMaxAge   = 24 * time.Hour
)

var astrbotAPIHTTPClient = &http.Client{Timeout: 45 * time.Second}

type astrbotAPIService struct {
	appleToken   string
	apiToken     string
	listenAddr   string
	artifactRoot string

	seq   atomic.Uint64
	jobs  map[string]*astrbotJob
	order []string
	mu    sync.RWMutex

	queue chan *astrbotJob
}

type astrbotJobStatus string

const (
	astrbotJobQueued    astrbotJobStatus = "queued"
	astrbotJobRunning   astrbotJobStatus = "running"
	astrbotJobCompleted astrbotJobStatus = "completed"
	astrbotJobFailed    astrbotJobStatus = "failed"
)

type astrbotJob struct {
	ID        string                 `json:"job_id"`
	Status    astrbotJobStatus       `json:"status"`
	Error     string                 `json:"error,omitempty"`
	CreatedAt time.Time              `json:"created_at"`
	UpdatedAt time.Time              `json:"updated_at"`
	Request   astrbotDownloadRequest `json:"request"`
	Result    *astrbotDownloadResult `json:"result,omitempty"`
}

type astrbotSearchRequest struct {
	Type       string `json:"type"`
	Query      string `json:"query"`
	Storefront string `json:"storefront,omitempty"`
	Language   string `json:"language,omitempty"`
	Limit      int    `json:"limit,omitempty"`
	Offset     int    `json:"offset,omitempty"`
}

type astrbotSearchItem struct {
	MediaType     string `json:"media_type"`
	ID            string `json:"id"`
	Name          string `json:"name"`
	Artist        string `json:"artist,omitempty"`
	Album         string `json:"album,omitempty"`
	Detail        string `json:"detail,omitempty"`
	URL           string `json:"url,omitempty"`
	ContentRating string `json:"content_rating,omitempty"`
}

type astrbotResolveURLRequest struct {
	URL  string `json:"url,omitempty"`
	Text string `json:"text,omitempty"`
}

type astrbotResolveURLResponse struct {
	Target *AppleURLTarget `json:"target"`
}

type astrbotArtistChildrenRequest struct {
	ArtistID     string `json:"artist_id,omitempty"`
	URL          string `json:"url,omitempty"`
	Relationship string `json:"relationship,omitempty"`
	Storefront   string `json:"storefront,omitempty"`
	Language     string `json:"language,omitempty"`
	Limit        int    `json:"limit,omitempty"`
	Offset       int    `json:"offset,omitempty"`
}

type astrbotDownloadRequest struct {
	MediaType            string `json:"media_type,omitempty"`
	ID                   string `json:"id,omitempty"`
	URL                  string `json:"url,omitempty"`
	Storefront           string `json:"storefront,omitempty"`
	Quality              string `json:"quality,omitempty"`
	AACType              string `json:"aac_type,omitempty"`
	MVAudioType          string `json:"mv_audio_type,omitempty"`
	TransferMode         string `json:"transfer_mode,omitempty"`
	LyricsFormat         string `json:"lyrics_format,omitempty"`
	IncludeLyrics        *bool  `json:"include_lyrics,omitempty"`
	IncludeCover         *bool  `json:"include_cover,omitempty"`
	IncludeAnimatedCover *bool  `json:"include_animated_cover,omitempty"`
}

type astrbotDownloadResult struct {
	MediaType    string              `json:"media_type"`
	MediaID      string              `json:"media_id"`
	Storefront   string              `json:"storefront"`
	TransferMode string              `json:"transfer_mode"`
	Files        []astrbotOutputFile `json:"files"`
	ZipFile      *astrbotOutputFile  `json:"zip_file,omitempty"`
}

type astrbotOutputFile struct {
	Path           string `json:"path"`
	Name           string `json:"name"`
	Size           int64  `json:"size"`
	Kind           string `json:"kind"`
	TrackID        string `json:"track_id,omitempty"`
	Title          string `json:"title,omitempty"`
	Performer      string `json:"performer,omitempty"`
	DurationMillis int64  `json:"duration_millis,omitempty"`
	Temporary      bool   `json:"temporary,omitempty"`
}

type astrbotArtworkRequest struct {
	MediaType  string `json:"media_type,omitempty"`
	ID         string `json:"id,omitempty"`
	URL        string `json:"url,omitempty"`
	Storefront string `json:"storefront,omitempty"`
	Animated   bool   `json:"animated,omitempty"`
}

type astrbotArtworkResponse struct {
	MediaType   string            `json:"media_type"`
	MediaID     string            `json:"media_id"`
	Storefront  string            `json:"storefront"`
	DisplayName string            `json:"display_name"`
	Animated    bool              `json:"animated"`
	File        astrbotOutputFile `json:"file"`
}

type astrbotLyricsRequest struct {
	MediaType    string `json:"media_type,omitempty"`
	ID           string `json:"id,omitempty"`
	URL          string `json:"url,omitempty"`
	Storefront   string `json:"storefront,omitempty"`
	OutputFormat string `json:"output_format,omitempty"`
	TransferMode string `json:"transfer_mode,omitempty"`
}

type astrbotLyricsResponse struct {
	MediaType   string              `json:"media_type"`
	MediaID     string              `json:"media_id"`
	Storefront  string              `json:"storefront"`
	Format      string              `json:"format"`
	LyricsType  string              `json:"lyrics_type,omitempty"`
	Files       []astrbotOutputFile `json:"files"`
	ZipFile     *astrbotOutputFile  `json:"zip_file,omitempty"`
	FailedCount int                 `json:"failed_count,omitempty"`
}

type astrbotErrorResponse struct {
	Error string `json:"error"`
}

func runAstrBotAPIServer(token string, listenAddr string) error {
	listenAddr = strings.TrimSpace(listenAddr)
	if listenAddr == "" {
		listenAddr = defaultAstrBotAPIListen
	}
	apiToken := strings.TrimSpace(os.Getenv("ASTRBOT_API_TOKEN"))
	if !isLoopbackListenAddr(listenAddr) && apiToken == "" {
		return fmt.Errorf("refusing non-loopback bind (%s) without ASTRBOT_API_TOKEN", listenAddr)
	}
	artifactRoot := filepath.Join(os.TempDir(), "amdl-astrbot-api")
	if err := os.MkdirAll(artifactRoot, 0755); err != nil {
		return fmt.Errorf("failed to create artifact root: %w", err)
	}
	service := &astrbotAPIService{
		appleToken:   token,
		apiToken:     apiToken,
		listenAddr:   listenAddr,
		artifactRoot: artifactRoot,
		jobs:         make(map[string]*astrbotJob),
		queue:        make(chan *astrbotJob, maxAstrBotJobHistory),
	}
	service.startWorker()
	service.cleanupArtifacts(astrbotArtifactMaxAge)

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", service.handleHealth)
	mux.HandleFunc("/v1/search", service.handleSearch)
	mux.HandleFunc("/v1/resolve-url", service.handleResolveURL)
	mux.HandleFunc("/v1/artist-children", service.handleArtistChildren)
	mux.HandleFunc("/v1/download", service.handleDownload)
	mux.HandleFunc("/v1/jobs/", service.handleJobStatus)
	mux.HandleFunc("/v1/artwork", service.handleArtwork)
	mux.HandleFunc("/v1/lyrics", service.handleLyrics)

	srv := &http.Server{
		Addr:              service.listenAddr,
		Handler:           service.withRecovery(mux),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      120 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}

	fmt.Printf("AstrBot API server listening on http://%s\n", service.listenAddr)
	fmt.Printf("AstrBot API artifact root: %s\n", service.artifactRoot)
	if apiToken == "" {
		fmt.Println("AstrBot API auth: disabled (set ASTRBOT_API_TOKEN to enable)")
	} else {
		fmt.Println("AstrBot API auth: enabled (Authorization: Bearer <token> or X-AstrBot-Token)")
	}
	return srv.ListenAndServe()
}

func (s *astrbotAPIService) withRecovery(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				fmt.Printf("AstrBot API panic: %v\n", rec)
				writeJSON(w, http.StatusInternalServerError, astrbotErrorResponse{Error: "internal server error"})
			}
		}()
		if strings.HasPrefix(r.URL.Path, "/v1/") && !s.authorized(r) {
			writeJSON(w, http.StatusUnauthorized, astrbotErrorResponse{Error: "unauthorized"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *astrbotAPIService) startWorker() {
	go func() {
		for job := range s.queue {
			func() {
				defer func() {
					if rec := recover(); rec != nil {
						s.setJobFailed(job.ID, fmt.Errorf("worker panic: %v", rec))
					}
				}()
				s.setJobRunning(job.ID)
				result, err := s.executeDownload(job.Request)
				if err != nil {
					s.setJobFailed(job.ID, err)
					return
				}
				s.setJobCompleted(job.ID, result)
			}()
		}
	}()
}

func (s *astrbotAPIService) authorized(r *http.Request) bool {
	if strings.TrimSpace(s.apiToken) == "" {
		return true
	}
	if token := parseBearerToken(r.Header.Get("Authorization")); token != "" {
		return secureTokenEqual(token, s.apiToken)
	}
	if token := strings.TrimSpace(r.Header.Get("X-AstrBot-Token")); token != "" {
		return secureTokenEqual(token, s.apiToken)
	}
	return false
}

func parseBearerToken(raw string) string {
	parts := strings.Fields(strings.TrimSpace(raw))
	if len(parts) != 2 {
		return ""
	}
	if !strings.EqualFold(parts[0], "bearer") {
		return ""
	}
	return strings.TrimSpace(parts[1])
}

func secureTokenEqual(got, want string) bool {
	if got == "" || want == "" {
		return false
	}
	if len(got) != len(want) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

func isLoopbackListenAddr(listenAddr string) bool {
	host := strings.TrimSpace(listenAddr)
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	host = strings.TrimSpace(host)
	if host == "" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback()
}

func (s *astrbotAPIService) nextJobID() string {
	seq := s.seq.Add(1)
	return fmt.Sprintf("job_%d_%06d", time.Now().UnixMilli(), seq)
}

func (s *astrbotAPIService) addJob(req astrbotDownloadRequest) *astrbotJob {
	now := time.Now()
	job := &astrbotJob{
		ID:        s.nextJobID(),
		Status:    astrbotJobQueued,
		CreatedAt: now,
		UpdatedAt: now,
		Request:   req,
	}
	s.mu.Lock()
	s.jobs[job.ID] = job
	s.order = append(s.order, job.ID)
	s.pruneJobsLocked()
	s.mu.Unlock()
	return job
}

func (s *astrbotAPIService) removeJob(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.jobs, id)
	for idx, jobID := range s.order {
		if jobID == id {
			s.order = append(s.order[:idx], s.order[idx+1:]...)
			break
		}
	}
}

func (s *astrbotAPIService) pruneJobsLocked() {
	if len(s.order) <= maxAstrBotJobHistory {
		return
	}
	for len(s.order) > maxAstrBotJobHistory {
		oldID := s.order[0]
		s.order = s.order[1:]
		delete(s.jobs, oldID)
	}
}

func (s *astrbotAPIService) getJob(id string) (*astrbotJob, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	job, ok := s.jobs[id]
	if !ok {
		return nil, false
	}
	copied := *job
	if job.Result != nil {
		resCopy := *job.Result
		if job.Result.Files != nil {
			resCopy.Files = append([]astrbotOutputFile{}, job.Result.Files...)
		}
		if job.Result.ZipFile != nil {
			zipCopy := *job.Result.ZipFile
			resCopy.ZipFile = &zipCopy
		}
		copied.Result = &resCopy
	}
	return &copied, true
}

func (s *astrbotAPIService) setJobRunning(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if job, ok := s.jobs[id]; ok {
		job.Status = astrbotJobRunning
		job.Error = ""
		job.UpdatedAt = time.Now()
	}
}

func (s *astrbotAPIService) setJobCompleted(id string, result *astrbotDownloadResult) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if job, ok := s.jobs[id]; ok {
		job.Status = astrbotJobCompleted
		job.Error = ""
		job.Result = result
		job.UpdatedAt = time.Now()
	}
}

func (s *astrbotAPIService) setJobFailed(id string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if job, ok := s.jobs[id]; ok {
		job.Status = astrbotJobFailed
		if err != nil {
			job.Error = err.Error()
		} else {
			job.Error = "download failed"
		}
		job.UpdatedAt = time.Now()
	}
}

func (s *astrbotAPIService) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, astrbotErrorResponse{Error: "method not allowed"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":            true,
		"service":       "apple-music-downloader-bot",
		"mode":          "astrbot-api",
		"auth_required": strings.TrimSpace(s.apiToken) != "",
		"timestamp":     time.Now(),
	})
}

func (s *astrbotAPIService) handleSearch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, astrbotErrorResponse{Error: "method not allowed"})
		return
	}
	var req astrbotSearchRequest
	if err := decodeJSON(r.Body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, astrbotErrorResponse{Error: err.Error()})
		return
	}
	kind := strings.ToLower(strings.TrimSpace(req.Type))
	if kind != "song" && kind != "album" && kind != "artist" {
		writeJSON(w, http.StatusBadRequest, astrbotErrorResponse{Error: "type must be song|album|artist"})
		return
	}
	query := strings.TrimSpace(req.Query)
	if query == "" {
		writeJSON(w, http.StatusBadRequest, astrbotErrorResponse{Error: "query is required"})
		return
	}
	limit := req.Limit
	if limit <= 0 {
		limit = defaultSearchLimit
	}
	if limit > 30 {
		limit = 30
	}
	offset := req.Offset
	if offset < 0 {
		offset = 0
	}
	storefront := normalizeStorefront(req.Storefront)
	lang := normalizeLanguage(req.Language)
	resp, err := ampapi.Search(storefront, query, kind+"s", lang, s.appleToken, limit, offset)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, astrbotErrorResponse{Error: fmt.Sprintf("search failed: %v", err)})
		return
	}
	items, hasNext := apputils.BuildSearchItems(kind, resp)
	out := make([]astrbotSearchItem, 0, len(items))
	for _, item := range items {
		out = append(out, astrbotSearchItem{
			MediaType:     itemTypeToMediaType(item.Type),
			ID:            strings.TrimSpace(item.ID),
			Name:          strings.TrimSpace(item.Name),
			Artist:        strings.TrimSpace(item.Artist),
			Album:         strings.TrimSpace(item.Album),
			Detail:        strings.TrimSpace(item.Detail),
			URL:           strings.TrimSpace(item.URL),
			ContentRating: strings.TrimSpace(item.ContentRating),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"type":       kind,
		"query":      query,
		"storefront": storefront,
		"language":   lang,
		"limit":      limit,
		"offset":     offset,
		"has_next":   hasNext,
		"items":      out,
	})
}

func (s *astrbotAPIService) handleResolveURL(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, astrbotErrorResponse{Error: "method not allowed"})
		return
	}
	var req astrbotResolveURLRequest
	if err := decodeJSON(r.Body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, astrbotErrorResponse{Error: err.Error()})
		return
	}
	raw := strings.TrimSpace(req.URL)
	if raw == "" {
		raw = extractFirstAppleMusicURL(req.Text)
	}
	if raw == "" {
		writeJSON(w, http.StatusBadRequest, astrbotErrorResponse{Error: "url is required"})
		return
	}
	target, err := parseAppleMusicURL(raw)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, astrbotErrorResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, astrbotResolveURLResponse{Target: target})
}

func (s *astrbotAPIService) handleArtistChildren(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, astrbotErrorResponse{Error: "method not allowed"})
		return
	}
	var req astrbotArtistChildrenRequest
	if err := decodeJSON(r.Body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, astrbotErrorResponse{Error: err.Error()})
		return
	}
	artistID := strings.TrimSpace(req.ArtistID)
	storefront := normalizeStorefront(req.Storefront)
	if strings.TrimSpace(req.URL) != "" {
		target, err := parseAppleMusicURL(req.URL)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, astrbotErrorResponse{Error: err.Error()})
			return
		}
		if target.MediaType != mediaTypeArtist {
			writeJSON(w, http.StatusBadRequest, astrbotErrorResponse{Error: "url is not an artist url"})
			return
		}
		artistID = target.ID
		if target.Storefront != "" {
			storefront = target.Storefront
		}
	}
	if artistID == "" {
		writeJSON(w, http.StatusBadRequest, astrbotErrorResponse{Error: "artist_id is required"})
		return
	}
	relationship := normalizeArtistRelationship(req.Relationship)
	if relationship == "" {
		relationship = "albums"
	}
	limit := req.Limit
	if limit <= 0 {
		limit = defaultSearchLimit
	}
	if limit > 50 {
		limit = 50
	}
	offset := req.Offset
	if offset < 0 {
		offset = 0
	}
	lang := normalizeLanguage(req.Language)
	var (
		items   []apputils.SearchResultItem
		hasNext bool
		err     error
	)
	if relationship == "albums" {
		items, hasNext, err = apputils.FetchArtistAlbums(storefront, artistID, s.appleToken, limit, offset, lang)
	} else {
		items, hasNext, err = apputils.FetchArtistMusicVideos(storefront, artistID, s.appleToken, limit, offset, lang)
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, astrbotErrorResponse{Error: fmt.Sprintf("artist-children failed: %v", err)})
		return
	}
	out := make([]astrbotSearchItem, 0, len(items))
	for _, item := range items {
		out = append(out, astrbotSearchItem{
			MediaType:     itemTypeToMediaType(item.Type),
			ID:            strings.TrimSpace(item.ID),
			Name:          strings.TrimSpace(item.Name),
			Artist:        strings.TrimSpace(item.Artist),
			Album:         strings.TrimSpace(item.Album),
			Detail:        strings.TrimSpace(item.Detail),
			URL:           strings.TrimSpace(item.URL),
			ContentRating: strings.TrimSpace(item.ContentRating),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"artist_id":    artistID,
		"relationship": relationship,
		"storefront":   storefront,
		"language":     lang,
		"limit":        limit,
		"offset":       offset,
		"has_next":     hasNext,
		"items":        out,
	})
}

func (s *astrbotAPIService) handleDownload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, astrbotErrorResponse{Error: "method not allowed"})
		return
	}
	var req astrbotDownloadRequest
	if err := decodeJSON(r.Body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, astrbotErrorResponse{Error: err.Error()})
		return
	}
	normalized, err := s.normalizeDownloadRequest(req)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, astrbotErrorResponse{Error: err.Error()})
		return
	}
	job := s.addJob(normalized)
	select {
	case s.queue <- job:
		writeJSON(w, http.StatusAccepted, map[string]any{
			"job_id":     job.ID,
			"status":     job.Status,
			"poll_url":   "/v1/jobs/" + job.ID,
			"created_at": job.CreatedAt,
		})
	default:
		s.removeJob(job.ID)
		writeJSON(w, http.StatusServiceUnavailable, astrbotErrorResponse{Error: "download queue is full"})
	}
}

func (s *astrbotAPIService) handleJobStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, astrbotErrorResponse{Error: "method not allowed"})
		return
	}
	jobID := strings.TrimSpace(strings.TrimPrefix(r.URL.Path, "/v1/jobs/"))
	if jobID == "" || strings.Contains(jobID, "/") {
		writeJSON(w, http.StatusBadRequest, astrbotErrorResponse{Error: "invalid job_id"})
		return
	}
	job, ok := s.getJob(jobID)
	if !ok {
		writeJSON(w, http.StatusNotFound, astrbotErrorResponse{Error: "job not found"})
		return
	}
	writeJSON(w, http.StatusOK, job)
}

func (s *astrbotAPIService) handleArtwork(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, astrbotErrorResponse{Error: "method not allowed"})
		return
	}
	s.cleanupArtifacts(astrbotArtifactMaxAge)
	var req astrbotArtworkRequest
	if err := decodeJSON(r.Body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, astrbotErrorResponse{Error: err.Error()})
		return
	}
	target, err := resolveTargetFromRequest(req.URL, req.MediaType, req.ID, req.Storefront)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, astrbotErrorResponse{Error: err.Error()})
		return
	}
	if req.Animated {
		s.handleArtworkAnimated(w, target)
		return
	}
	resp, err := s.fetchArtwork(target)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, astrbotErrorResponse{Error: err.Error()})
		return
	}
	coverPath, tmpDir, err := renderCoverToTemp(resp.CoverURL)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, astrbotErrorResponse{Error: fmt.Sprintf("download cover failed: %v", err)})
		return
	}
	defer os.RemoveAll(tmpDir)
	displayName := sanitizeFileBaseName(resp.DisplayName) + "-cover" + strings.ToLower(filepath.Ext(coverPath))
	finalPath, err := s.persistArtifactFile(coverPath, displayName)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, astrbotErrorResponse{Error: fmt.Sprintf("persist cover failed: %v", err)})
		return
	}
	file, err := buildOutputFile(finalPath, true)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, astrbotErrorResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, astrbotArtworkResponse{
		MediaType:   target.MediaType,
		MediaID:     target.ID,
		Storefront:  resolveStorefront(target),
		DisplayName: resp.DisplayName,
		Animated:    false,
		File:        file,
	})
}

func (s *astrbotAPIService) handleArtworkAnimated(w http.ResponseWriter, target *AppleURLTarget) {
	if target == nil {
		writeJSON(w, http.StatusBadRequest, astrbotErrorResponse{Error: "invalid target"})
		return
	}
	switch target.MediaType {
	case mediaTypeSong, mediaTypeAlbum, mediaTypePlaylist, mediaTypeStation:
	default:
		writeJSON(w, http.StatusBadRequest, astrbotErrorResponse{Error: "animated artwork only supports song|album|playlist|station"})
		return
	}
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		writeJSON(w, http.StatusBadRequest, astrbotErrorResponse{Error: "animated artwork requires ffmpeg in PATH"})
		return
	}
	resp, err := s.fetchArtwork(target)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, astrbotErrorResponse{Error: err.Error()})
		return
	}
	if strings.TrimSpace(resp.MotionURL) == "" {
		writeJSON(w, http.StatusBadRequest, astrbotErrorResponse{Error: "animated artwork not available"})
		return
	}
	videoURL, err := extractVideo(resp.MotionURL)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, astrbotErrorResponse{Error: fmt.Sprintf("resolve animated artwork failed: %v", err)})
		return
	}
	tmpFile, err := os.CreateTemp("", "amdl-animated-cover-*.mp4")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, astrbotErrorResponse{Error: err.Error()})
		return
	}
	tmpPath := tmpFile.Name()
	_ = tmpFile.Close()
	defer os.Remove(tmpPath)
	cmd := exec.Command("ffmpeg", "-loglevel", "error", "-y", "-i", videoURL, "-c", "copy", tmpPath)
	if output, err := cmd.CombinedOutput(); err != nil {
		errMsg := strings.TrimSpace(string(output))
		if errMsg == "" {
			errMsg = err.Error()
		}
		writeJSON(w, http.StatusInternalServerError, astrbotErrorResponse{Error: fmt.Sprintf("download animated artwork failed: %s", errMsg)})
		return
	}
	displayName := sanitizeFileBaseName(resp.DisplayName) + "-animated-cover.mp4"
	finalPath, err := s.persistArtifactFile(tmpPath, displayName)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, astrbotErrorResponse{Error: fmt.Sprintf("persist animated artwork failed: %v", err)})
		return
	}
	file, err := buildOutputFile(finalPath, true)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, astrbotErrorResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, astrbotArtworkResponse{
		MediaType:   target.MediaType,
		MediaID:     target.ID,
		Storefront:  resolveStorefront(target),
		DisplayName: resp.DisplayName,
		Animated:    true,
		File:        file,
	})
}

func (s *astrbotAPIService) handleLyrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, astrbotErrorResponse{Error: "method not allowed"})
		return
	}
	s.cleanupArtifacts(astrbotArtifactMaxAge)
	var req astrbotLyricsRequest
	if err := decodeJSON(r.Body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, astrbotErrorResponse{Error: err.Error()})
		return
	}
	target, err := resolveTargetFromRequest(req.URL, req.MediaType, req.ID, req.Storefront)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, astrbotErrorResponse{Error: err.Error()})
		return
	}
	if target.MediaType != mediaTypeSong && target.MediaType != mediaTypeAlbum {
		writeJSON(w, http.StatusBadRequest, astrbotErrorResponse{Error: "lyrics only supports song|album"})
		return
	}
	if len(strings.TrimSpace(Config.MediaUserToken)) <= 50 {
		writeJSON(w, http.StatusBadRequest, astrbotErrorResponse{Error: "lyrics requires media-user-token in config.yaml"})
		return
	}
	format := normalizeLyricsOutputFormat(req.OutputFormat)
	if format == "" {
		format = normalizeLyricsOutputFormat(Config.LrcFormat)
	}
	if format == "" {
		format = defaultTelegramLyricsFormat
	}
	storefront := resolveStorefront(target)
	if target.MediaType == mediaTypeSong {
		content, lyricType, err := s.fetchLyricsOnly(target.ID, storefront, format)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, astrbotErrorResponse{Error: err.Error()})
			return
		}
		baseName := "song-" + target.ID
		if resp, err := ampapi.GetSongResp(storefront, target.ID, Config.Language, s.appleToken); err == nil && len(resp.Data) > 0 {
			baseName = composeArtistTitle(resp.Data[0].Attributes.ArtistName, resp.Data[0].Attributes.Name)
		}
		displayName := sanitizeFileBaseName(baseName) + ".lyrics." + format
		path, err := s.writeArtifactTextFile(displayName, content)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, astrbotErrorResponse{Error: err.Error()})
			return
		}
		file, err := buildOutputFile(path, true)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, astrbotErrorResponse{Error: err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, astrbotLyricsResponse{
			MediaType:  target.MediaType,
			MediaID:    target.ID,
			Storefront: storefront,
			Format:     format,
			LyricsType: lyricType,
			Files:      []astrbotOutputFile{file},
		})
		return
	}
	transferMode := strings.ToLower(strings.TrimSpace(req.TransferMode))
	if transferMode != transferModeZip {
		transferMode = transferModeOneByOne
	}
	files, failedCount, err := s.exportAlbumLyrics(target.ID, storefront, format)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, astrbotErrorResponse{Error: err.Error()})
		return
	}
	out := make([]astrbotOutputFile, 0, len(files))
	for _, path := range files {
		item, err := buildOutputFile(path, true)
		if err != nil {
			continue
		}
		out = append(out, item)
	}
	resp := astrbotLyricsResponse{
		MediaType:   target.MediaType,
		MediaID:     target.ID,
		Storefront:  storefront,
		Format:      format,
		Files:       out,
		FailedCount: failedCount,
	}
	if transferMode == transferModeZip && len(files) > 1 {
		zipPath, displayName, err := createZipFromPaths(files)
		if err == nil {
			finalZip, perr := s.persistArtifactFile(zipPath, displayName)
			_ = os.Remove(zipPath)
			if perr == nil {
				if zipItem, ierr := buildOutputFile(finalZip, true); ierr == nil {
					resp.ZipFile = &zipItem
				}
			}
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *astrbotAPIService) normalizeDownloadRequest(req astrbotDownloadRequest) (astrbotDownloadRequest, error) {
	target, err := resolveTargetFromRequest(req.URL, req.MediaType, req.ID, req.Storefront)
	if err != nil {
		return astrbotDownloadRequest{}, err
	}
	switch target.MediaType {
	case mediaTypeSong, mediaTypeAlbum, mediaTypePlaylist, mediaTypeStation, mediaTypeMusicVideo:
	default:
		return astrbotDownloadRequest{}, fmt.Errorf("download does not support media_type=%s", target.MediaType)
	}
	quality := normalizeTelegramFormat(req.Quality)
	if quality == "" {
		quality = defaultTelegramFormat
	}
	aacType := normalizeTelegramAACType(req.AACType)
	if aacType == "" {
		aacType = normalizeTelegramAACType(Config.AacType)
	}
	if aacType == "" {
		aacType = defaultTelegramAACType
	}
	mvAudioType := normalizeTelegramMVAudioType(req.MVAudioType)
	if mvAudioType == "" {
		mvAudioType = normalizeTelegramMVAudioType(Config.MVAudioType)
	}
	if mvAudioType == "" {
		mvAudioType = defaultTelegramMVAudioType
	}
	transferMode := strings.ToLower(strings.TrimSpace(req.TransferMode))
	if transferMode != transferModeZip {
		transferMode = transferModeOneByOne
	}
	lyricsFormat := normalizeLyricsOutputFormat(req.LyricsFormat)
	if lyricsFormat == "" {
		lyricsFormat = normalizeLyricsOutputFormat(Config.LrcFormat)
	}
	if lyricsFormat == "" {
		lyricsFormat = defaultTelegramLyricsFormat
	}
	includeLyrics := true
	if req.IncludeLyrics != nil {
		includeLyrics = *req.IncludeLyrics
	}
	includeCover := true
	if req.IncludeCover != nil {
		includeCover = *req.IncludeCover
	}
	includeAnimated := true
	if req.IncludeAnimatedCover != nil {
		includeAnimated = *req.IncludeAnimatedCover
	}
	storefront := resolveStorefront(target)
	return astrbotDownloadRequest{
		MediaType:            target.MediaType,
		ID:                   target.ID,
		URL:                  target.RawURL,
		Storefront:           storefront,
		Quality:              quality,
		AACType:              aacType,
		MVAudioType:          mvAudioType,
		TransferMode:         transferMode,
		LyricsFormat:         lyricsFormat,
		IncludeLyrics:        boolPtr(includeLyrics),
		IncludeCover:         boolPtr(includeCover),
		IncludeAnimatedCover: boolPtr(includeAnimated),
	}, nil
}

func (s *astrbotAPIService) executeDownload(req astrbotDownloadRequest) (*astrbotDownloadResult, error) {
	lastDownloadedPaths = nil
	clearDownloadState()
	counter = structs.Counter{}
	okDict = make(map[string][]int)

	dl_atmos = false
	dl_aac = false
	dl_select = false
	dl_song = false

	quality := normalizeTelegramFormat(req.Quality)
	if quality == "" {
		quality = defaultTelegramFormat
	}
	switch quality {
	case telegramFormatAtmos:
		dl_atmos = true
	case telegramFormatAac:
		dl_aac = true
	}
	includeLyrics := req.IncludeLyrics != nil && *req.IncludeLyrics
	includeCover := req.IncludeCover == nil || *req.IncludeCover
	includeAnimated := req.IncludeAnimatedCover == nil || *req.IncludeAnimatedCover
	lyricsFormat := normalizeLyricsOutputFormat(req.LyricsFormat)
	if lyricsFormat == "" {
		lyricsFormat = defaultTelegramLyricsFormat
	}
	transferMode := req.TransferMode
	if transferMode != transferModeZip {
		transferMode = transferModeOneByOne
	}
	if req.MediaType == mediaTypeMusicVideo {
		transferMode = transferModeOneByOne
	}

	prevAacType := Config.AacType
	prevMVAudioType := Config.MVAudioType
	prevLrcFormat := Config.LrcFormat
	prevSaveLrcFile := Config.SaveLrcFile
	prevEmbedLrc := Config.EmbedLrc
	prevSaveAnimatedArtwork := Config.SaveAnimatedArtwork
	prevConvertAfterDownload := Config.ConvertAfterDownload
	prevConvertFormat := Config.ConvertFormat
	prevConvertKeepOriginal := Config.ConvertKeepOriginal
	prevConvertSkipLossyToLossless := Config.ConvertSkipLossyToLossless
	prevStaticCoverDownload := botStaticCoverDownload
	defer func() {
		Config.AacType = prevAacType
		Config.MVAudioType = prevMVAudioType
		Config.LrcFormat = prevLrcFormat
		Config.SaveLrcFile = prevSaveLrcFile
		Config.EmbedLrc = prevEmbedLrc
		Config.SaveAnimatedArtwork = prevSaveAnimatedArtwork
		Config.ConvertAfterDownload = prevConvertAfterDownload
		Config.ConvertFormat = prevConvertFormat
		Config.ConvertKeepOriginal = prevConvertKeepOriginal
		Config.ConvertSkipLossyToLossless = prevConvertSkipLossyToLossless
		botStaticCoverDownload = prevStaticCoverDownload
	}()

	Config.AacType = req.AACType
	Config.MVAudioType = req.MVAudioType
	Config.LrcFormat = lyricsFormat
	Config.SaveLrcFile = includeLyrics
	Config.EmbedLrc = false
	Config.SaveAnimatedArtwork = includeAnimated
	botStaticCoverDownload = includeCover

	Config.ConvertAfterDownload = false
	if quality == telegramFormatFlac {
		Config.ConvertAfterDownload = true
		Config.ConvertFormat = telegramFormatFlac
		Config.ConvertKeepOriginal = false
		Config.ConvertSkipLossyToLossless = false
		if _, err := exec.LookPath(Config.FFmpegPath); err != nil {
			return nil, fmt.Errorf("ffmpeg not found at '%s'", Config.FFmpegPath)
		}
	} else {
		Config.ConvertFormat = ""
	}

	storefront := normalizeStorefront(req.Storefront)
	mediaID := strings.TrimSpace(req.ID)
	if mediaID == "" {
		return nil, fmt.Errorf("media id is required")
	}
	var err error
	switch req.MediaType {
	case mediaTypeSong:
		dl_song = true
		err = ripSong(mediaID, s.appleToken, storefront, Config.MediaUserToken)
		dl_song = false
	case mediaTypeAlbum:
		err = ripAlbum(mediaID, s.appleToken, storefront, Config.MediaUserToken, "")
	case mediaTypePlaylist:
		err = ripPlaylist(mediaID, s.appleToken, storefront, Config.MediaUserToken)
	case mediaTypeStation:
		if len(strings.TrimSpace(Config.MediaUserToken)) <= 50 {
			return nil, fmt.Errorf("station download requires media-user-token")
		}
		err = ripStation(mediaID, s.appleToken, storefront, Config.MediaUserToken)
	case mediaTypeMusicVideo:
		if len(strings.TrimSpace(Config.MediaUserToken)) <= 50 {
			return nil, fmt.Errorf("mv download requires media-user-token")
		}
		if _, lookErr := exec.LookPath("mp4decrypt"); lookErr != nil {
			return nil, fmt.Errorf("mv download requires mp4decrypt in PATH")
		}
		saveDir := strings.TrimSpace(Config.AlacSaveFolder)
		if saveDir == "" {
			saveDir = "AM-DL downloads"
		}
		err = mvDownloader(mediaID, saveDir, s.appleToken, storefront, Config.MediaUserToken, nil)
	default:
		return nil, fmt.Errorf("unsupported media type: %s", req.MediaType)
	}
	if err != nil {
		return nil, err
	}

	paths := append([]string{}, lastDownloadedPaths...)
	if len(paths) == 0 {
		return nil, fmt.Errorf("no files were downloaded")
	}
	primaryCount := len(paths)
	if req.MediaType == mediaTypeSong || req.MediaType == mediaTypeAlbum {
		paths = augmentDownloadedPathsForRequest(paths, includeLyrics, includeCover, includeAnimated, lyricsFormat)
	}
	files := make([]astrbotOutputFile, 0, len(paths))
	for _, path := range paths {
		item, ferr := buildOutputFile(path, false)
		if ferr != nil {
			continue
		}
		files = append(files, item)
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("downloaded files are missing")
	}
	result := &astrbotDownloadResult{
		MediaType:    req.MediaType,
		MediaID:      mediaID,
		Storefront:   storefront,
		TransferMode: transferMode,
		Files:        files,
	}
	if transferMode == transferModeZip && (primaryCount > 1 || req.MediaType == mediaTypeSong) {
		zipPath, displayName, zerr := createZipFromPaths(paths)
		if zerr == nil {
			finalZip, perr := s.persistArtifactFile(zipPath, displayName)
			_ = os.Remove(zipPath)
			if perr == nil {
				if zipItem, ierr := buildOutputFile(finalZip, true); ierr == nil {
					result.ZipFile = &zipItem
				}
			}
		}
	}
	return result, nil
}

func augmentDownloadedPathsForRequest(paths []string, includeLyrics bool, includeCover bool, includeAnimated bool, lyricsFormat string) []string {
	if len(paths) == 0 {
		return paths
	}
	if !includeLyrics && !includeCover && !includeAnimated {
		return paths
	}
	if lyricsFormat == "" {
		lyricsFormat = defaultTelegramLyricsFormat
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
		if includeCover {
			if _, ok := coverDone[dir]; !ok {
				coverDone[dir] = struct{}{}
				appendFile(findCoverFile(dir))
			}
		}
		if includeAnimated {
			if _, ok := animatedDone[dir]; !ok {
				animatedDone[dir] = struct{}{}
				appendFile(filepath.Join(dir, "square_animated_artwork.mp4"))
				appendFile(filepath.Join(dir, "tall_animated_artwork.mp4"))
			}
		}
		if includeLyrics && isAudioPath(path) {
			ext := strings.ToLower(filepath.Ext(path))
			lyricsPath := strings.TrimSuffix(path, ext) + "." + lyricsFormat
			appendFile(lyricsPath)
		}
	}
	return result
}

func (s *astrbotAPIService) fetchLyricsOnly(songID string, storefront string, outputFormat string) (string, string, error) {
	var lastErr error
	lyricTypes := []string{"syllable-lyrics", "lyrics"}
	for _, lyricType := range lyricTypes {
		content, err := lyrics.Get(storefront, songID, lyricType, Config.Language, outputFormat, s.appleToken, Config.MediaUserToken)
		if err != nil {
			lastErr = err
			continue
		}
		if strings.TrimSpace(content) == "" {
			lastErr = fmt.Errorf("empty lyrics content")
			continue
		}
		return content, lyricType, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("lyrics unavailable")
	}
	return "", "", lastErr
}

func (s *astrbotAPIService) exportAlbumLyrics(albumID string, storefront string, format string) ([]string, int, error) {
	if strings.TrimSpace(albumID) == "" {
		return nil, 0, fmt.Errorf("album id is empty")
	}
	resp, err := ampapi.GetAlbumResp(storefront, albumID, Config.Language, s.appleToken)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to load album: %w", err)
	}
	if resp == nil || len(resp.Data) == 0 {
		return nil, 0, fmt.Errorf("album not found")
	}
	album := resp.Data[0]
	albumDir, err := os.MkdirTemp(s.artifactRoot, "lyrics-album-*")
	if err != nil {
		return nil, 0, err
	}
	usedNames := make(map[string]struct{})
	paths := []string{}
	failed := 0
	for idx, track := range album.Relationships.Tracks.Data {
		if !strings.EqualFold(track.Type, "songs") || strings.TrimSpace(track.ID) == "" {
			continue
		}
		content, _, lerr := s.fetchLyricsOnly(track.ID, storefront, format)
		if lerr != nil || strings.TrimSpace(content) == "" {
			failed++
			continue
		}
		baseName := sanitizeFileBaseName(composeArtistTitle(track.Attributes.ArtistName, track.Attributes.Name))
		if baseName == "" {
			baseName = "track-" + track.ID
		}
		order := track.Attributes.TrackNumber
		if order <= 0 {
			order = idx + 1
		}
		fileName := fmt.Sprintf("%02d. %s.lyrics.%s", order, baseName, format)
		fileName = uniqueName(usedNames, fileName)
		fullPath := filepath.Join(albumDir, fileName)
		if werr := os.WriteFile(fullPath, []byte(content), 0644); werr != nil {
			failed++
			continue
		}
		paths = append(paths, fullPath)
	}
	if len(paths) == 0 {
		_ = os.RemoveAll(albumDir)
		return nil, failed, fmt.Errorf("no lyrics exported")
	}
	sort.Strings(paths)
	return paths, failed, nil
}

func (s *astrbotAPIService) fetchArtwork(target *AppleURLTarget) (artworkFetchResult, error) {
	if target == nil {
		return artworkFetchResult{}, fmt.Errorf("invalid target")
	}
	storefront := resolveStorefront(target)
	switch target.MediaType {
	case mediaTypeSong:
		resp, err := ampapi.GetSongResp(storefront, target.ID, Config.Language, s.appleToken)
		if err != nil {
			return artworkFetchResult{}, err
		}
		if len(resp.Data) == 0 {
			return artworkFetchResult{}, fmt.Errorf("song not found")
		}
		item := resp.Data[0]
		result := artworkFetchResult{
			DisplayName: composeArtistTitle(item.Attributes.ArtistName, item.Attributes.Name),
			CoverURL:    strings.TrimSpace(item.Attributes.Artwork.URL),
		}
		if albums := item.Relationships.Albums.Data; len(albums) > 0 {
			albumID := strings.TrimSpace(albums[0].ID)
			if albumID != "" {
				if albumResp, err := ampapi.GetAlbumResp(storefront, albumID, Config.Language, s.appleToken); err == nil && len(albumResp.Data) > 0 {
					result.MotionURL = firstNonEmpty(
						albumResp.Data[0].Attributes.EditorialVideo.MotionSquare.Video,
						albumResp.Data[0].Attributes.EditorialVideo.MotionTall.Video,
					)
				}
			}
		}
		if result.DisplayName == "" {
			result.DisplayName = "song-" + target.ID
		}
		if result.CoverURL == "" {
			return artworkFetchResult{}, fmt.Errorf("song cover unavailable")
		}
		return result, nil
	case mediaTypeAlbum:
		resp, err := ampapi.GetAlbumResp(storefront, target.ID, Config.Language, s.appleToken)
		if err != nil {
			return artworkFetchResult{}, err
		}
		if len(resp.Data) == 0 {
			return artworkFetchResult{}, fmt.Errorf("album not found")
		}
		item := resp.Data[0]
		result := artworkFetchResult{
			DisplayName: composeArtistTitle(item.Attributes.ArtistName, item.Attributes.Name),
			CoverURL:    strings.TrimSpace(item.Attributes.Artwork.URL),
			MotionURL: firstNonEmpty(
				item.Attributes.EditorialVideo.MotionSquare.Video,
				item.Attributes.EditorialVideo.MotionTall.Video,
			),
		}
		if result.DisplayName == "" {
			result.DisplayName = "album-" + target.ID
		}
		if result.CoverURL == "" {
			return artworkFetchResult{}, fmt.Errorf("album cover unavailable")
		}
		return result, nil
	case mediaTypePlaylist:
		resp, err := ampapi.GetPlaylistResp(storefront, target.ID, Config.Language, s.appleToken)
		if err != nil {
			return artworkFetchResult{}, err
		}
		if len(resp.Data) == 0 {
			return artworkFetchResult{}, fmt.Errorf("playlist not found")
		}
		item := resp.Data[0]
		result := artworkFetchResult{
			DisplayName: strings.TrimSpace(item.Attributes.Name),
			CoverURL:    strings.TrimSpace(item.Attributes.Artwork.URL),
			MotionURL: firstNonEmpty(
				item.Attributes.EditorialVideo.MotionSquare.Video,
				item.Attributes.EditorialVideo.MotionTall.Video,
			),
		}
		if result.DisplayName == "" {
			result.DisplayName = "playlist-" + target.ID
		}
		if result.CoverURL == "" {
			return artworkFetchResult{}, fmt.Errorf("playlist cover unavailable")
		}
		return result, nil
	case mediaTypeStation:
		resp, err := ampapi.GetStationResp(storefront, target.ID, Config.Language, s.appleToken)
		if err != nil {
			return artworkFetchResult{}, err
		}
		if len(resp.Data) == 0 {
			return artworkFetchResult{}, fmt.Errorf("station not found")
		}
		item := resp.Data[0]
		result := artworkFetchResult{
			DisplayName: strings.TrimSpace(item.Attributes.Name),
			CoverURL:    strings.TrimSpace(item.Attributes.Artwork.URL),
			MotionURL: firstNonEmpty(
				item.Attributes.EditorialVideo.MotionSquare.Video,
				item.Attributes.EditorialVideo.MotionTall.Video,
			),
		}
		if result.DisplayName == "" {
			result.DisplayName = "station-" + target.ID
		}
		if result.CoverURL == "" {
			return artworkFetchResult{}, fmt.Errorf("station cover unavailable")
		}
		return result, nil
	case mediaTypeMusicVideo:
		resp, err := ampapi.GetMusicVideoResp(storefront, target.ID, Config.Language, s.appleToken)
		if err != nil {
			return artworkFetchResult{}, err
		}
		if len(resp.Data) == 0 {
			return artworkFetchResult{}, fmt.Errorf("music video not found")
		}
		item := resp.Data[0]
		result := artworkFetchResult{
			DisplayName: composeArtistTitle(item.Attributes.ArtistName, item.Attributes.Name),
			CoverURL:    strings.TrimSpace(item.Attributes.Artwork.URL),
		}
		if result.DisplayName == "" {
			result.DisplayName = "music-video-" + target.ID
		}
		if result.CoverURL == "" {
			return artworkFetchResult{}, fmt.Errorf("music video cover unavailable")
		}
		return result, nil
	case mediaTypeArtist:
		name, coverURL, err := s.fetchArtistProfile(storefront, target.ID)
		if err != nil {
			return artworkFetchResult{}, err
		}
		if name == "" {
			name = "artist-" + target.ID
		}
		return artworkFetchResult{DisplayName: name, CoverURL: coverURL}, nil
	default:
		return artworkFetchResult{}, fmt.Errorf("unsupported media type: %s", target.MediaType)
	}
}

func (s *astrbotAPIService) fetchArtistProfile(storefront string, artistID string) (string, string, error) {
	req, err := http.NewRequest("GET", fmt.Sprintf("https://amp-api.music.apple.com/v1/catalog/%s/artists/%s", storefront, artistID), nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", s.appleToken))
	req.Header.Set("User-Agent", "Mozilla/5.0")
	req.Header.Set("Origin", "https://music.apple.com")
	query := req.URL.Query()
	if strings.TrimSpace(Config.Language) != "" {
		query.Set("l", Config.Language)
	}
	req.URL.RawQuery = query.Encode()
	resp, err := astrbotAPIHTTPClient.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("artist request failed: %s", resp.Status)
	}
	data := artistProfileResponse{}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return "", "", err
	}
	if len(data.Data) == 0 {
		return "", "", fmt.Errorf("artist not found")
	}
	name := strings.TrimSpace(data.Data[0].Attributes.Name)
	coverURL := strings.TrimSpace(data.Data[0].Attributes.Artwork.URL)
	if coverURL == "" {
		return name, "", fmt.Errorf("artist profile photo unavailable")
	}
	return name, coverURL, nil
}

func resolveTargetFromRequest(rawURL string, mediaType string, mediaID string, storefront string) (*AppleURLTarget, error) {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL != "" {
		if parsed, err := parseAppleMusicURL(rawURL); err == nil {
			if storefront != "" {
				parsed.Storefront = storefront
			}
			return parsed, nil
		}
		if extracted := extractFirstAppleMusicURL(rawURL); extracted != "" {
			parsed, err := parseAppleMusicURL(extracted)
			if err == nil {
				if storefront != "" {
					parsed.Storefront = storefront
				}
				return parsed, nil
			}
		}
	}
	mediaType = normalizeCommandMediaType(mediaType)
	if mediaType == "" {
		return nil, fmt.Errorf("invalid media_type")
	}
	mediaID = strings.TrimSpace(mediaID)
	if mediaID == "" {
		return nil, fmt.Errorf("id is required")
	}
	if storefront == "" {
		storefront = normalizeStorefront("")
	}
	return &AppleURLTarget{MediaType: mediaType, ID: mediaID, Storefront: storefront}, nil
}

func normalizeStorefront(storefront string) string {
	storefront = strings.TrimSpace(strings.ToLower(storefront))
	if len(storefront) == 2 {
		return storefront
	}
	if len(Config.Storefront) == 2 {
		return strings.ToLower(Config.Storefront)
	}
	return "us"
}

func normalizeLanguage(language string) string {
	language = strings.TrimSpace(language)
	if language != "" {
		return language
	}
	if strings.TrimSpace(Config.TelegramSearchLanguage) != "" {
		return strings.TrimSpace(Config.TelegramSearchLanguage)
	}
	return strings.TrimSpace(Config.Language)
}

func itemTypeToMediaType(itemType string) string {
	switch strings.ToLower(strings.TrimSpace(itemType)) {
	case "song", "songs":
		return mediaTypeSong
	case "album", "albums":
		return mediaTypeAlbum
	case "artist", "artists":
		return mediaTypeArtist
	case "music video", "music videos", "music-video", "music-videos":
		return mediaTypeMusicVideo
	case "playlist", "playlists":
		return mediaTypePlaylist
	case "station", "stations":
		return mediaTypeStation
	default:
		return ""
	}
}

func buildOutputFile(path string, temporary bool) (astrbotOutputFile, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		absPath = path
	}
	info, err := os.Stat(absPath)
	if err != nil {
		return astrbotOutputFile{}, err
	}
	item := astrbotOutputFile{
		Path:      absPath,
		Name:      filepath.Base(absPath),
		Size:      info.Size(),
		Kind:      detectFileKind(absPath),
		Temporary: temporary,
	}
	if meta, ok := getDownloadedMeta(absPath); ok {
		item.TrackID = meta.TrackID
		item.Title = meta.Title
		item.Performer = meta.Performer
		item.DurationMillis = meta.DurationMillis
	}
	return item, nil
}

func detectFileKind(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".m4a", ".flac", ".mp3", ".aac", ".wav", ".opus":
		return "audio"
	case ".mp4", ".m4v", ".mov":
		return "video"
	case ".jpg", ".jpeg", ".png", ".webp", ".gif":
		return "image"
	case ".lrc", ".ttml", ".txt":
		return "lyrics"
	case ".zip":
		return "archive"
	default:
		return "file"
	}
}

func (s *astrbotAPIService) persistArtifactFile(srcPath string, displayName string) (string, error) {
	displayName = sanitizeFileBaseName(displayName)
	ext := strings.ToLower(filepath.Ext(srcPath))
	if filepath.Ext(displayName) == "" && ext != "" {
		displayName += ext
	}
	if displayName == "" {
		displayName = fmt.Sprintf("artifact-%d%s", time.Now().UnixMilli(), ext)
	}
	targetPath := filepath.Join(s.artifactRoot, displayName)
	if _, err := os.Stat(targetPath); err == nil {
		targetPath = filepath.Join(s.artifactRoot, fmt.Sprintf("%d-%s", time.Now().UnixNano(), displayName))
	}
	if err := copyFile(srcPath, targetPath); err != nil {
		return "", err
	}
	return targetPath, nil
}

func (s *astrbotAPIService) writeArtifactTextFile(displayName string, content string) (string, error) {
	displayName = sanitizeFileBaseName(displayName)
	if displayName == "" {
		displayName = fmt.Sprintf("lyrics-%d.txt", time.Now().UnixMilli())
	}
	if filepath.Ext(displayName) == "" {
		displayName += ".txt"
	}
	targetPath := filepath.Join(s.artifactRoot, displayName)
	if _, err := os.Stat(targetPath); err == nil {
		targetPath = filepath.Join(s.artifactRoot, fmt.Sprintf("%d-%s", time.Now().UnixNano(), displayName))
	}
	if err := os.WriteFile(targetPath, []byte(content), 0644); err != nil {
		return "", err
	}
	return targetPath, nil
}

func copyFile(srcPath string, dstPath string) error {
	src, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer src.Close()
	if err := os.MkdirAll(filepath.Dir(dstPath), 0755); err != nil {
		return err
	}
	dst, err := os.Create(dstPath)
	if err != nil {
		return err
	}
	defer dst.Close()
	if _, err := io.Copy(dst, src); err != nil {
		return err
	}
	return dst.Close()
}

func (s *astrbotAPIService) cleanupArtifacts(maxAge time.Duration) {
	if maxAge <= 0 {
		return
	}
	entries, err := os.ReadDir(s.artifactRoot)
	if err != nil {
		return
	}
	now := time.Now()
	for _, entry := range entries {
		path := filepath.Join(s.artifactRoot, entry.Name())
		info, ierr := entry.Info()
		if ierr != nil {
			continue
		}
		if now.Sub(info.ModTime()) <= maxAge {
			continue
		}
		_ = os.RemoveAll(path)
	}
}

func boolPtr(v bool) *bool {
	return &v
}

func decodeJSON(body io.ReadCloser, dst any) error {
	defer body.Close()
	decoder := json.NewDecoder(io.LimitReader(body, 5*1024*1024))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return fmt.Errorf("invalid json: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return fmt.Errorf("invalid json: multiple json values")
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(payload); err != nil {
		fmt.Printf("AstrBot API write json error: %v\n", err)
	}
}
