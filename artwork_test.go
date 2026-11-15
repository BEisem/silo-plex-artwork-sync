package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	"github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/runtimehost"
	"google.golang.org/protobuf/types/known/structpb"
)

// ---- artwork fakes ----

// artPlex is a read-only Plex with one movie library and one show library.
// Paths and their JSON are set per test; images are served as PNG bytes that
// name their path.
type artPlex struct {
	mu       sync.Mutex
	movie    map[string]any   // the movie's metadata
	movie4k  map[string]any   // the same film in "Movies 4K", or nil
	show     map[string]any   // the show's metadata
	seasons  []map[string]any // the show's seasons
	logos    []map[string]any // /clearLogos answer for every item
	logoCode int              // non-zero: /clearLogos answers this status
	coll     map[string]any   // a collection in Movies, or nil
	images   map[string][]byte
	requests []string
	writes   int
}

func (f *artPlex) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.Method != http.MethodGet {
			f.writes++
			t.Errorf("plugin wrote to Plex: %s %s", r.Method, r.URL)
		}
		f.requests = append(f.requests, r.URL.Path)
		write := func(v any) { _ = json.NewEncoder(w).Encode(map[string]any{"MediaContainer": v}) }
		list := func(items ...map[string]any) {
			var out []any
			for _, it := range items {
				if it != nil {
					out = append(out, it)
				}
			}
			write(map[string]any{"totalSize": len(out), "Metadata": out})
		}
		switch p := r.URL.Path; {
		case p == "/library/sections":
			dirs := []map[string]string{{"key": "1", "title": "Movies", "type": "movie", "uuid": "u-mov"},
				{"key": "2", "title": "TV", "type": "show", "uuid": "u-tv"}}
			if f.movie4k != nil {
				dirs = append(dirs, map[string]string{"key": "3", "title": "Movies 4K", "type": "movie", "uuid": "u-4k"})
			}
			write(map[string]any{"Directory": dirs})
		case p == "/library/sections/1/label", p == "/library/sections/2/label", p == "/library/sections/3/label":
			write(map[string]any{"Directory": []map[string]string{{"key": "9", "title": "Overlay"}}})
		case p == "/library/sections/1/all":
			if r.URL.Query().Get("label") != "" {
				list() // nothing is overlaid
				return
			}
			list(f.movie)
		case p == "/library/sections/3/all":
			list(f.movie4k)
		case p == "/library/sections/2/all":
			if r.URL.Query().Get("label") != "" {
				list()
				return
			}
			list(f.show)
		case p == "/library/metadata/20/children":
			list(f.seasons...)
		case strings.HasSuffix(p, "/clearLogos"):
			if f.logoCode != 0 {
				w.WriteHeader(f.logoCode)
				return
			}
			list(f.logos...)
		case p == "/library/sections/1/collections":
			list(f.coll)
		case p == "/library/sections/2/collections", p == "/library/sections/3/collections":
			list()
		case p == "/library/collections/900/children":
			list(f.movie)
		default:
			if data, ok := f.images[p]; ok {
				_, _ = w.Write(data)
				return
			}
			if strings.HasPrefix(p, "/library/") {
				_, _ = w.Write(pngNamed(p))
				return
			}
			t.Errorf("unexpected Plex request %s", r.URL)
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

// pngNamed is a tiny PNG followed by the path, so uploads can be traced.
func pngNamed(path string) []byte {
	var buf bytes.Buffer
	img := image.NewNRGBA(image.Rect(0, 0, 2, 2))
	_ = png.Encode(&buf, img)
	return append(buf.Bytes(), []byte("#"+path)...)
}

func uploadedPath(data string) string {
	_, path, _ := strings.Cut(data, "#")
	return path
}

type upload struct{ id, kind, path, contentType string }

// artSilo fakes the Silo routes the artwork sync uses.
type artSilo struct {
	mu        sync.Mutex
	uploads   []upload
	seasons   map[string][]map[string]any // series id -> seasons
	seasonReq int
	fail      map[string]int // "id/type" -> status to answer
	profile   string         // X-Profile-Id the season list requires
	collBack  map[string]string
	collPost  map[string]string
	collItems int
	created   bool
}

func (f *artSilo) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer sa_admin" {
			problem(w, http.StatusUnauthorized, "no key")
			return
		}
		path := strings.TrimPrefix(r.URL.Path, "/api/v2")
		parts := strings.Split(strings.Trim(path, "/"), "/")
		switch {
		case r.Method == http.MethodGet && path == "/profiles":
			_, _ = io.WriteString(w, `{"items":[{"id":"7","name":"Kid","is_primary":false},{"id":"1","name":"Admin","is_primary":true}]}`)
		case r.Method == http.MethodGet && len(parts) == 4 && parts[0] == "catalog" && parts[3] == "seasons":
			f.seasonReq++
			if r.Header.Get("X-Profile-Id") != f.profile {
				problem(w, http.StatusBadRequest, "X-Profile-Id header is required")
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"items": f.seasons[parts[2]]})
		case r.Method == http.MethodPost && len(parts) == 5 && parts[4] == "upload":
			if err := r.ParseMultipartForm(32 << 20); err != nil {
				t.Errorf("upload form: %v", err)
			}
			kind := r.FormValue("type")
			if st := f.fail[parts[2]+"/"+kind]; st != 0 {
				problem(w, st, "refused")
				return
			}
			file, hdr, err := r.FormFile("image")
			if err != nil {
				t.Fatalf("upload without image: %v", err)
			}
			data, _ := io.ReadAll(file)
			f.uploads = append(f.uploads, upload{id: parts[2], kind: kind, path: uploadedPath(string(data)), contentType: hdr.Header.Get("Content-Type")})
			_, _ = io.WriteString(w, `{}`)
		case r.Method == http.MethodGet && path == "/admin/collections":
			if f.created {
				_, _ = io.WriteString(w, `{"items":[{"id":"c1","slug":"kometa-1-saga","title":"Saga","library_ids":["1"]}]}`)
				return
			}
			_, _ = io.WriteString(w, `{"items":[]}`)
		case r.Method == http.MethodPost && path == "/admin/collections":
			f.created = true
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"id":"c1"}`)
		case r.Method == http.MethodGet && path == "/admin/collections/c1":
			w.Header().Set("ETag", `"e"`)
			_, _ = io.WriteString(w, `{"id":"c1"}`)
		case r.Method == http.MethodPut && (path == "/admin/collections/c1/poster" || path == "/admin/collections/c1/backdrop"):
			_ = r.ParseMultipartForm(32 << 20)
			file, _, err := r.FormFile("image")
			if err != nil {
				t.Fatalf("collection image: %v", err)
			}
			data, _ := io.ReadAll(file)
			if parts[3] == "poster" {
				f.collPost["c1"] = uploadedPath(string(data))
			} else {
				f.collBack["c1"] = uploadedPath(string(data))
			}
			_, _ = io.WriteString(w, `{"id":"c1"}`)
		case r.Method == http.MethodPut && len(parts) == 5 && parts[3] == "items":
			f.collItems++
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected Silo request %s %s", r.Method, r.URL)
			problem(w, http.StatusNotFound, "no")
		}
	})
}

func (f *artSilo) take() []upload {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.uploads
	f.uploads = nil
	return out
}

func (f *artSilo) has(kind, id, path string) bool {
	for _, u := range f.uploads {
		if u.kind == kind && u.id == id && u.path == path {
			return true
		}
	}
	return false
}

type artEnv struct {
	plex    *artPlex
	silo    *artSilo
	state   *memState
	catalog *fakeCatalog
	plexURL string
	siloURL string
}

func newArtEnv(t *testing.T) *artEnv {
	e := &artEnv{
		plex: &artPlex{
			movie: map[string]any{"ratingKey": "10", "title": "Film", "type": "movie", "year": 2020,
				"thumb": "/library/metadata/10/thumb/100", "art": "/library/metadata/10/art/100",
				"Guid":  []map[string]string{{"id": "tmdb://100"}},
				"Image": []map[string]string{{"type": "coverPoster", "url": "/library/metadata/10/thumb/100"}, {"type": "clearLogo", "url": "/library/metadata/10/clearLogo/100"}}},
			show: map[string]any{"ratingKey": "20", "title": "Show", "type": "show",
				"thumb": "/library/metadata/20/thumb/1", "art": "/library/metadata/20/art/1",
				"Guid": []map[string]string{{"id": "tmdb://300"}}},
			seasons: []map[string]any{
				{"ratingKey": "30", "type": "season", "index": 0, "title": "Specials", "thumb": "/library/metadata/30/thumb/1"},
				{"ratingKey": "31", "type": "season", "index": 1, "title": "Season 1", "thumb": "/library/metadata/31/thumb/1"},
				{"ratingKey": "32", "type": "season", "index": 2, "title": "Renamed Arc", "thumb": "/library/metadata/32/thumb/1"},
				{"ratingKey": "33", "type": "season", "index": 3, "title": "Season 3"},
				{"ratingKey": "34", "type": "season", "index": 4, "title": "Season 4", "thumb": "/library/metadata/34/thumb/1"},
			},
			logos: []map[string]any{{"key": "/library/metadata/20/clearLogo/5", "selected": true}, {"key": "https://provider.example/logo.png", "selected": false}},
		},
		silo: &artSilo{profile: "1", collBack: map[string]string{}, collPost: map[string]string{}, seasons: map[string][]map[string]any{
			"sr-s": {{"content_id": "se-0", "season_number": 0}, {"content_id": "se-1", "season_number": 1},
				{"content_id": "se-2", "season_number": 2}, {"content_id": "se-3", "season_number": 3}},
		}},
		state: &memState{m: map[string][]byte{}},
		catalog: &fakeCatalog{byType: map[string]map[string]runtimehost.MediaPresence{
			"movie": {"100": {MediaID: "mv-1", LibraryID: "1"}},
			"tv":    {"300": {MediaID: "sr-s", LibraryID: "2"}},
		}},
	}
	ps := httptest.NewServer(e.plex.handler(t))
	ss := httptest.NewServer(e.silo.handler(t))
	t.Cleanup(ps.Close)
	t.Cleanup(ss.Close)
	e.plexURL, e.siloURL = ps.URL, ss.URL
	return e
}

func allArtwork() Config {
	return Config{SyncBackdrops: true, SyncLogos: true, SyncSeasonPosters: true}
}

func (e *artEnv) run(t *testing.T, cfg Config) *runSummary {
	t.Helper()
	sum, err := e.runErr(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	return sum
}

func (e *artEnv) runErr(ctx context.Context, cfg Config) (*runSummary, error) {
	st, err := loadState(context.Background(), e.state)
	if err != nil {
		return nil, err
	}
	sum := &runSummary{}
	s := &syncer{cfg: cfg, catalog: e.catalog, plex: newPlexClient(e.plexURL, "plex-token"),
		silo: newSiloClient(e.siloURL, "sa_admin"), state: st, log: func(string, ...any) {}, summary: sum}
	return sum, s.run(ctx)
}

// ---- tests ----

func TestArtworkFirstRunAndNoChange(t *testing.T) {
	e := newArtEnv(t)
	sum := e.run(t, allArtwork())
	if sum.ErrorCount != 0 {
		t.Fatalf("errors: %v", sum.Errors)
	}
	got := e.silo.uploads
	for _, want := range []upload{
		{id: "mv-1", kind: "poster", path: "/library/metadata/10/thumb/100"},
		{id: "mv-1", kind: "backdrop", path: "/library/metadata/10/art/100"},
		{id: "mv-1", kind: "logo", path: "/library/metadata/10/clearLogo/100"},
		{id: "sr-s", kind: "poster", path: "/library/metadata/20/thumb/1"},
		{id: "sr-s", kind: "backdrop", path: "/library/metadata/20/art/1"},
		{id: "sr-s", kind: "logo", path: "/library/metadata/20/clearLogo/5"},
		{id: "se-0", kind: "poster", path: "/library/metadata/30/thumb/1"},
		{id: "se-1", kind: "poster", path: "/library/metadata/31/thumb/1"},
		{id: "se-2", kind: "poster", path: "/library/metadata/32/thumb/1"},
	} {
		if !e.silo.has(want.kind, want.id, want.path) {
			t.Errorf("missing upload %+v in %+v", want, got)
		}
	}
	if len(got) != 9 {
		t.Errorf("uploads = %d, want 9: %+v", len(got), got)
	}
	for _, u := range got {
		if u.contentType != "image/png" {
			t.Errorf("upload %+v declared %s", u, u.contentType)
		}
	}
	if sum.PostersApplied != 2 || sum.BackdropsApplied != 2 || sum.LogosApplied != 2 || sum.SeasonPostersApplied != 3 {
		t.Errorf("counts = %+v", sum)
	}
	// Season 3 has no poster in Plex; season 4 is not in Silo.
	if sum.ArtworkMissing != 1 || sum.SeasonsNotInSilo != 1 {
		t.Errorf("missing %d, seasons not in Silo %d", sum.ArtworkMissing, sum.SeasonsNotInSilo)
	}
	// Only the series' seasons were read, never episodes.
	for _, p := range e.plex.requests {
		if strings.Contains(p, "/31/children") || strings.Contains(p, "allLeaves") {
			t.Errorf("episodes were read: %s", p)
		}
	}

	e.silo.take()
	seasonReads := e.silo.seasonReq
	sum = e.run(t, allArtwork())
	if len(e.silo.uploads) != 0 || sum.ErrorCount != 0 {
		t.Fatalf("second run uploaded %+v errors %v", e.silo.uploads, sum.Errors)
	}
	if sum.ArtworkUnchanged != 7 || sum.PostersUnchanged != 2 {
		t.Errorf("unchanged = %d / posters %d", sum.ArtworkUnchanged, sum.PostersUnchanged)
	}
	// Season 4 is in Plex but not in Silo, so Silo's list for this show is
	// checked again each run in case the season appears; that is one read.
	if e.silo.seasonReq != seasonReads+1 {
		t.Errorf("Silo season reads: %d then %d", seasonReads, e.silo.seasonReq)
	}
	if e.plex.writes != 0 {
		t.Errorf("Plex was written to")
	}
}

func TestOnlyChangedArtworkIsUploaded(t *testing.T) {
	e := newArtEnv(t)
	e.run(t, allArtwork())
	e.silo.take()

	e.plex.mu.Lock()
	e.plex.movie["art"] = "/library/metadata/10/art/200" // backdrop changed
	e.plex.seasons[1]["thumb"] = "/library/metadata/31/thumb/2"
	e.plex.logos = []map[string]any{{"key": "/library/metadata/20/clearLogo/5"}, {"key": "/library/metadata/20/clearLogo/6", "selected": true}}
	e.plex.mu.Unlock()
	sum := e.run(t, allArtwork())
	got := e.silo.take()
	if len(got) != 3 || !hasUpload(got, "backdrop", "mv-1", "/library/metadata/10/art/200") ||
		!hasUpload(got, "poster", "se-1", "/library/metadata/31/thumb/2") ||
		!hasUpload(got, "logo", "sr-s", "/library/metadata/20/clearLogo/6") {
		t.Fatalf("uploads = %+v (%+v)", got, sum)
	}

	// Only the poster changes: the backdrop and logo stay put.
	e.plex.mu.Lock()
	e.plex.movie["thumb"] = "/library/metadata/10/thumb/300"
	e.plex.mu.Unlock()
	e.run(t, allArtwork())
	got = e.silo.take()
	if len(got) != 1 || got[0].kind != "poster" || got[0].id != "mv-1" {
		t.Fatalf("poster-only change uploaded %+v", got)
	}
}

func hasUpload(list []upload, kind, id, path string) bool {
	return (&artSilo{uploads: list}).has(kind, id, path)
}

func TestNewArtworkOffByDefault(t *testing.T) {
	e := newArtEnv(t)
	sum := e.run(t, Config{})
	for _, u := range e.silo.uploads {
		if u.kind != "poster" || strings.HasPrefix(u.id, "se-") {
			t.Errorf("default config uploaded %+v", u)
		}
	}
	if sum.PostersApplied != 2 || e.silo.seasonReq != 0 {
		t.Errorf("summary %+v season reads %d", sum, e.silo.seasonReq)
	}
	for _, p := range e.plex.requests {
		if strings.Contains(p, "clearLogos") || strings.Contains(p, "children") {
			t.Errorf("default config asked Plex for %s", p)
		}
	}
}

func TestDryRunWritesNothing(t *testing.T) {
	e := newArtEnv(t)
	cfg := allArtwork()
	cfg.DryRun = true
	sum := e.run(t, cfg)
	if len(e.silo.uploads) != 0 {
		t.Fatalf("dry run uploaded %+v", e.silo.uploads)
	}
	if sum.BackdropsApplied != 2 || sum.LogosApplied != 2 || sum.SeasonPostersApplied != 3 {
		t.Errorf("dry run should report what it would do: %+v", sum)
	}
	if len(e.state.m) != 0 {
		keys := []string{}
		for k := range e.state.m {
			keys = append(keys, k)
		}
		t.Errorf("dry run wrote state: %v", keys)
	}
}

func TestLogoSelection(t *testing.T) {
	cases := []struct {
		name    string
		logos   []map[string]any
		code    int
		want    string
		undeter bool
		errs    bool
	}{
		{name: "selected among several", logos: []map[string]any{{"key": "/library/metadata/20/clearLogo/1"}, {"key": "/library/metadata/20/clearLogo/2", "selected": true}}, want: "/library/metadata/20/clearLogo/2"},
		{name: "none selected", logos: []map[string]any{{"key": "/library/metadata/20/clearLogo/1"}, {"key": "/library/metadata/20/clearLogo/2"}}, undeter: true},
		{name: "two selected", logos: []map[string]any{{"key": "/library/metadata/20/clearLogo/1", "selected": true}, {"key": "/library/metadata/20/clearLogo/2", "selected": true}}, undeter: true},
		{name: "selected is a remote URL", logos: []map[string]any{{"key": "https://evil.example/l.png", "selected": true}}, undeter: true},
		{name: "no logos", undeter: true},
		{name: "endpoint unavailable", code: http.StatusInternalServerError, errs: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newArtEnv(t)
			e.plex.logos, e.plex.logoCode = c.logos, c.code
			sum, err := e.runErr(context.Background(), Config{SyncLogos: true, NoPosters: true})
			if err != nil {
				t.Fatal(err)
			}
			var showLogo []upload
			for _, u := range e.silo.uploads {
				if u.id == "sr-s" {
					showLogo = append(showLogo, u)
				}
			}
			switch {
			case c.want != "":
				if len(showLogo) != 1 || showLogo[0].path != c.want || showLogo[0].kind != "logo" {
					t.Errorf("uploads %+v", showLogo)
				}
			default:
				if len(showLogo) != 0 {
					t.Errorf("should not upload: %+v", showLogo)
				}
			}
			if c.undeter != (sum.LogosUndetermined == 1) {
				t.Errorf("undetermined = %d", sum.LogosUndetermined)
			}
			if c.errs != (sum.ErrorsByType["logo"] == 1) {
				t.Errorf("errors = %v", sum.ErrorsByType)
			}
			// The movie's logo comes from its listing and is still synced.
			if !e.silo.has("logo", "mv-1", "/library/metadata/10/clearLogo/100") {
				t.Errorf("movie logo missing: %+v", e.silo.uploads)
			}
		})
	}
}

func TestListedImagesWithoutLogoMeansNoLogo(t *testing.T) {
	e := newArtEnv(t)
	e.plex.movie["Image"] = []map[string]string{{"type": "coverPoster", "url": "/library/metadata/10/thumb/100"}}
	sum := e.run(t, Config{SyncLogos: true, NoPosters: true})
	for _, u := range e.silo.uploads {
		if u.id == "mv-1" {
			t.Errorf("uploaded %+v for an item with no logo", u)
		}
	}
	if sum.ArtworkMissing != 1 {
		t.Errorf("missing = %d", sum.ArtworkMissing)
	}
	for _, p := range e.plex.requests {
		if p == "/library/metadata/10/clearLogos" {
			t.Errorf("listed Image array should be trusted without asking /clearLogos")
		}
	}
}

func TestUploadFailureKeepsStateAndOthersProceed(t *testing.T) {
	e := newArtEnv(t)
	e.silo.fail = map[string]int{"mv-1/backdrop": http.StatusInternalServerError, "se-1/poster": http.StatusUnprocessableEntity}
	sum := e.run(t, allArtwork())
	if sum.ErrorsByType["backdrop"] != 1 || sum.ErrorsByType["season_poster"] != 1 {
		t.Fatalf("errors by type = %v", sum.ErrorsByType)
	}
	if !e.silo.has("logo", "mv-1", "/library/metadata/10/clearLogo/100") || !e.silo.has("poster", "se-2", "/library/metadata/32/thumb/1") {
		t.Errorf("other artwork should still be applied: %+v", e.silo.uploads)
	}
	e.silo.take()
	e.silo.fail = nil
	e.run(t, allArtwork())
	got := e.silo.take()
	if len(got) != 2 || !hasUpload(got, "backdrop", "mv-1", "/library/metadata/10/art/100") || !hasUpload(got, "poster", "se-1", "/library/metadata/31/thumb/1") {
		t.Fatalf("retry run should push exactly the failed ones: %+v", got)
	}
}

func TestSeasonListNeedsProfile(t *testing.T) {
	e := newArtEnv(t)
	e.silo.profile = "1" // the primary profile, not the first listed
	sum := e.run(t, Config{SyncSeasonPosters: true, NoPosters: true})
	if sum.SeasonPostersApplied != 3 || sum.ErrorCount != 0 {
		t.Fatalf("summary %+v", sum)
	}
}

func TestShowWithoutSeasons(t *testing.T) {
	e := newArtEnv(t)
	e.plex.seasons = nil
	sum := e.run(t, Config{SyncSeasonPosters: true, NoPosters: true})
	if sum.ErrorCount != 0 || len(e.silo.uploads) != 0 || e.silo.seasonReq != 0 {
		t.Fatalf("summary %+v uploads %+v", sum, e.silo.uploads)
	}
}

func TestSilosSeasonListIsValidated(t *testing.T) {
	e := newArtEnv(t)
	e.silo.seasons["sr-s"] = []map[string]any{{"content_id": "se-1", "season_number": 1}, {"content_id": "se-x", "season_number": 1}}
	sum := e.run(t, Config{SyncSeasonPosters: true, NoPosters: true})
	if len(e.silo.uploads) != 0 || sum.ErrorsByType["season_poster"] != 1 {
		t.Fatalf("ambiguous season list must not be used: %+v %+v", e.silo.uploads, sum.ErrorsByType)
	}
}

func TestDuplicateLibrariesUseWinner(t *testing.T) {
	e := newArtEnv(t)
	e.plex.movie4k = map[string]any{"ratingKey": "40", "title": "Film", "type": "movie",
		"thumb": "/library/metadata/40/thumb/1", "art": "/library/metadata/40/art/1",
		"Guid":  []map[string]string{{"id": "tmdb://100"}},
		"Media": []map[string]string{{"videoResolution": "4k"}},
		"Image": []map[string]string{{"type": "clearLogo", "url": "/library/metadata/40/clearLogo/1"}}}
	e.run(t, Config{SyncBackdrops: true, SyncLogos: true})
	got := e.silo.take()
	if !hasUpload(got, "backdrop", "mv-1", "/library/metadata/40/art/1") || !hasUpload(got, "logo", "mv-1", "/library/metadata/40/clearLogo/1") ||
		!hasUpload(got, "poster", "mv-1", "/library/metadata/40/thumb/1") {
		t.Fatalf("4K copy should win every type: %+v", got)
	}
	if hasUpload(got, "backdrop", "mv-1", "/library/metadata/10/art/100") {
		t.Errorf("losing copy's backdrop uploaded")
	}

	// The 4K copy leaves Plex: the 1080p library now wins and is pushed.
	e.plex.mu.Lock()
	e.plex.movie4k["Guid"] = []map[string]string{}
	e.plex.mu.Unlock()
	e.run(t, Config{SyncBackdrops: true, SyncLogos: true})
	got = e.silo.take()
	if !hasUpload(got, "backdrop", "mv-1", "/library/metadata/10/art/100") || !hasUpload(got, "logo", "mv-1", "/library/metadata/10/clearLogo/100") {
		t.Fatalf("new winner should be re-evaluated: %+v", got)
	}
}

func TestOverlayFilterAppliesToPostersOnly(t *testing.T) {
	e := newArtEnv(t)
	sum := e.run(t, Config{OnlyOverlaid: true, SyncBackdrops: true})
	got := e.silo.take()
	for _, u := range got {
		if u.kind == "poster" {
			t.Errorf("no item is overlaid, poster uploaded: %+v", u)
		}
	}
	if !hasUpload(got, "backdrop", "mv-1", "/library/metadata/10/art/100") || !hasUpload(got, "backdrop", "sr-s", "/library/metadata/20/art/1") {
		t.Errorf("backdrops are not filtered by the Overlay label: %+v", got)
	}
	if sum.PlexItems != 0 {
		t.Errorf("Plex items counts the poster listing as before, got %d", sum.PlexItems)
	}
}

func TestUnversionedPathIsHashed(t *testing.T) {
	e := newArtEnv(t)
	e.plex.movie["art"] = "/library/metadata/10/art"
	e.plex.images = map[string][]byte{"/library/metadata/10/art": pngNamed("v1")}
	e.run(t, Config{SyncBackdrops: true, NoPosters: true})
	if len(e.silo.take()) != 2 {
		t.Fatal("first run should upload both backdrops")
	}
	sum := e.run(t, Config{SyncBackdrops: true, NoPosters: true})
	if len(e.silo.take()) != 0 || sum.ArtworkUnchanged != 2 {
		t.Fatalf("same bytes should not be uploaded again: %+v", sum)
	}
	e.plex.mu.Lock()
	e.plex.images["/library/metadata/10/art"] = pngNamed("v2")
	e.plex.mu.Unlock()
	e.run(t, Config{SyncBackdrops: true, NoPosters: true})
	if got := e.silo.take(); len(got) != 1 || got[0].path != "v2" {
		t.Fatalf("changed bytes behind the same URL should be uploaded: %+v", got)
	}
}

func TestCancelledArtworkResumes(t *testing.T) {
	e := newArtEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	n := 0
	e.silo.mu.Lock()
	inner := e.silo.handler(t)
	e.silo.mu.Unlock()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inner.ServeHTTP(w, r)
		if strings.HasSuffix(r.URL.Path, "/upload") {
			n++
			if n == 4 {
				cancel()
			}
		}
	}))
	defer srv.Close()
	e.siloURL = srv.URL
	_, err := e.runErr(ctx, allArtwork())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	first := e.silo.take()
	sum := e.run(t, allArtwork())
	second := e.silo.take()
	// An upload whose answer was cut off by the cancel is not recorded and
	// is sent again: at least once, never lost.
	if len(second) == 0 || len(second) >= 9 || len(first)+len(second) > 10 || sum.ErrorCount != 0 {
		t.Fatalf("resume: first %d, second %d (%+v)", len(first), len(second), sum)
	}
	all := append(first, second...)
	for _, k := range []string{"mv-1/poster", "mv-1/backdrop", "mv-1/logo", "sr-s/poster", "sr-s/backdrop", "sr-s/logo", "se-0/poster", "se-1/poster", "se-2/poster"} {
		id, kind, _ := strings.Cut(k, "/")
		found := false
		for _, u := range all {
			found = found || (u.id == id && u.kind == kind)
		}
		if !found {
			t.Errorf("%s never uploaded", k)
		}
	}
}

func TestRekeyedItemIsNotRecorded(t *testing.T) {
	e := newArtEnv(t)
	e.silo.fail = map[string]int{"mv-1/backdrop": http.StatusNotFound}
	sum := e.run(t, Config{SyncBackdrops: true, NoPosters: true})
	if sum.ErrorsByType["backdrop"] != 1 || !strings.Contains(strings.Join(sum.Errors, " "), "looked up again") {
		t.Fatalf("errors %v", sum.Errors)
	}
	st, _ := loadState(context.Background(), e.state)
	if v := st.artworkVal("u-mov/10/backdrop"); v != "" {
		t.Fatalf("state advanced after a failed upload: %q", v)
	}
}

func TestCollectionBackdrop(t *testing.T) {
	e := newArtEnv(t)
	e.plex.coll = map[string]any{"ratingKey": "900", "title": "Saga", "type": "collection",
		"thumb": "/library/collections/900/thumb/1", "art": "/library/collections/900/art/1"}
	cfg := Config{SyncCollections: true, SyncCollectionBackdrops: true, NoPosters: true}
	sum := e.run(t, cfg)
	if e.silo.collBack["c1"] != "/library/collections/900/art/1" || e.silo.collPost["c1"] != "/library/collections/900/thumb/1" || sum.CollectionBackdropsApplied != 1 {
		t.Fatalf("collection art: %+v %+v %+v", e.silo.collBack, e.silo.collPost, sum)
	}
	// Only the backdrop changes: nothing else is touched.
	e.silo.collPost = map[string]string{}
	items := e.silo.collItems
	e.plex.mu.Lock()
	e.plex.coll["art"] = "/library/collections/900/art/2"
	e.plex.mu.Unlock()
	sum = e.run(t, cfg)
	if e.silo.collBack["c1"] != "/library/collections/900/art/2" || len(e.silo.collPost) != 0 || e.silo.collItems != items || sum.CollectionsCreated != 0 {
		t.Fatalf("backdrop change touched more: %+v %+v %d", e.silo.collPost, sum, e.silo.collItems)
	}
	// No backdrop in Plex: Silo's is kept.
	e.plex.mu.Lock()
	delete(e.plex.coll, "art")
	e.plex.mu.Unlock()
	e.run(t, cfg)
	if e.silo.collBack["c1"] != "/library/collections/900/art/2" {
		t.Fatal("backdrop removed")
	}
	// Off by default.
	e2 := newArtEnv(t)
	e2.plex.coll = e.plex.coll
	e2.plex.coll["art"] = "/library/collections/900/art/1"
	e2.run(t, Config{SyncCollections: true})
	if len(e2.silo.collBack) != 0 {
		t.Fatal("collection backdrop synced while off")
	}
}

// ---- images ----

func transparentPNG(w, h int, noisy bool) []byte {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	r := rand.New(rand.NewPCG(1, 2))
	for y := range h {
		for x := range w {
			c := color.NRGBA{R: 200, G: 30, B: 30, A: uint8(x % 256)}
			if noisy {
				c = color.NRGBA{R: uint8(r.IntN(256)), G: uint8(r.IntN(256)), B: uint8(r.IntN(256)), A: uint8(r.IntN(256))}
			}
			img.SetNRGBA(x, y, c)
		}
	}
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return buf.Bytes()
}

func TestFitArtworkKeepsLogoTransparency(t *testing.T) {
	small := transparentPNG(64, 32, false)
	out, err := fitArtwork(ArtLogo, small)
	if err != nil || !bytes.Equal(out, small) {
		t.Fatalf("a logo under the limit must pass unchanged: %v", err)
	}

	big := transparentPNG(2400, 1200, true) // noise: ~11 MiB as PNG
	if len(big) <= maxUploadBytes {
		t.Skipf("test image only %d bytes", len(big))
	}
	out, err = fitArtwork(ArtLogo, big)
	if err != nil {
		t.Fatal(err)
	}
	if http.DetectContentType(out) != "image/png" || len(out) > maxUploadBytes {
		t.Fatalf("got %s, %d bytes", http.DetectContentType(out), len(out))
	}
	img, err := png.Decode(bytes.NewReader(out))
	if err != nil {
		t.Fatal(err)
	}
	translucent := false
	b := img.Bounds()
	for y := b.Min.Y; y < b.Max.Y && !translucent; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			if _, _, _, a := img.At(x, y).RGBA(); a < 0xffff {
				translucent = true
				break
			}
		}
	}
	if !translucent {
		t.Fatal("alpha channel lost")
	}
}

func TestFitArtworkRefusals(t *testing.T) {
	webp := append([]byte("RIFF\x00\x00\x00\x00WEBPVP8 "), make([]byte, maxUploadBytes)...)
	if _, err := fitArtwork(ArtLogo, webp); err == nil || !strings.Contains(err.Error(), "transparency") {
		t.Errorf("oversized WebP logo: %v", err)
	}
	small := []byte("RIFF\x00\x00\x00\x00WEBPVP8 tiny")
	if out, err := fitArtwork(ArtLogo, small); err != nil || !bytes.Equal(out, small) {
		t.Errorf("small WebP logo should pass: %v", err)
	}
	if _, err := fitArtwork(ArtBackdrop, []byte("<html>error</html>")); err == nil {
		t.Error("an HTML page must not be uploaded")
	}
	big := append([]byte{0xff, 0xd8, 0xff}, make([]byte, maxUploadBytes)...)
	if _, err := fitArtwork(ArtBackdrop, big); err == nil {
		t.Error("corrupt oversized JPEG should fail")
	}
}

func TestPlexPathSafety(t *testing.T) {
	for p, ok := range map[string]bool{"/library/metadata/1/art/2": true, "//evil.example/x": false,
		"https://evil.example/x": false, "/photo/:/transcode?url=http://x": false, "relative": false} {
		if isPlexPath(p) != ok {
			t.Errorf("isPlexPath(%q) = %v", p, !ok)
		}
	}
	c := newPlexClient("http://127.0.0.1:1", "t")
	if _, err := c.Image(context.Background(), "//evil.example/x"); err == nil {
		t.Error("followed a host-relative URL")
	}
}

func TestFingerprint(t *testing.T) {
	if fingerprint("/library/metadata/1/art/170", "x") != "/library/metadata/1/art/170|x" {
		t.Error("versioned path")
	}
	if fingerprint("/library/metadata/1/art", "x") != "" {
		t.Error("an unversioned path must be hashed")
	}
	if !recorded("/a/1|sr|se-1", "/a/1|sr") || recorded("/a/1|sr", "") || recorded("/a/12|sr", "/a/1|sr") {
		t.Error("recorded")
	}
	if uploadTarget("sha256:ab|sr|se-1") != "se-1" || uploadTarget("/a/1|mv") != "mv" {
		t.Error("uploadTarget")
	}
}

// ---- compatibility with 0.5.0 ----

func TestConfigFrom050(t *testing.T) {
	v050, _ := structpb.NewStruct(map[string]any{"plex_url": "http://plex:32400", "plex_token": "tok", "silo_api_key": "sa_k",
		"webhook_secret": "s", "delay_minutes": 5.0, "only_overlaid": true, "sync_collections": true,
		"delete_removed_collections": true, "dry_run": false})
	cfg := parseConfig([]*pluginv1.ConfigEntry{{Key: "settings", Value: v050}})
	if cfg.NoPosters || cfg.SyncBackdrops || cfg.SyncLogos || cfg.SyncSeasonPosters || cfg.SyncCollectionBackdrops {
		t.Fatalf("new types must default off and posters on: %+v", cfg)
	}
	if cfg.PlexToken != "tok" || cfg.SiloAPIKey != "sa_k" || cfg.WebhookSecret != "s" || !cfg.OnlyOverlaid || cfg.validate() != nil {
		t.Fatalf("0.5.0 settings lost: %+v", cfg)
	}
	on, _ := structpb.NewStruct(map[string]any{"sync_posters": false, "sync_backdrops": true, "sync_logos": true,
		"sync_season_posters": true, "sync_collection_backdrops": true})
	cfg = parseConfig([]*pluginv1.ConfigEntry{{Key: "settings", Value: on}})
	if !cfg.NoPosters || !cfg.SyncBackdrops || !cfg.SyncLogos || !cfg.SyncSeasonPosters || !cfg.SyncCollectionBackdrops {
		t.Fatalf("switches not read: %+v", cfg)
	}
}

func TestManifestArtworkFields(t *testing.T) {
	data := manifestJSON
	var m struct {
		PluginID string `json:"plugin_id"`
		Schema   []struct {
			JSONSchema string `json:"json_schema"`
			Form       struct {
				Fields []struct {
					Key     string `json:"key"`
					Default any    `json:"default_value"`
				} `json:"fields"`
			} `json:"admin_form"`
		} `json:"global_config_schema"`
	}
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	if m.PluginID != pluginID {
		t.Fatalf("plugin id changed: %s", m.PluginID)
	}
	var schema struct {
		Properties map[string]any `json:"properties"`
	}
	if err := json.Unmarshal([]byte(m.Schema[0].JSONSchema), &schema); err != nil {
		t.Fatal(err)
	}
	defaults := map[string]any{}
	for _, f := range m.Schema[0].Form.Fields {
		if _, ok := schema.Properties[f.Key]; !ok {
			t.Errorf("form field %s is not in the JSON schema", f.Key)
		}
		defaults[f.Key] = f.Default
	}
	for key, want := range map[string]any{"sync_posters": true, "sync_backdrops": false, "sync_logos": false,
		"sync_season_posters": false, "sync_collection_backdrops": false} {
		if defaults[key] != want {
			t.Errorf("%s default = %v, want %v", key, defaults[key], want)
		}
	}
}

func TestPostersFrom050AreNotReuploaded(t *testing.T) {
	e := newArtEnv(t)
	// State as 0.5.0 left it: the poster shards only.
	st, _ := loadState(context.Background(), e.state)
	st.setPosterThumb("u-mov/10", "/library/metadata/10/thumb/100|mv-1")
	st.setPosterThumb("u-tv/20", "/library/metadata/20/thumb/1|sr-s")
	if err := st.flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	for k := range e.state.m {
		if strings.HasPrefix(k, "artwork/") {
			t.Fatalf("0.5.0 fixture wrote %s", k)
		}
	}
	_ = st.markMigrated(context.Background(), "winner:"+winnerHighestRes)
	sum := e.run(t, Config{})
	if len(e.silo.uploads) != 0 || sum.PostersUnchanged != 2 {
		t.Fatalf("upgrade re-uploaded posters: %+v %+v", e.silo.uploads, sum)
	}
	// Turning backdrops on uploads only backdrops.
	e.run(t, Config{SyncBackdrops: true})
	for _, u := range e.silo.take() {
		if u.kind != "backdrop" {
			t.Errorf("enabling backdrops uploaded %+v", u)
		}
	}
}

func TestArtworkStateFits(t *testing.T) {
	// 2,000 movies and 500 shows with every type, 3,000 seasons: the busiest
	// artwork shard must stay well under the host's 256 KiB value limit.
	mem := &memState{m: map[string][]byte{}}
	st, _ := loadState(context.Background(), mem)
	for i := range 2500 {
		for _, typ := range []ArtworkType{ArtBackdrop, ArtLogo} {
			st.setArtwork(fmt.Sprintf("0d6f7a3c-8b1e-4c2a-9f3e-1234567890ab/%d/%s", 100000+i, typ),
				fmt.Sprintf("/library/metadata/%d/%s/1712345678|series-tmdb-%d", 100000+i, typ, 1000000+i))
		}
	}
	for i := range 3000 {
		st.setArtwork(fmt.Sprintf("0d6f7a3c-8b1e-4c2a-9f3e-1234567890ab/%d/season/%d", 200000+i/6, i%6),
			fmt.Sprintf("sha256:%064d|series-tmdb-%d|series-tmdb-%d-S%02d", i, 1000000+i, 1000000+i, i%6))
	}
	if err := st.flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	biggest := 0
	for k, v := range mem.m {
		if strings.HasPrefix(k, "artwork/") {
			biggest = max(biggest, len(v))
		}
	}
	if biggest > maxStateValue/2 {
		t.Fatalf("largest artwork shard %d bytes", biggest)
	}
	t.Logf("largest artwork shard: %d bytes", biggest)
}
