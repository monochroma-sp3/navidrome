package tidal

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/navidrome/navidrome/log"
	"github.com/navidrome/navidrome/model"
)

// SyncSearch searches Tidal for the given query and syncs the results into the local database
// so that subsequent Subsonic database queries will return the results seamlessly.
func SyncSearch(ctx context.Context, ds model.DataStore, client *Client, query string, limit int) error {
	return SyncSearchWithLibrary(ctx, ds, client, query, limit, 0)
}

// SyncSearchWithLibrary is the same as SyncSearch but lets the caller pass a
// pre-resolved Tidal library ID, avoiding a duplicate lookup when searchAll
// already needs the ID itself.
func SyncSearchWithLibrary(ctx context.Context, ds model.DataStore, client *Client, query string, limit int, libraryID int) error {
	cleanQuery := strings.Trim(query, " \"'")
	if cleanQuery == "" {
		return nil
	}

	if limit > 50 {
		limit = 50
	}

	if libraryID == 0 {
		id, err := GetOrCreateTidalLibrary(ctx, ds)
		if err != nil {
			return fmt.Errorf("failed to get/create tidal library: %w", err)
		}
		libraryID = id
	}

	// Fire all three searches (tracks/artists/albums) in parallel so queries
	// by artist name or album title surface direct results, not just tracks
	// that happen to match. Running them sequentially was the main latency
	// cost on JIT search — three round-trips to hifi-api instead of one.
	var (
		tracksResp                *SearchResponse
		artistsResp               *SearchResponse
		albumsResp                *SearchResponse
		tracksErr, artistsErr, albumsErr error
		wg                        sync.WaitGroup
	)
	wg.Add(3)
	go func() {
		defer wg.Done()
		tracksResp, tracksErr = client.SearchTracks(ctx, cleanQuery, limit, 0)
	}()
	go func() {
		defer wg.Done()
		artistsResp, artistsErr = client.SearchArtists(ctx, cleanQuery, limit, 0)
	}()
	go func() {
		defer wg.Done()
		albumsResp, albumsErr = client.SearchAlbums(ctx, cleanQuery, limit, 0)
	}()
	wg.Wait()
	if tracksErr != nil {
		log.Error(ctx, "Tidal track search failed", "query", cleanQuery, tracksErr)
	}
	if artistsErr != nil {
		log.Error(ctx, "Tidal artist search failed", "query", cleanQuery, artistsErr)
	}
	if albumsErr != nil {
		log.Error(ctx, "Tidal album search failed", "query", cleanQuery, albumsErr)
	}

	// Direct artist hits
	if artistsResp != nil && artistsResp.Data.Artists != nil {
		for _, a := range artistsResp.Data.Artists.Items {
			ref := TidalArtistRef{ID: a.ID, Name: a.Name, Picture: a.Picture}
			if err := EnsureTidalArtist(ctx, ds, ref, libraryID); err != nil {
				log.Error(ctx, "Failed to save searched Tidal artist", "name", a.Name, err)
			}
		}
	}

	// Direct album hits (full TidalAlbum → richer than the album ref from tracks)
	if albumsResp != nil && albumsResp.Data.Albums != nil {
		for _, al := range albumsResp.Data.Albums.Items {
			if al.Artist.ID != 0 {
				ref := TidalArtistRef{ID: al.Artist.ID, Name: al.Artist.Name, Picture: al.Artist.Picture}
				if err := EnsureTidalArtist(ctx, ds, ref, libraryID); err != nil {
					log.Error(ctx, "Failed to save album's artist", "artist", al.Artist.Name, err)
				}
			}
			if err := ensureTidalAlbumFull(ctx, ds, al, libraryID); err != nil {
				log.Error(ctx, "Failed to save searched Tidal album", "title", al.Title, err)
			}
		}
	}

	if tracksResp == nil || len(tracksResp.Data.Items) == 0 {
		if imp := (artistsResp != nil && artistsResp.Data.Artists != nil && len(artistsResp.Data.Artists.Items) > 0) ||
			(albumsResp != nil && albumsResp.Data.Albums != nil && len(albumsResp.Data.Albums.Items) > 0); imp {
			refreshArtistStatsAsync(ds)
		}
		return nil
	}

	imported := 0
	for _, track := range tracksResp.Data.Items {
		// Ensure artist exists
		if track.Artist.ID != 0 {
			if err := EnsureTidalArtist(ctx, ds, track.Artist, libraryID); err != nil {
				log.Error(ctx, "Failed to ensure artist for sync search", "artist", track.Artist.Name, err)
			}
		}

		// Ensure album exists
		albumID := TidalAlbumID(track.Album.ID)
		if track.Album.ID != 0 {
			if err := EnsureTidalAlbumFromRef(ctx, ds, track.Album, track.Artist, libraryID); err != nil {
				log.Error(ctx, "Failed to ensure album for sync search", "album", track.Album.Title, err)
			}
		}

		// Save track
		mf := TrackToMediaFile(track, albumID, libraryID)
		mf.ExternalSource = ExternalSourceTidal
		mf.ExternalID = strconv.Itoa(track.ID)

		if err := ds.MediaFile(ctx).Put(&mf); err != nil {
			log.Error(ctx, "Failed to save Tidal track during sync", "trackID", track.ID, err)
			continue
		}
		// Remember Tidal popularity so searchAll can rerank results by
		// prevalence without a DB schema change.
		RememberPopularity(mf.ID, track.Popularity)
		imported++
	}

	if imported > 0 {
		refreshArtistStatsAsync(ds)
	}

	log.Debug(ctx, "JIT Tidal sync completed", "query", query, "imported", imported)
	return nil
}

