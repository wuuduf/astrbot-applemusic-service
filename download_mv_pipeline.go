package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/wuuduf/astrbot-applemusic-service/utils/ampapi"
	"github.com/wuuduf/astrbot-applemusic-service/utils/runv3"
	"github.com/wuuduf/astrbot-applemusic-service/utils/structs"
	"github.com/wuuduf/astrbot-applemusic-service/utils/task"
)

type musicVideoDownloadContext struct {
	session        *DownloadSession
	cfg            *structs.ConfigSet
	token          string
	storefront     string
	mediaUserToken string
	track          *task.Track
	mediaID        string
	saveDir        string
	resp           *ampapi.MusicVideoRespData
	genre          string
	mvSaveName     string
	videoPath      string
	audioPath      string
	outputPath     string
	coverPath      string
}

func mvDownloader(session *DownloadSession, adamID string, saveDir string, token string, storefront string, mediaUserToken string, track *task.Track) error {
	ctx, err := resolveMusicVideoMediaStage(session, adamID, saveDir, token, storefront, mediaUserToken, track)
	if err != nil {
		return err
	}
	if handleExistingMusicVideoStage(ctx) {
		return nil
	}
	if err := prepareMusicVideoWorkspaceStage(ctx); err != nil {
		return err
	}
	defer os.Remove(ctx.videoPath)
	defer os.Remove(ctx.audioPath)
	defer os.Remove(ctx.coverPath)
	if err := downloadMusicVideoMediaStage(ctx); err != nil {
		return err
	}
	return postProcessMusicVideoStage(ctx)
}

func resolveMusicVideoMediaStage(session *DownloadSession, adamID string, saveDir string, token string, storefront string, mediaUserToken string, track *task.Track) (*musicVideoDownloadContext, error) {
	if session == nil {
		return nil, fmt.Errorf("download session is nil")
	}
	cfg := &session.Config
	mvInfo, err := ampapi.GetMusicVideoResp(storefront, adamID, cfg.Language, token)
	if err != nil {
		return nil, fmt.Errorf("failed to get MV manifest: %w", err)
	}
	mvData, err := firstMusicVideoData("download.resolveMusicVideoMedia", mvInfo)
	if err != nil {
		return nil, err
	}
	mvGenre, err := firstGenreName("download.resolveMusicVideoMedia", mvData.Attributes.GenreNames)
	if err != nil {
		return nil, err
	}
	if strings.HasSuffix(saveDir, ".") {
		saveDir = strings.ReplaceAll(saveDir, ".", "")
	}
	saveDir = strings.TrimSpace(saveDir)
	mvSaveName := fmt.Sprintf("%s (%s)", mvData.Attributes.Name, adamID)
	if track != nil {
		mvSaveName = fmt.Sprintf("%02d. %s", track.TaskNum, mvData.Attributes.Name)
	}
	return &musicVideoDownloadContext{
		session:        session,
		cfg:            cfg,
		token:          token,
		storefront:     storefront,
		mediaUserToken: mediaUserToken,
		track:          track,
		mediaID:        adamID,
		saveDir:        saveDir,
		resp:           mvData,
		genre:          mvGenre,
		mvSaveName:     mvSaveName,
		videoPath:      filepath.Join(saveDir, fmt.Sprintf("%s_vid.mp4", adamID)),
		audioPath:      filepath.Join(saveDir, fmt.Sprintf("%s_aud.mp4", adamID)),
		outputPath:     filepath.Join(saveDir, fmt.Sprintf("%s.mp4", forbiddenNames.ReplaceAllString(mvSaveName, "_"))),
	}, nil
}

func handleExistingMusicVideoStage(ctx *musicVideoDownloadContext) bool {
	fmt.Println(ctx.resp.Attributes.Name)
	exists := false
	if ctx.session.shouldReuseExistingFiles() {
		exists, _ = fileExists(ctx.outputPath)
	}
	if !exists {
		return false
	}
	fmt.Println("MV already exists locally.")
	meta := AudioMeta{
		TrackID:        ctx.mediaID,
		Title:          strings.TrimSpace(ctx.resp.Attributes.Name),
		Performer:      strings.TrimSpace(ctx.resp.Attributes.ArtistName),
		DurationMillis: int64(ctx.resp.Attributes.DurationInMillis),
	}
	if ctx.track != nil {
		ctx.track.SavePath = ctx.outputPath
		ctx.track.SaveName = filepath.Base(ctx.outputPath)
	}
	ctx.session.recordDownloadedFile(ctx.outputPath, meta)
	return true
}

