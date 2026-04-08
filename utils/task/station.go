package task

import (
	//"bufio"
	"errors"
	"fmt"

	//"os"
	//"strconv"
	//"strings"

	//"github.com/fatih/color"
	//"github.com/olekukonko/tablewriter"

	"github.com/wuuduf/astrbot-applemusic-service/utils/ampapi"
	"github.com/wuuduf/astrbot-applemusic-service/utils/safe"
)

type Station struct {
	Storefront string
	ID         string

	SaveDir   string
	SaveName  string
	Codec     string
	CoverPath string

	Language string
	Resp     ampapi.StationResp
	Type     string
	Name     string
	Tracks   []Track
}

func NewStation(st string, id string) *Station {
	a := new(Station)
	a.Storefront = st
	a.ID = id
	//fmt.Println("Album created")
	return a

}

func (a *Station) GetResp(mutoken, token, l string) error {
	var err error
	a.Language = l
	resp, err := ampapi.GetStationResp(a.Storefront, a.ID, a.Language, token)
	if err != nil {
		return errors.New("error getting station response")
	}
	a.Resp = *resp
	stationData, err := safe.FirstRef("task.Station.GetResp", "station.data", a.Resp.Data)
	if err != nil {
		return err
	}
	//简化高频调用名称
	a.Type = stationData.Attributes.PlayParams.Format
	a.Name = stationData.Attributes.Name
	if a.Type != "tracks" {
		return nil
	}
	tracksResp, err := ampapi.GetStationNextTracks(a.ID, mutoken, a.Language, token)
	if err != nil {
		return errors.New("error getting station tracks response")
	}
	//fmt.Println("Getting album response")
	//从resp中的Tracks数据中提取trackData信息到新的Track结构体中
	for i, trackData := range tracksResp.Data {
		albumResp, err := ampapi.GetAlbumRespByHref(trackData.Href, a.Language, token)
		if err != nil {
			fmt.Println("Error getting album response:", err)
			continue
		}
		albumData, dataErr := safe.FirstRef("task.Station.GetResp", "station.track.album.data", albumResp.Data)
		if dataErr != nil {
			fmt.Println("Error parsing album response:", dataErr)
			continue
		}
		albumLen := len(albumData.Relationships.Tracks.Data)
		discTotal := 0
		if albumLen > 0 {
			discTotal = albumData.Relationships.Tracks.Data[albumLen-1].Attributes.DiscNumber
		}
		a.Tracks = append(a.Tracks, Track{
			ID:         trackData.ID,
			Type:       trackData.Type,
			Name:       trackData.Attributes.Name,
			Language:   a.Language,
			Storefront: a.Storefront,

			//SaveDir:   filepath.Join(a.SaveDir, a.SaveName),
			//Codec:     a.Codec,
			TaskNum:   i + 1,
			TaskTotal: len(tracksResp.Data),
			M3u8:      trackData.Attributes.ExtendedAssetUrls.EnhancedHls,
			WebM3u8:   trackData.Attributes.ExtendedAssetUrls.EnhancedHls,
			//CoverPath: a.CoverPath,

			Resp:      trackData,
			PreType:   "stations",
			DiscTotal: discTotal,
			PreID:     a.ID,
			AlbumData: buildTrackAlbumData(albumData),
		})
		a.Tracks[i].PlaylistData = TrackPlaylistData{
			Name:       a.Name,
			ArtistName: "Apple Music Station",
		}
	}
	return nil
}

func (a *Station) GetArtwork() string {
	if len(a.Resp.Data) == 0 {
		return ""
	}
	return a.Resp.Data[0].Attributes.Artwork.URL
}
