package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/runtimehost"
)

const (
	pluginID         = "community.kometa-sync"
	overlayLabel     = "Overlay" // the label Kometa puts on items it has overlaid
	posterWorkers    = 4
	presenceBatch    = 100 // host limit for CheckMediaPresence
	maxErrorMessages = 25
	flushEvery       = 100
)

// presenceLookup is satisfied by *runtimehost.Client.
type presenceLookup interface {
	CheckMediaPresence(ctx context.Context, provider, mediaType string, ids []string) (map[string]runtimehost.MediaPresence, error)
}

// syncer holds what one run needs.
type syncer struct {
	cfg      Config
	catalog  presenceLookup
	plex     *PlexClient
	silo     *SiloClient
	state    *stateStore
	progress *syncProgress // nil-safe

	siloLibraries []siloLibrary                  // for placing collections
	libraryMap    map[string]int                 // Plex section ID -> chosen Silo library
	existing      map[string]*siloCollectionInfo // Silo collections by ID, read per run
	bySlug        map[string]*siloCollectionInfo
	combined      bool // one Silo collection per title across libraries of the same type
	log           func(msg string, args ...any)
	selected      func(plexSection) bool          // nil: fall back to cfg.wantsLibrary
	rank          func(plexSection, plexItem) int // nil: highest resolution wins
	policy        string                          // duplicate-policy signature

	mu      sync.Mutex
	summary *runSummary
}

func (s *syncer) addError(format string, args ...any) {
	if isCancellation(args) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.summary.ErrorCount++
	if len(s.summary.Errors) < maxErrorMessages {
		s.summary.Errors = append(s.summary.Errors, fmt.Sprintf(format, args...))
	}
	s.log("sync error", "error", fmt.Sprintf(format, args...))
}

// resolveSiloBase works out the loopback URL the plugin calls Silo's REST
// API on.
func resolveSiloBase(ctx context.Context, host *runtimehost.Client, override string) (string, error) {
	base := strings.TrimRight(override, "/")
	if base == "" {
		info, err := host.GetHostInfo(ctx)
		if err != nil {
			return "", fmt.Errorf("get host info: %w", err)
		}
		base = normalizeLoopback(info.InternalBaseURL)
	}
	if base == "" {
		return "", errors.New("Silo did not report its internal URL; set \"Silo internal URL\" in the plugin settings")
	}
	return base, nil
}

// defaultPluginContentPrefix is where Silo serves plugin HTTP routes. Silo
// reports the full address (PluginProxyBaseUrl) when it knows its public URL;
// this is only the fallback for building a placeholder when it doesn't.
const defaultPluginContentPrefix = "/api/v2/plugin-content"

// pluginBaseURL is the address of this plugin's own HTTP routes, e.g.
// https://silo.example.com/api/v2/plugin-content/plugins/12. Without a public
// URL configured in Silo, the host part is the placeholder SILO_HOST.
func pluginBaseURL(ctx context.Context, host *runtimehost.Client) (string, error) {
	if info, err := host.GetHostInfo(ctx); err == nil && info.PluginProxyBaseURL != "" {
		return strings.TrimRight(info.PluginProxyBaseURL, "/"), nil
	}
	plugins, err := host.ListInstalledPlugins(ctx)
	if err != nil {
		return "", fmt.Errorf("list installed plugins: %w", err)
	}
	for _, p := range plugins {
		if p.GetPluginId() == pluginID && p.GetEnabled() {
			return fmt.Sprintf("http://SILO_HOST%s/plugins/%d", defaultPluginContentPrefix, p.GetInstallationId()), nil
		}
	}
	return "", errors.New("could not find this plugin's installation id")
}

// normalizeLoopback rewrites wildcard listen addresses (":8080",
// "0.0.0.0:8080") to 127.0.0.1, because Silo refuses to dial 0.0.0.0.
func normalizeLoopback(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	host, port, err := net.SplitHostPort(u.Host)
	if err != nil {
		return strings.TrimRight(raw, "/")
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		u.Host = net.JoinHostPort("127.0.0.1", port)
	}
	return strings.TrimRight(u.String(), "/")
}

