package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

// ArtworkType is one kind of artwork the plugin copies from Plex to Silo.
// Movie and series posters keep their 0.5.0 code path and state (see
// applyPosters); the other types go through the jobs below.
type ArtworkType string

const (
	ArtPoster             ArtworkType = "poster"
	ArtBackdrop           ArtworkType = "backdrop"
	ArtLogo               ArtworkType = "logo"
	ArtSeasonPoster       ArtworkType = "season_poster"
	ArtCollectionBackdrop ArtworkType = "collection_backdrop"
)

// siloType is the "type" field of Silo's images/upload form.
func (t ArtworkType) siloType() string {
	switch t {
	case ArtBackdrop, ArtCollectionBackdrop:
		return "backdrop"
	case ArtLogo:
		return "logo"
	default:
		return "poster"
	}
}

// ArtworkJob is one image to copy: where it comes from in Plex, where it goes
// in Silo, and the fingerprint recorded once Silo has accepted it.
type ArtworkJob struct {
	Type      ArtworkType
	Title     string // for logs and errors
	RatingKey string // the Plex item (the show, for a season poster)
	Path      string // the selected image's Plex path
	Season    int    // season number, for ArtSeasonPoster
	SiloID    string // Silo content ID (the season's, for ArtSeasonPoster)
	Owner     string // the movie or series the fingerprint names
	StateKey  string
	StateVal  string // fingerprint; "" means hash the image to decide
}

// The recorded value is the fingerprint, followed for a season poster by
// "|<season content ID>". Either way the last "|" field is the Silo item the
// image was uploaded to, which is what a revert resets.
func (j ArtworkJob) recordValue(fp string) string {
	if j.Type == ArtSeasonPoster {
		return fp + "|" + j.SiloID
	}
	return fp
}

// recorded reports whether a stored value is for fingerprint fp.
func recorded(stored, fp string) bool {
	return fp != "" && (stored == fp || strings.HasPrefix(stored, fp+"|"))
}

// uploadTarget is the Silo item a stored artwork value was uploaded to.
func uploadTarget(stored string) string {
	if i := strings.LastIndex(stored, "|"); i >= 0 {
		return stored[i+1:]
	}
	return ""
}

// artworkKey is the state key of one artwork type of one Plex item. The
// library is part of the key, so a change of winning library re-evaluates
// the item.
func artworkKey(sec plexSection, ratingKey string, t ArtworkType) string {
	return sectionID(sec) + "/" + ratingKey + "/" + string(t)
}

func seasonKey(sec plexSection, showRatingKey string, number int) string {
	return sectionID(sec) + "/" + showRatingKey + "/season/" + strconv.Itoa(number)
}

// versionedPath matches Plex image paths that end in the image's upload
// timestamp (/library/metadata/1/art/1700000000). Plex writes a new
// timestamp whenever the selected image changes, so the path alone tells
// whether the image changed.
var versionedPath = regexp.MustCompile(`^/library/(metadata|collections)/\d+/[A-Za-z]+/\d+$`)

// fingerprint is what the state records for an image: its versioned path, or
// for any other path nothing yet (the job hashes the bytes), plus the Silo ID
// so a re-keyed Silo item is pushed again.
func fingerprint(path, siloID string) string {
	if versionedPath.MatchString(path) {
		return path + "|" + siloID
	}
	return ""
}

func hashFingerprint(data []byte, siloID string) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:]) + "|" + siloID
}

// artworkCounter returns the summary field a type's applied count goes to.
func (s *runSummary) artworkCounter(t ArtworkType) *int {
	switch t {
	case ArtBackdrop:
		return &s.BackdropsApplied
	case ArtLogo:
		return &s.LogosApplied
	case ArtSeasonPoster:
		return &s.SeasonPostersApplied
	case ArtCollectionBackdrop:
		return &s.CollectionBackdropsApplied
	}
	return &s.PostersApplied
}

func (s *syncer) countArtwork(f func(*runSummary)) {
	s.mu.Lock()
	f(s.summary)
	s.mu.Unlock()
}

func (s *syncer) artworkError(t ArtworkType, format string, args ...any) {
	s.mu.Lock()
	if s.summary.ErrorsByType == nil {
		s.summary.ErrorsByType = map[string]int{}
	}
	if !isCancellation(args) {
		s.summary.ErrorsByType[string(t)]++
	}
	s.mu.Unlock()
	s.addError(string(t)+" "+format, args...)
}