func prepareMusicVideoWorkspaceStage(ctx *musicVideoDownloadContext) error {
	if err := os.MkdirAll(ctx.saveDir, os.ModePerm); err != nil {
		return fmt.Errorf("failed to create mv save directory: %w", err)
	}
	return nil
}

func downloadMusicVideoMediaStage(ctx *musicVideoDownloadContext) error {
	mvm3u8URL, _, _, err := runv3.GetWebplayback(ctx.mediaID, ctx.token, ctx.mediaUserToken, true)
	if err != nil {
		return fmt.Errorf("failed to get MV playback info: %w", err)
	}
	if strings.TrimSpace(mvm3u8URL) == "" {
		return errors.New("media-user-token may be wrong or expired")
	}

	videoM3U8URL, err := extractVideoWithConfig(mvm3u8URL, *ctx.cfg)
	if err != nil {
		return fmt.Errorf("failed to resolve MV video stream: %w", err)
	}
	if strings.TrimSpace(videoM3U8URL) == "" {
		return errors.New("failed to resolve MV video stream")
	}
	videoKeyAndURLs, err := runv3.Run(ctx.mediaID, videoM3U8URL, ctx.token, ctx.mediaUserToken, true, "", nil)
	if err != nil {
		return fmt.Errorf("failed to fetch MV video segments: %w", err)
	}
	if strings.TrimSpace(videoKeyAndURLs) == "" {
		return errors.New("mv video key payload is empty")
	}
	if err := runv3.ExtMvData(videoKeyAndURLs, ctx.videoPath); err != nil {
		return fmt.Errorf("failed to save MV video track: %w", err)
	}

	audioM3U8URL, err := extractMvAudioWithConfig(mvm3u8URL, *ctx.cfg)
	if err != nil {
		return fmt.Errorf("failed to resolve MV audio stream: %w", err)
	}
	if strings.TrimSpace(audioM3U8URL) == "" {
		return errors.New("failed to resolve MV audio stream")
	}
	audioKeyAndURLs, err := runv3.Run(ctx.mediaID, audioM3U8URL, ctx.token, ctx.mediaUserToken, true, "", nil)
	if err != nil {
		return fmt.Errorf("failed to fetch MV audio segments: %w", err)
	}
	if strings.TrimSpace(audioKeyAndURLs) == "" {
		return errors.New("mv audio key payload is empty")
	}
	if err := runv3.ExtMvData(audioKeyAndURLs, ctx.audioPath); err != nil {
		return fmt.Errorf("failed to save MV audio track: %w", err)
	}
	return nil
}