// refreshArtistStatsAsync kicks the artist stats refresh off to a goroutine
// with a fresh background context. Previously this ran inline at the tail
// of SyncSearch, blocking the JIT path on a full table scan; the stats only
// affect things like album counts shown in the browse UI, which tolerate a
// brief lag just fine.
func refreshArtistStatsAsync(ds model.DataStore) {
	go func() {
		ctx := context.Background()
		if _, err := ds.Artist(ctx).RefreshStats(false); err != nil {
			log.Error(ctx, "Failed to refresh artist stats after JIT sync", err)
		}
	}()
}

// SyncAlbum fetches a specific album and all its tracks from Tidal, and syncs them to the database.
func SyncAlbum(ctx context.Context, ds model.DataStore, client *Client, tidalID int) error {
	libraryID, err := GetOrCreateTidalLibrary(ctx, ds)
	if err != nil {
		return fmt.Errorf("failed to get/create tidal library: %w", err)
	}

	albumResp, err := client.GetAlbum(ctx, tidalID)
	if err != nil {
		return fmt.Errorf("tidal album fetch failed: %w", err)
	}

	al := albumResp.Data
	if al.ID == 0 {
		return fmt.Errorf("tidal album %d not found", tidalID)
	}

	// Ensure artist exists
	if al.Artist.ID != 0 {
		ref := TidalArtistRef{ID: al.Artist.ID, Name: al.Artist.Name, Picture: al.Artist.Picture}
		if err := EnsureTidalArtist(ctx, ds, ref, libraryID); err != nil {
			log.Error(ctx, "Failed to save album's artist", "artist", al.Artist.Name, err)
		}
	}

	// Ensure album exists
	if err := ensureTidalAlbumFull(ctx, ds, al, libraryID); err != nil {
		log.Error(ctx, "Failed to save searched Tidal album", "title", al.Title, err)
	}

	imported := 0
	albumID := TidalAlbumID(al.ID)
	for _, item := range al.Items {
		track := item.Item
		if track.ID == 0 {
			continue
		}
		
		if track.Artist.ID != 0 {
			if err := EnsureTidalArtist(ctx, ds, track.Artist, libraryID); err != nil {
				log.Error(ctx, "Failed to ensure artist for sync album", "artist", track.Artist.Name, err)
			}
		}
		
		// Some tracks in album list don't have album info, populate it
		track.Album.ID = al.ID
		track.Album.Title = al.Title
		track.Album.Cover = al.Cover

		mf := TrackToMediaFile(track, albumID, libraryID)
		mf.ExternalSource = ExternalSourceTidal
		mf.ExternalID = strconv.Itoa(track.ID)

		if err := ds.MediaFile(ctx).Put(&mf); err != nil {
			if strings.Contains(err.Error(), "UNIQUE constraint failed") {
				continue
			}
			log.Error(ctx, "Failed to save Tidal track during album sync", "trackID", track.ID, err)
			continue
		}
		imported++
	}

	if imported > 0 {
		if _, err := ds.Artist(ctx).RefreshStats(false); err != nil {
			log.Error(ctx, "Failed to refresh artist stats after JIT sync", err)
		}
	}

	log.Debug(ctx, "JIT Tidal album sync completed", "album", al.Title, "imported", imported)
	return nil
}