// planItemArtwork lists the backdrop and logo jobs for the winning copy of
// each Silo item. Items whose artwork is unchanged since the last upload are
// counted and left out.
func (s *syncer) planItemArtwork(ctx context.Context, winners []posterCandidate) []ArtworkJob {
	var jobs []ArtworkJob
	add := func(c posterCandidate, t ArtworkType, path string) {
		key := artworkKey(c.sec, c.job.item.RatingKey, t)
		if path == "" {
			s.countArtwork(func(r *runSummary) { r.ArtworkMissing++ })
			return
		}
		val := fingerprint(path, c.job.contentID)
		if recorded(s.state.artworkVal(key), val) {
			s.countArtwork(func(r *runSummary) { r.ArtworkUnchanged++ })
			return
		}
		jobs = append(jobs, ArtworkJob{Type: t, Title: itemLabel(c.job.item), RatingKey: c.job.item.RatingKey,
			Path: path, SiloID: c.job.contentID, Owner: c.job.contentID, StateKey: key, StateVal: val})
	}
	for _, c := range winners {
		if ctx.Err() != nil {
			return jobs
		}
		if s.cfg.SyncBackdrops {
			add(c, ArtBackdrop, c.job.item.Art)
		}
		if s.cfg.SyncLogos {
			path, ok := s.selectedLogo(ctx, c.job.item)
			if ok {
				add(c, ArtLogo, path)
			}
		}
	}
	return jobs
}

// selectedLogo finds the clear logo Plex shows for an item. Listings on
// current Plex servers carry the selected artwork in the Image array; when a
// server leaves it out, /clearLogos is asked which entry is selected. ok is
// false when the selection can't be told reliably; that is reported, never
// guessed.
func (s *syncer) selectedLogo(ctx context.Context, it plexItem) (string, bool) {
	if path, listed := it.clearLogo(); listed {
		if path != "" && !isPlexPath(path) {
			s.logoUndetermined(it, "selected logo is not served by Plex")
			return "", false
		}
		return path, true
	}
	path, ok, err := s.plex.SelectedClearLogo(ctx, it.RatingKey)
	switch {
	case err != nil:
		s.artworkError(ArtLogo, "%s: read Plex logos: %v", itemLabel(it), err)
		return "", false
	case !ok:
		s.logoUndetermined(it, "Plex marks no single local logo as selected")
		return "", false
	}
	return path, true
}

func (s *syncer) logoUndetermined(it plexItem, why string) {
	s.countArtwork(func(r *runSummary) { r.LogosUndetermined++ })
	s.log("logo skipped: selection not identifiable", "artwork_type", ArtLogo, "title", itemLabel(it),
		"plex_rating_key", it.RatingKey, "reason", why)
}

// planSeasonPosters lists the season poster jobs for the winning copy of each
// series. Seasons are read from Plex per show (never episodes); Silo's
// seasons are read only for a series with a changed season poster, and a
// season is matched by number alone, within the series the show already
// maps to. A season Silo doesn't have is counted and skipped, never created.
func (s *syncer) planSeasonPosters(ctx context.Context, winners []posterCandidate) []ArtworkJob {
	var (
		mu   sync.Mutex
		jobs []ArtworkJob
		wg   sync.WaitGroup
	)
	work := make(chan posterCandidate)
	for range posterWorkers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for c := range work {
				j := s.planShowSeasons(ctx, c)
				mu.Lock()
				jobs = append(jobs, j...)
				mu.Unlock()
			}
		}()
	}
	for _, c := range winners {
		if c.job.mediaType != "tv" {
			continue
		}
		select {
		case work <- c:
		case <-ctx.Done():
		}
		if ctx.Err() != nil {
			break
		}
	}
	close(work)
	wg.Wait()
	return jobs
}

