package main

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"sort"
	"strings"
	"time"
)

// Host instance state allows at most 256 keys of 256 KiB each, so the poster
// and collection maps are split across fixed shards. Only shards that changed
// are written back.
const (
	posterShards     = 32
	collectionShards = 64
	artworkShards    = 32 // backdrops, logos and season posters
	maxStateValue    = 250 << 10
)

// stateIO is the subset of the SDK's runtime host client the store needs.
type stateIO interface {
	ReadInstanceState(ctx context.Context, key string) ([]byte, bool, error)
	WriteInstanceState(ctx context.Context, key string, value []byte) error
}

type collectionRecord struct {
	SiloID   string   `json:"silo_id"`
	Title    string   `json:"title,omitempty"`
	Libs     []int    `json:"libs,omitempty"`
	Thumb    string   `json:"thumb,omitempty"`
	Art      string   `json:"art,omitempty"`      // backdrop path last uploaded
	Members  []string `json:"members,omitempty"`  // Silo content IDs this plugin added
	Sections []string `json:"sections,omitempty"` // Plex libraries it draws on; first supplies the poster
	Order    []string `json:"order,omitempty"`    // member order last sent to Silo
}

// runSummary is persisted so the status page survives plugin restarts.
type runSummary struct {
	StartedAt          string `json:"started_at"`
	FinishedAt         string `json:"finished_at,omitempty"`
	Trigger            string `json:"trigger"`
	DryRun             bool   `json:"dry_run,omitempty"`
	PlexItems          int    `json:"plex_items"`
	PostersApplied     int    `json:"posters_applied"`
	PostersUnchanged   int    `json:"posters_unchanged"`
	NotInSilo          int    `json:"not_in_silo"`
	NoTMDBID           int    `json:"no_tmdb_id"`
	CollectionsCreated int    `json:"collections_created"`
	CollectionsUpdated int    `json:"collections_updated"`
	CollectionsDeleted int    `json:"collections_deleted"`
	PostersReverted    int    `json:"posters_reverted,omitempty"`
	Cancelled          bool   `json:"cancelled,omitempty"`
	Note               string `json:"note,omitempty"`
	DuplicatesSkipped  int    `json:"duplicates_skipped,omitempty"`

	// Artwork beyond movie and series posters (absent on 0.5.0 summaries).
	BackdropsApplied           int            `json:"backdrops_applied,omitempty"`
	LogosApplied               int            `json:"logos_applied,omitempty"`
	SeasonPostersApplied       int            `json:"season_posters_applied,omitempty"`
	CollectionBackdropsApplied int            `json:"collection_backdrops_applied,omitempty"`
	ArtworkUnchanged           int            `json:"artwork_unchanged,omitempty"`
	ArtworkMissing             int            `json:"artwork_missing,omitempty"`    // nothing selected in Plex
	LogosUndetermined          int            `json:"logos_undetermined,omitempty"` // selection not identifiable
	SeasonsNotInSilo           int            `json:"seasons_not_in_silo,omitempty"`
	ErrorsByType               map[string]int `json:"errors_by_type,omitempty"`
	CollectionsChecked         int            `json:"collections_checked,omitempty"`
	Errors                     []string       `json:"errors,omitempty"`
	ErrorCount                 int            `json:"error_count"`
}

type metaState struct {
	PendingDue string      `json:"pending_due,omitempty"` // RFC3339; a webhook-requested run not yet done
	LastRun    *runSummary `json:"last_run,omitempty"`
}

type stateStore struct {
	io          stateIO
	posters     [posterShards]map[string]string
	collections [collectionShards]map[string]*collectionRecord
	artwork     [artworkShards]map[string]string
	dirtyP      [posterShards]bool
	dirtyC      [collectionShards]bool
	dirtyA      [artworkShards]bool
}

func shardOf(key string, n int) int {
	h := fnv.New32a()
	_, _ = h.Write([]byte(key))
	return int(h.Sum32() % uint32(n))
}

func loadState(ctx context.Context, io stateIO) (*stateStore, error) {
	s := &stateStore{io: io}
	for i := range posterShards {
		s.posters[i] = map[string]string{}
		if err := s.read(ctx, fmt.Sprintf("posters/%02d", i), &s.posters[i]); err != nil {
			return nil, err
		}
	}
	for i := range collectionShards {
		s.collections[i] = map[string]*collectionRecord{}
		if err := s.read(ctx, fmt.Sprintf("collections/%02d", i), &s.collections[i]); err != nil {
			return nil, err
		}
	}
	for i := range artworkShards {
		s.artwork[i] = map[string]string{}
		if err := s.read(ctx, fmt.Sprintf("artwork/%02d", i), &s.artwork[i]); err != nil {
			return nil, err
		}
	}
	return s, nil
}

func (s *stateStore) read(ctx context.Context, key string, into any) error {
	data, found, err := s.io.ReadInstanceState(ctx, key)
	if err != nil {
		return fmt.Errorf("read state %s: %w", key, err)
	}
	if !found || len(data) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, into); err != nil {
		return fmt.Errorf("decode state %s: %w", key, err)
	}
	return nil
}