// run performs one full sync.
func (s *syncer) run(ctx context.Context) error {
	sections, err := s.plex.Sections(ctx)
	if err != nil {
		return err
	}
	pick := s.selected
	if pick == nil {
		pick = func(sec plexSection) bool { return s.cfg.wantsLibrary(sec.Title) }
	}
	var wanted []plexSection
	for _, sec := range sections {
		if pick(sec) {
			wanted = append(wanted, sec)
		}
	}
	if len(wanted) == 0 {
		s.summary.Note = "No libraries are selected, so nothing was synced. Choose libraries on this page."
		return nil
	}

	var cands []posterCandidate
	var unlabeled []posterJob
	s.progress.set("Reading Plex libraries", len(wanted))
	for i, sec := range wanted {
		if err := ctx.Err(); err != nil {
			return err
		}
		s.progress.detailf(sec.Title)
		c, u, err := s.collectPosters(ctx, sec, i)
		s.progress.add(1)
		if err != nil {
			s.addError("posters in %q: %v", sec.Title, err)
			continue
		}
		cands = append(cands, c...)
		unlabeled = append(unlabeled, u...)
	}
	policy := s.policy
	if policy == "" {
		policy = winnerHighestRes
	}
	migration := "winner:" + policy
	forceShared := !s.state.migrated(ctx, migration)
	applyErr := s.applyPosters(ctx, s.pickPosters(cands, unlabeled, forceShared))
	if err := s.state.flush(ctx); err != nil {
		return err
	}
	if applyErr != nil {
		return applyErr
	}
	if forceShared && !s.cfg.DryRun && s.summary.ErrorCount == 0 {
		if err := s.state.markMigrated(ctx, migration); err != nil {
			s.addError("save migration: %v", err)
		}
	}

	if !s.cfg.SyncCollections {
		if s.summary.Note == "" {
			s.summary.Note = "Plex collections are turned off in the plugin settings, so collections were not checked."
		}
		return nil
	}
	return s.syncCollections(ctx, wanted)
}

func mediaTypeFor(sec plexSection) string {
	if sec.Type == "show" {
		return "tv"
	}
	return "movie"
}

// lookup maps TMDB ids to Silo catalog matches (movies or series only).
func (s *syncer) lookup(ctx context.Context, mediaType string, tmdbIDs []string) (map[string]runtimehost.MediaPresence, error) {
	out := map[string]runtimehost.MediaPresence{}
	unique := dedupe(tmdbIDs)
	for start := 0; start < len(unique); start += presenceBatch {
		end := min(start+presenceBatch, len(unique))
		found, err := s.catalog.CheckMediaPresence(ctx, "tmdb", mediaType, unique[start:end])
		if err != nil {
			return nil, fmt.Errorf("Silo catalog lookup: %w", err)
		}
		for k, v := range found {
			out[k] = v
		}
	}
	return out, nil
}

type posterJob struct {
	item      plexItem
	contentID string
	mediaType string // "movie" or "tv", for looking the item up again
	tmdb      string
	stateKey  string
	stateVal  string
	forget    bool // overlay removed in Plex: push the clean poster once, then stop tracking
}

// itemLabel names an item in error messages, without doubling a year the
// title already carries ("The Head (2020)").
func itemLabel(it plexItem) string {
	if it.Year > 0 && !strings.Contains(it.Title, fmt.Sprintf("(%d)", it.Year)) {
		return fmt.Sprintf("%s (%d)", it.Title, it.Year)
	}
	return it.Title
}

func posterKey(sec plexSection, ratingKey string) string {
	return sectionID(sec) + "/" + ratingKey
}

// posterCandidate is one Plex item that maps to a Silo movie or series.
type posterCandidate struct {
	job     posterJob
	rank    int  // higher resolution wins when two libraries share a Silo item
	order   int  // Plex library order, the tiebreak
	changed bool // its poster differs from what was last pushed for it
}

// resolutionRank orders Plex resolutions. Plex reports "4k", "1080", "720",
// "576", "480" or "sd" per media version.
func resolutionRank(res string) int {
	switch strings.ToLower(strings.TrimSpace(res)) {
	case "4k", "2160", "2160p", "uhd":
		return 4
	case "1080", "1080p":
		return 3
	case "720", "720p":
		return 2
	case "":
		return 0
	default:
		return 1
	}
}

// libraryNameRank treats libraries named like "Movies 4K" or "TV UHD" as 4K.
// Plex reports no resolution for shows, so this is how TV libraries rank.
func libraryNameRank(title string) int {
	t := strings.ToLower(title)
	for _, hint := range []string{"4k", "uhd", "2160"} {
		if strings.Contains(t, hint) {
			return 4
		}
	}
	return 0
}

func (s *syncer) rankOf(sec plexSection, it plexItem) int {
	if s.rank != nil {
		return s.rank(sec, it)
	}
	return itemRank(sec, it)
}