func (s *syncer) planShowSeasons(ctx context.Context, c posterCandidate) []ArtworkJob {
	show := c.job.item
	seasons, err := s.plex.Seasons(ctx, show.RatingKey)
	if err != nil {
		s.artworkError(ArtSeasonPoster, "%s: read Plex seasons: %v", itemLabel(show), err)
		return nil
	}
	type pending struct {
		number int
		path   string
	}
	var changed []pending
	for _, se := range seasons {
		if se.Thumb == "" {
			s.countArtwork(func(r *runSummary) { r.ArtworkMissing++ })
			continue
		}
		val := fingerprint(se.Thumb, c.job.contentID)
		s.mu.Lock()
		stored := s.state.artworkVal(seasonKey(c.sec, show.RatingKey, *se.Index))
		s.mu.Unlock()
		if recorded(stored, val) {
			s.countArtwork(func(r *runSummary) { r.ArtworkUnchanged++ })
			continue
		}
		changed = append(changed, pending{number: *se.Index, path: se.Thumb})
	}
	if len(changed) == 0 {
		return nil
	}
	siloSeasons, err := s.silo.SeriesSeasons(ctx, c.job.contentID)
	if err != nil {
		s.artworkError(ArtSeasonPoster, "%s: read Silo seasons: %v", itemLabel(show), err)
		return nil
	}
	var jobs []ArtworkJob
	for _, p := range changed {
		id, ok := siloSeasons[p.number]
		if !ok {
			s.countArtwork(func(r *runSummary) { r.SeasonsNotInSilo++ })
			s.log("season not in Silo", "artwork_type", ArtSeasonPoster, "title", itemLabel(show),
				"silo_item", c.job.contentID, "season_number", p.number, "status", "skipped")
			continue
		}
		// The fingerprint names the series, not the season ID: the season ID
		// is only known after asking Silo, which unchanged seasons skip.
		jobs = append(jobs, ArtworkJob{Type: ArtSeasonPoster, Title: itemLabel(show), RatingKey: show.RatingKey,
			Path: p.path, Season: p.number, SiloID: id, Owner: c.job.contentID, StateKey: seasonKey(c.sec, show.RatingKey, p.number),
			StateVal: fingerprint(p.path, c.job.contentID)})
	}
	return jobs
}

// applyArtwork runs the jobs with the poster worker limit. State advances
// only after Silo accepts an image; a dry run writes nothing.
func (s *syncer) applyArtwork(ctx context.Context, jobs []ArtworkJob) error {
	if s.cfg.DryRun {
		for _, j := range jobs {
			s.countArtwork(func(r *runSummary) { *r.artworkCounter(j.Type)++ })
			s.log("dry run: would apply artwork", j.logArgs("planned")...)
		}
		return nil
	}
	s.progress.set("Applying artwork", len(jobs))
	work := make(chan ArtworkJob)
	var wg sync.WaitGroup
	var done int
	for range posterWorkers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range work {
				applied, err := s.applyArtworkJob(ctx, j)
				s.progress.add(1)
				if err != nil {
					s.artworkError(j.Type, "%s: %v", j.describe(), err)
					continue
				}
				s.mu.Lock()
				if applied {
					*s.summary.artworkCounter(j.Type)++
				} else {
					s.summary.ArtworkUnchanged++
				}
				done++
				var ferr error
				if done%flushEvery == 0 {
					ferr = s.state.flush(ctx)
				}
				s.mu.Unlock()
				if ferr != nil {
					s.addError("save progress: %v", ferr)
				}
			}
		}()
	}
	for _, j := range jobs {
		if ctx.Err() != nil {
			break
		}
		select {
		case work <- j:
		case <-ctx.Done():
		}
	}
	close(work)
	wg.Wait()
	return ctx.Err()
}

// applyArtworkJob downloads one image and uploads it. applied is false when
// hashing showed Silo already has these bytes.
func (s *syncer) applyArtworkJob(ctx context.Context, j ArtworkJob) (applied bool, err error) {
	data, err := s.plex.Image(ctx, j.Path)
	if err != nil {
		return false, err
	}
	val := j.StateVal
	if val == "" {
		val = hashFingerprint(data, j.Owner)
		s.mu.Lock()
		same := recorded(s.state.artworkVal(j.StateKey), val)
		s.mu.Unlock()
		if same {
			return false, nil
		}
	}
	data, err = fitArtwork(j.Type, data)
	if err != nil {
		return false, err
	}
	err = s.silo.UploadItemImage(ctx, j.SiloID, j.Type.siloType(), data)
	if errors.Is(err, errNotFound) {
		// Re-keyed or removed in Silo since the lookup. Nothing is recorded,
		// so the next run looks the item up again.
		return false, fmt.Errorf("Silo item %s no longer exists; it is looked up again on the next run", j.SiloID)
	}
	if err != nil {
		return false, err
	}
	s.mu.Lock()
	s.state.setArtwork(j.StateKey, j.recordValue(val))
	s.mu.Unlock()
	s.log("artwork applied", j.logArgs("applied")...)
	return true, nil
}

func (j ArtworkJob) describe() string {
	if j.Type == ArtSeasonPoster {
		return fmt.Sprintf("%s season %d", j.Title, j.Season)
	}
	return j.Title
}

func (j ArtworkJob) logArgs(status string) []any {
	args := []any{"artwork_type", string(j.Type), "title", j.Title, "plex_rating_key", j.RatingKey, "silo_item", j.SiloID}
	if j.Type == ArtSeasonPoster {
		args = append(args, "season_number", j.Season)
	}
	return append(args, "status", status)
}

// isComposite reports Plex's generated collage artwork, which is not an image
// anyone chose.
func isComposite(path string) bool { return strings.Contains(path, "/composite/") }