func (s *stateStore) write(ctx context.Context, key string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(data) > maxStateValue {
		return fmt.Errorf("state %s is %d bytes, over the host limit", key, len(data))
	}
	return s.io.WriteInstanceState(ctx, key, data)
}

func (s *stateStore) posterThumb(ratingKey string) string {
	return s.posters[shardOf(ratingKey, posterShards)][ratingKey]
}

func (s *stateStore) setPosterThumb(ratingKey, thumb string) {
	i := shardOf(ratingKey, posterShards)
	s.posters[i][ratingKey] = thumb
	s.dirtyP[i] = true
}

func (s *stateStore) deletePoster(key string) {
	i := shardOf(key, posterShards)
	delete(s.posters[i], key)
	s.dirtyP[i] = true
}

// posterKeys returns tracked poster keys that start with prefix.
func (s *stateStore) posterKeys(prefix string) []string {
	var keys []string
	for i := range posterShards {
		for k := range s.posters[i] {
			if strings.HasPrefix(k, prefix) {
				keys = append(keys, k)
			}
		}
	}
	sort.Strings(keys)
	return keys
}

// Artwork other than movie and series posters is kept apart from the poster
// shards, so posters recorded by 0.5.0 keep their keys and values and are not
// pushed again. Keys are "<section>/<ratingKey>/<type>" (season posters:
// "<section>/<showRatingKey>/season/<n>").
func (s *stateStore) artworkVal(key string) string {
	return s.artwork[shardOf(key, artworkShards)][key]
}

func (s *stateStore) setArtwork(key, val string) {
	i := shardOf(key, artworkShards)
	s.artwork[i][key] = val
	s.dirtyA[i] = true
}

func (s *stateStore) deleteArtwork(key string) {
	i := shardOf(key, artworkShards)
	if _, ok := s.artwork[i][key]; ok {
		delete(s.artwork[i], key)
		s.dirtyA[i] = true
	}
}

// artworkKeys returns tracked artwork keys that start with prefix.
func (s *stateStore) artworkKeys(prefix string) []string {
	var keys []string
	for i := range artworkShards {
		for k := range s.artwork[i] {
			if strings.HasPrefix(k, prefix) {
				keys = append(keys, k)
			}
		}
	}
	sort.Strings(keys)
	return keys
}

func (s *stateStore) collection(key string) *collectionRecord {
	return s.collections[shardOf(key, collectionShards)][key]
}

func (s *stateStore) setCollection(key string, rec *collectionRecord) {
	i := shardOf(key, collectionShards)
	if rec == nil {
		delete(s.collections[i], key)
	} else {
		sort.Strings(rec.Members)
		s.collections[i][key] = rec
	}
	s.dirtyC[i] = true
}

// collectionKeys returns every tracked collection key.
func (s *stateStore) collectionKeys() []string {
	var keys []string
	for i := range collectionShards {
		for k := range s.collections[i] {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return keys
}

// flush writes back every shard that changed.
func (s *stateStore) flush(ctx context.Context) error {
	// Saving must finish even when the run was cancelled; that is what lets
	// the next run skip the work already done.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
	defer cancel()
	for i := range posterShards {
		if s.dirtyP[i] {
			if err := s.write(ctx, fmt.Sprintf("posters/%02d", i), s.posters[i]); err != nil {
				return err
			}
			s.dirtyP[i] = false
		}
	}
	for i := range artworkShards {
		if s.dirtyA[i] {
			if err := s.write(ctx, fmt.Sprintf("artwork/%02d", i), s.artwork[i]); err != nil {
				return err
			}
			s.dirtyA[i] = false
		}
	}
	for i := range collectionShards {
		if s.dirtyC[i] {
			if err := s.write(ctx, fmt.Sprintf("collections/%02d", i), s.collections[i]); err != nil {
				return err
			}
			s.dirtyC[i] = false
		}
	}
	return nil
}

func readMeta(ctx context.Context, io stateIO) (metaState, error) {
	var m metaState
	data, found, err := io.ReadInstanceState(ctx, "meta")
	if err != nil || !found || len(data) == 0 {
		return m, err
	}
	err = json.Unmarshal(data, &m)
	return m, err
}

func writeMeta(ctx context.Context, io stateIO, m metaState) error {
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return io.WriteInstanceState(ctx, "meta", data)
}

// One-time corrections, tracked by name so each runs once. Each duplicate
// policy ("winner:<signature>") is one: switching policy re-pushes the winner
// of every shared item once, so Silo matches the new choice.
func (s *stateStore) migrated(ctx context.Context, name string) bool {
	data, found, err := s.io.ReadInstanceState(ctx, "migrations")
	if err != nil || !found {
		return false
	}
	var done map[string]bool
	_ = json.Unmarshal(data, &done)
	return done[name]
}

func (s *stateStore) markMigrated(ctx context.Context, name string) error {
	done := map[string]bool{}
	if data, found, err := s.io.ReadInstanceState(ctx, "migrations"); err == nil && found {
		_ = json.Unmarshal(data, &done)
	}
	if strings.HasPrefix(name, "winner:") {
		for k := range done {
			if strings.HasPrefix(k, "winner:") {
				delete(done, k)
			}
		}
	}
	done[name] = true
	return s.write(ctx, "migrations", done)
}