func itemRank(sec plexSection, it plexItem) int {
	rank := libraryNameRank(sec.Title)
	for _, m := range it.Media {
		rank = max(rank, resolutionRank(m.VideoResolution))
	}
	return rank
}

// collectPosters lists a library's candidates without applying anything.
func (s *syncer) collectPosters(ctx context.Context, sec plexSection, order int) ([]posterCandidate, []posterJob, error) {
	label := ""
	if s.cfg.OnlyOverlaid {
		label = overlayLabel
	}
	items, err := s.plex.Items(ctx, sec, label)
	if err != nil {
		return nil, nil, err
	}

	var ids []string
	for _, it := range items {
		if id := it.tmdbID(); id != "" {
			ids = append(ids, id)
		}
	}
	matches, err := s.lookup(ctx, mediaTypeFor(sec), ids)
	if err != nil {
		return nil, nil, err
	}

	var out []posterCandidate
	s.mu.Lock()
	s.summary.PlexItems += len(items)
	for _, it := range items {
		tmdb := it.tmdbID()
		switch {
		case tmdb == "":
			s.summary.NoTMDBID++
			continue
		case it.Thumb == "":
			continue
		}
		match, ok := matches[tmdb]
		if !ok || match.MediaID == "" {
			s.summary.NotInSilo++
			continue
		}
		// The thumb path ends in an upload timestamp, so it changes whenever
		// Kometa (or anyone) replaces the poster in Plex.
		key := posterKey(sec, it.RatingKey)
		val := it.Thumb + "|" + match.MediaID
		out = append(out, posterCandidate{
			job:     posterJob{item: it, contentID: match.MediaID, mediaType: mediaTypeFor(sec), tmdb: tmdb, stateKey: key, stateVal: val},
			rank:    s.rankOf(sec, it),
			order:   order,
			changed: s.state.posterThumb(key) != val,
		})
	}
	s.mu.Unlock()

	var unlabeled []posterJob
	if s.cfg.OnlyOverlaid {
		unlabeled = s.unlabeledJobs(ctx, sec, items)
	}
	return out, unlabeled, nil
}

// pickPosters decides what to push when several Plex libraries hold the same
// Silo item (for example Movies and Movies 4k): only the highest-resolution
// copy's poster is used. forceShared re-pushes the winner of every shared
// item once, to correct items an older version left on the lower-res poster.
func (s *syncer) pickPosters(cands []posterCandidate, unlabeled []posterJob, forceShared bool) []posterJob {
	byItem := map[string][]posterCandidate{}
	var order []string
	for _, c := range cands {
		if _, ok := byItem[c.job.contentID]; !ok {
			order = append(order, c.job.contentID)
		}
		byItem[c.job.contentID] = append(byItem[c.job.contentID], c)
	}
	var jobs []posterJob
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range order {
		group := byItem[id]
		win := group[0]
		for _, c := range group[1:] {
			if c.rank > win.rank || (c.rank == win.rank && c.order < win.order) {
				win = c
			}
		}
		s.summary.DuplicatesSkipped += len(group) - 1
		if win.changed || (forceShared && len(group) > 1) {
			jobs = append(jobs, win.job)
		} else {
			s.summary.PostersUnchanged++
		}
	}
	// An item whose overlay was removed in one library but that another
	// library still pushes keeps the other library's poster.
	for _, j := range unlabeled {
		if _, stillPushed := byItem[j.contentID]; stillPushed {
			s.state.deletePoster(j.stateKey)
			continue
		}
		jobs = append(jobs, j)
	}
	return jobs
}