func postProcessMusicVideoStage(ctx *musicVideoDownloadContext) error {
	tags := []string{
		"tool=",
		fmt.Sprintf("artist=%s", ctx.resp.Attributes.ArtistName),
		fmt.Sprintf("title=%s", ctx.resp.Attributes.Name),
		fmt.Sprintf("genre=%s", ctx.genre),
		fmt.Sprintf("created=%s", ctx.resp.Attributes.ReleaseDate),
		fmt.Sprintf("ISRC=%s", ctx.resp.Attributes.Isrc),
	}
	if ctx.resp.Attributes.ContentRating == "explicit" {
		tags = append(tags, "rating=1")
	} else if ctx.resp.Attributes.ContentRating == "clean" {
		tags = append(tags, "rating=2")
	} else {
		tags = append(tags, "rating=0")
	}

	if ctx.track != nil {
		if ctx.track.PreType == "playlists" && !ctx.cfg.UseSongInfoForPlaylist {
			tags = append(tags,
				"disk=1/1",
				fmt.Sprintf("album=%s", ctx.track.PlaylistData.Attributes.Name),
				fmt.Sprintf("track=%d", ctx.track.TaskNum),
				fmt.Sprintf("tracknum=%d/%d", ctx.track.TaskNum, ctx.track.TaskTotal),
				fmt.Sprintf("album_artist=%s", ctx.track.PlaylistData.Attributes.ArtistName),
				fmt.Sprintf("performer=%s", ctx.track.Resp.Attributes.ArtistName),
			)
		} else if ctx.track.PreType == "playlists" && ctx.cfg.UseSongInfoForPlaylist {
			tags = append(tags,
				fmt.Sprintf("album=%s", ctx.track.AlbumData.Attributes.Name),
				fmt.Sprintf("disk=%d/%d", ctx.track.Resp.Attributes.DiscNumber, ctx.track.DiscTotal),
				fmt.Sprintf("track=%d", ctx.track.Resp.Attributes.TrackNumber),
				fmt.Sprintf("tracknum=%d/%d", ctx.track.Resp.Attributes.TrackNumber, ctx.track.AlbumData.Attributes.TrackCount),
				fmt.Sprintf("album_artist=%s", ctx.track.AlbumData.Attributes.ArtistName),
				fmt.Sprintf("performer=%s", ctx.track.Resp.Attributes.ArtistName),
				fmt.Sprintf("copyright=%s", ctx.track.AlbumData.Attributes.Copyright),
				fmt.Sprintf("UPC=%s", ctx.track.AlbumData.Attributes.Upc),
			)
		} else {
			tags = append(tags,
				fmt.Sprintf("album=%s", ctx.track.AlbumData.Attributes.Name),
				fmt.Sprintf("disk=%d/%d", ctx.track.Resp.Attributes.DiscNumber, ctx.track.DiscTotal),
				fmt.Sprintf("track=%d", ctx.track.Resp.Attributes.TrackNumber),
				fmt.Sprintf("tracknum=%d/%d", ctx.track.Resp.Attributes.TrackNumber, ctx.track.AlbumData.Attributes.TrackCount),
				fmt.Sprintf("album_artist=%s", ctx.track.AlbumData.Attributes.ArtistName),
				fmt.Sprintf("performer=%s", ctx.track.Resp.Attributes.ArtistName),
				fmt.Sprintf("copyright=%s", ctx.track.AlbumData.Attributes.Copyright),
				fmt.Sprintf("UPC=%s", ctx.track.AlbumData.Attributes.Upc),
			)
		}
	} else {
		tags = append(tags,
			fmt.Sprintf("album=%s", ctx.resp.Attributes.AlbumName),
			fmt.Sprintf("disk=%d", ctx.resp.Attributes.DiscNumber),
			fmt.Sprintf("track=%d", ctx.resp.Attributes.TrackNumber),
			fmt.Sprintf("tracknum=%d", ctx.resp.Attributes.TrackNumber),
			fmt.Sprintf("performer=%s", ctx.resp.Attributes.ArtistName),
		)
	}

	thumbURL := ctx.resp.Attributes.Artwork.URL
	baseThumbName := forbiddenNames.ReplaceAllString(ctx.mvSaveName, "_") + "_thumbnail"
	covPath, err := writeCoverWithConfig(ctx.saveDir, baseThumbName, thumbURL, ctx.cfg)
	if err != nil {
		fmt.Println("Failed to save MV thumbnail:", err)
	} else {
		ctx.coverPath = covPath
		tags = append(tags, fmt.Sprintf("cover=%s", covPath))
	}

	fmt.Printf("MV Remuxing...")
	if _, err := runExternalCommand(context.Background(), "MP4Box", "-itags", strings.Join(tags, ":"), "-quiet", "-add", ctx.videoPath, "-add", ctx.audioPath, "-keep-utc", "-new", ctx.outputPath); err != nil {
		fmt.Printf("MV mux failed: %v\n", err)
		return err
	}
	fmt.Printf("\rMV Remuxed.   \n")

	meta := AudioMeta{
		TrackID:        ctx.mediaID,
		Title:          strings.TrimSpace(ctx.resp.Attributes.Name),
		Performer:      strings.TrimSpace(ctx.resp.Attributes.ArtistName),
		DurationMillis: int64(ctx.resp.Attributes.DurationInMillis),
	}
	if ctx.track != nil {
		ctx.track.SavePath = ctx.outputPath
		ctx.track.SaveName = filepath.Base(ctx.outputPath)
	}
	ctx.session.recordDownloadedFile(ctx.outputPath, meta)
	return nil
}