// GetOrCreateTidalLibrary finds or creates the virtual Tidal library in the database.
// Also guarantees that a root virtual folder exists for the library.
func GetOrCreateTidalLibrary(ctx context.Context, ds model.DataStore) (int, error) {
	libs, err := ds.Library(ctx).GetAll()
	if err != nil {
		return 0, err
	}

	var libID int
	for _, lib := range libs {
		if lib.Name == "Tidal" || lib.Path == "tidal://virtual" {
			libID = lib.ID
			break
		}
	}

	if libID == 0 {
		lib := &model.Library{
			Name:            "Tidal",
			Path:            "tidal://virtual",
			DefaultNewUsers: true,
		}
		if err := ds.Library(ctx).Put(lib); err != nil {
			return 0, fmt.Errorf("creating Tidal library: %w", err)
		}
		log.Info(ctx, "Created virtual Tidal library", "id", lib.ID)
		libID = lib.ID
	}

	if err := ensureTidalFolder(ctx, ds, libID); err != nil {
		log.Error(ctx, "Failed to ensure Tidal virtual folder", "libraryID", libID, err)
	}
	return libID, nil
}

// TidalFolderID returns the stable folder ID for the Tidal virtual root folder.
func TidalFolderID(libraryID int) string {
	return model.FolderID(model.Library{ID: libraryID, Path: "tidal://virtual"}, ".")
}

// ensureTidalFolder makes sure a single virtual root folder exists for the Tidal library.
// Subsonic/browse queries that JOIN media_file → folder expect a non-missing folder row;
// without it, search/browse hide Tidal tracks.
func ensureTidalFolder(ctx context.Context, ds model.DataStore, libraryID int) error {
	lib := model.Library{ID: libraryID, Path: "tidal://virtual"}
	f := model.NewFolder(lib, ".")
	f.Missing = false
	if err := ds.Folder(ctx).Put(f); err != nil {
		return fmt.Errorf("creating tidal root folder: %w", err)
	}
	return nil
}

// EnsureTidalArtist ensures a Tidal artist exists in the database.
func EnsureTidalArtist(ctx context.Context, ds model.DataStore, ref TidalArtistRef, libraryID int) error {
	artistID := TidalArtistID(ref.ID)

	// Check if already exists
	exists, err := ds.Artist(ctx).Exists(artistID)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}

	artist := &model.Artist{
		ID:             artistID,
		Name:           ref.Name,
		ExternalSource: ExternalSourceTidal,
		ExternalID:     strconv.Itoa(ref.ID),
	}

	// Set artist image if available
	if ref.Picture != "" {
		artist.MediumImageUrl = TidalArtistImageURL(ref.Picture, 750)
	}

	err = ds.Artist(ctx).Put(artist)
	if err != nil {
		// Concurrent JIT searches on /song, /album, /artist can all try to
		// upsert the same artist — Exists returns false for both, then one
		// of the Puts loses the UNIQUE race. Treat that as success.
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return nil
		}
		return fmt.Errorf("saving Tidal artist %q: %w", ref.Name, err)
	}

	// Associate artist with the Tidal library
	err = ds.Library(ctx).AddArtist(libraryID, artistID)
	if err != nil {
		log.Error(ctx, "Failed to add artist to Tidal library", "artistID", artistID, err)
	}

	return nil
}

// ensureTidalAlbumFull saves a full TidalAlbum (richer metadata than the ref variant:
// release date, cover, participants, etc.). Idempotent: later calls update the row via Put.
func ensureTidalAlbumFull(ctx context.Context, ds model.DataStore, a TidalAlbum, libraryID int) error {
	album := AlbumToModel(a, libraryID)
	album.ExternalSource = ExternalSourceTidal
	album.ExternalID = strconv.Itoa(a.ID)
	return ds.Album(ctx).Put(&album)
}

// EnsureTidalAlbumFromRef ensures a Tidal album exists in the DB (from a TidalAlbumRef).
func EnsureTidalAlbumFromRef(ctx context.Context, ds model.DataStore, ref TidalAlbumRef, artistRef TidalArtistRef, libraryID int) error {
	albumID := TidalAlbumID(ref.ID)

	exists, err := ds.Album(ctx).Exists(albumID)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}

	album := &model.Album{
		ID:             albumID,
		LibraryID:      libraryID,
		Name:           ref.Title,
		AlbumArtist:    artistRef.Name,
		AlbumArtistID:  TidalArtistID(artistRef.ID),
		ExternalSource: ExternalSourceTidal,
		ExternalID:     strconv.Itoa(ref.ID),
	}

	// Set cover art URLs
	if ref.Cover != "" {
		album.SmallImageUrl = TidalCoverURL(ref.Cover, 80)
		album.MediumImageUrl = TidalCoverURL(ref.Cover, 640)
		album.LargeImageUrl = TidalCoverURL(ref.Cover, 1280)
	}

	if err := ds.Album(ctx).Put(album); err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return nil
		}
		return err
	}
	return nil
}