// applyPosters pushes the chosen posters to Silo.
func (s *syncer) applyPosters(ctx context.Context, jobs []posterJob) error {
	if s.cfg.DryRun {
		s.mu.Lock()
		s.summary.PostersApplied += len(jobs)
		s.mu.Unlock()
		for _, j := range jobs {
			s.log("dry run: would apply poster", "title", j.item.Title, "silo_item", j.contentID)
		}
		return nil
	}

	s.progress.set("Applying posters", len(jobs))
	work := make(chan posterJob)
	var wg sync.WaitGroup
	var done int
	for range posterWorkers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range work {
				err := s.applyPoster(ctx, j)
				if errors.Is(err, errNotFound) {
					// Silo re-keys an item when it matches it better (for example
					// series-imdb-… to series-tvdb-…), and the old ID stops
					// existing. Look the title up again and retry once.
					var gone bool
					j, gone, err = s.retryWithCurrentID(ctx, j)
					if gone {
						s.progress.add(1)
						continue
					}
				}
				s.progress.add(1)
				if err != nil {
					s.addError("%s: %v", itemLabel(j.item), err)
					continue
				}
				s.mu.Lock()
				if j.forget {
					s.state.deletePoster(j.stateKey)
				} else {
					s.state.setPosterThumb(j.stateKey, j.stateVal)
				}
				s.summary.PostersApplied++
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

// unlabeledJobs finds items this plugin synced before that no longer carry
// Kometa's Overlay label (overlays removed or reset). Their current, clean
// Plex poster is pushed to Silo once so Silo doesn't keep a stale overlay.
// Silo IDs are looked up fresh, since Silo may have re-keyed the item since.
func (s *syncer) unlabeledJobs(ctx context.Context, sec plexSection, labeled []plexItem) []posterJob {
	seen := map[string]bool{}
	for _, it := range labeled {
		seen[posterKey(sec, it.RatingKey)] = true
	}
	forget := func(key string) {
		if !s.cfg.DryRun {
			s.mu.Lock()
			s.state.deletePoster(key)
			s.mu.Unlock()
		}
	}
	type pending struct {
		key string
		it  plexItem
	}
	var todo []pending
	var ids []string
	for _, key := range s.state.posterKeys(sectionID(sec) + "/") {
		if seen[key] {
			continue
		}
		ratingKey := strings.TrimPrefix(key, sectionID(sec)+"/")
		it, found, err := s.plex.Item(ctx, ratingKey)
		if err != nil {
			s.addError("check unlabeled item %s: %v", ratingKey, err)
			continue
		}
		if !found || it.Thumb == "" || it.tmdbID() == "" || (it.Type != "movie" && it.Type != "show") {
			forget(key)
			continue
		}
		todo = append(todo, pending{key: key, it: it})
		ids = append(ids, it.tmdbID())
	}
	if len(todo) == 0 {
		return nil
	}
	matches, err := s.lookup(ctx, mediaTypeFor(sec), ids)
	if err != nil {
		s.addError("check unlabeled items in %q: %v", sec.Title, err)
		return nil
	}
	var jobs []posterJob
	for _, p := range todo {
		m, ok := matches[p.it.tmdbID()]
		if !ok || m.MediaID == "" {
			forget(p.key)
			continue
		}
		jobs = append(jobs, posterJob{item: p.it, contentID: m.MediaID, mediaType: mediaTypeFor(sec), tmdb: p.it.tmdbID(), stateKey: p.key, forget: true})
	}
	return jobs
}

// retryWithCurrentID re-resolves a job whose Silo item no longer exists.
// gone reports that Silo has no item for the title any more, which is not an
// error: the poster has nowhere to go, and the item is no longer tracked.
func (s *syncer) retryWithCurrentID(ctx context.Context, j posterJob) (posterJob, bool, error) {
	notFound := fmt.Errorf("Silo item %s no longer exists", j.contentID)
	if j.tmdb == "" || j.mediaType == "" {
		return j, false, notFound
	}
	found, err := s.lookup(ctx, j.mediaType, []string{j.tmdb})
	if err != nil {
		return j, false, err
	}
	m, ok := found[j.tmdb]
	if !ok || m.MediaID == "" {
		s.mu.Lock()
		s.summary.NotInSilo++
		s.state.deletePoster(j.stateKey)
		s.mu.Unlock()
		return j, true, nil
	}
	if m.MediaID == j.contentID {
		return j, false, notFound
	}
	j.contentID = m.MediaID
	if !j.forget {
		j.stateVal = j.item.Thumb + "|" + m.MediaID
	}
	return j, false, s.applyPoster(ctx, j)
}

func (s *syncer) applyPoster(ctx context.Context, j posterJob) error {
	data, err := s.plex.Image(ctx, j.item.Thumb)
	if err != nil {
		return err
	}
	return s.silo.UploadPoster(ctx, j.contentID, data)
}

func sectionID(sec plexSection) string {
	if sec.UUID != "" {
		return sec.UUID
	}
	return "section-" + sec.Key
}

func recMembers(rec *collectionRecord) []string {
	if rec == nil {
		return nil
	}
	return rec.Members
}

func diffMembers(desired, current []string) (added, removed []string) {
	cur := map[string]bool{}
	for _, c := range current {
		cur[c] = true
	}
	want := map[string]bool{}
	for _, d := range desired {
		want[d] = true
		if !cur[d] {
			added = append(added, d)
		}
	}
	for _, c := range current {
		if !want[c] {
			removed = append(removed, c)
		}
	}
	return added, removed
}

func dedupe(ids []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

func nowRFC3339() string { return time.Now().UTC().Format(time.RFC3339) }
