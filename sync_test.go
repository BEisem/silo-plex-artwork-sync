package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
	"io"
	"math/rand/v2"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	"github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/runtimehost"
	"github.com/hashicorp/go-hclog"
)

// ---- fakes ----

type memState struct {
	mu sync.Mutex
	m  map[string][]byte
}

func (s *memState) ReadInstanceState(_ context.Context, key string) ([]byte, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.m[key]
	return v, ok, nil
}

func (s *memState) WriteInstanceState(_ context.Context, key string, value []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(key) > 256 || len(value) > 256<<10 {
		panic("host state limits exceeded")
	}
	s.m[key] = append([]byte(nil), value...)
	return nil
}

type fakeCatalog struct {
	byType map[string]map[string]runtimehost.MediaPresence
}

func (f *fakeCatalog) CheckMediaPresence(_ context.Context, provider, mediaType string, ids []string) (map[string]runtimehost.MediaPresence, error) {
	if provider != "tmdb" {
		panic("unexpected provider " + provider)
	}
	if len(ids) > 100 {
		panic("too many ids in one presence call")
	}
	out := map[string]runtimehost.MediaPresence{}
	for _, id := range ids {
		if m, ok := f.byType[mediaType][id]; ok {
			out[id] = m
		}
	}
	return out, nil
}

type fakePlex struct {
	mu          sync.Mutex
	movieThumbA string
	collMembers []string // tmdb ids
	collections bool
	bUnlabeled  bool // Kometa removed Movie B's overlay
}

func (f *fakePlex) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Plex-Token") != "plex-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		write := func(v any) { _ = json.NewEncoder(w).Encode(map[string]any{"MediaContainer": v}) }
		movie := func(rk, title, tmdb, thumb string) map[string]any {
			m := map[string]any{"ratingKey": rk, "title": title, "type": "movie", "thumb": thumb, "guid": "plex://movie/" + rk}
			if tmdb != "" {
				m["Guid"] = []map[string]string{{"id": "imdb://tt1"}, {"id": "tmdb://" + tmdb}}
			}
			return m
		}
		q := r.URL.Query()
		switch {
		case r.URL.Path == "/library/sections":
			write(map[string]any{"Directory": []map[string]string{
				{"key": "1", "title": "Movies", "type": "movie", "uuid": "uuid-movies"},
				{"key": "2", "title": "TV Shows", "type": "show", "uuid": "uuid-tv"},
				{"key": "3", "title": "Music", "type": "artist", "uuid": "uuid-music"},
			}})
		case r.URL.Path == "/library/sections/1/label":
			write(map[string]any{"Directory": []map[string]string{{"key": "55", "title": "overlay"}, {"key": "56", "title": "Other"}}})
		case r.URL.Path == "/library/sections/2/label":
			write(map[string]any{"Directory": []map[string]string{{"key": "77", "title": "Overlay"}}})
		case r.URL.Path == "/library/sections/1/all":
			if q.Get("type") != "1" || q.Get("label") != "55" || q.Get("includeGuids") != "1" {
				t.Errorf("movie list query = %v", q)
			}
			list := []any{movie("10", "Movie A", "100", f.movieThumbA)}
			if !f.bUnlabeled {
				list = append(list, movie("11", "Movie B", "200", "/library/metadata/11/thumb/1"))
			}
			list = append(list, movie("12", "No GUID", "", "/library/metadata/12/thumb/1"),
				map[string]any{"ratingKey": "99", "title": "An Episode", "type": "episode", "thumb": "/library/metadata/99/thumb/1",
					"Guid": []map[string]string{{"id": "tmdb://999"}}})
			write(map[string]any{"totalSize": len(list), "Metadata": list})
		case r.URL.Path == "/library/metadata/11":
			write(map[string]any{"Metadata": []any{movie("11", "Movie B", "200", "/library/metadata/11/thumb/clean")}})
		case r.URL.Path == "/library/sections/2/all":
			if q.Get("type") != "2" || q.Get("label") != "77" {
				t.Errorf("show list query = %v", q)
			}
			write(map[string]any{"totalSize": 1, "Metadata": []any{
				map[string]any{"ratingKey": "20", "title": "Show S", "type": "show", "thumb": "/library/metadata/20/thumb/1", "guid": "plex://show/20",
					"Guid": []map[string]string{{"id": "tmdb://300"}}},
			}})
		case r.URL.Path == "/library/sections/1/collections":
			var colls []any
			if f.collections {
				colls = append(colls, map[string]any{"ratingKey": "900", "title": "Marvel", "type": "collection", "thumb": "/library/metadata/900/thumb/5", "guid": "collection://900"})
			}
			write(map[string]any{"Metadata": colls})
		case r.URL.Path == "/library/sections/2/collections":
			write(map[string]any{"Metadata": []any{}})
		case r.URL.Path == "/library/collections/900/children":
			var kids []any
			for _, id := range f.collMembers {
				kids = append(kids, movie("m"+id, "member "+id, id, "/x"))
			}
			write(map[string]any{"Metadata": kids})
		case strings.Contains(r.URL.Path, "/thumb/"):
			w.Header().Set("Content-Type", "image/jpeg")
			_, _ = io.WriteString(w, "IMAGE:"+r.URL.Path)
		default:
			t.Errorf("unexpected Plex request %s", r.URL)
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

type fakeSilo struct {
	mu        sync.Mutex
	routes    *routesServer
	applied   map[string]string // content id -> image bytes Silo fetched
	collTitle map[string]string
	collLibs  map[string][]int
	collItems map[string]map[string]bool
	collPost  map[string]string
	deleted   []string
	writes    int
	nextID    int
	srv       *httptest.Server
	locks     map[string][]int // content id -> locked fields
	throttle  int              // answer this many collection-item adds with 429 first
	collSlug  map[string]string
	goneIDs   map[string]bool // content IDs Silo answers 404 for
	fkGone    map[string]bool // content IDs a collection add rejects with 500
	collPos   map[string]map[string]int
	collRev   map[string]int
	staleOnce bool            // change the collection right after its next ETag read
	inUse     map[string]bool // collections a page row still shows (delete is 409)
	reorders  int
	refreshed []string
	jobPolls  map[string]int
	onApply   func() // called after each poster Silo accepts
}

func (f *fakeSilo) pos(collection string) map[string]int {
	if f.collPos == nil {
		f.collPos = map[string]map[string]int{}
	}
	if f.collPos[collection] == nil {
		f.collPos[collection] = map[string]int{}
	}
	return f.collPos[collection]
}

// ordered lists a collection's items by position, as Silo shows them.
func (f *fakeSilo) ordered(collection string) []string {
	var ids []string
	for id := range f.collItems[collection] {
		ids = append(ids, id)
	}
	p := f.pos(collection)
	sort.Slice(ids, func(i, j int) bool {
		if p[ids[i]] != p[ids[j]] {
			return p[ids[i]] < p[ids[j]]
		}
		return ids[i] < ids[j]
	})
	return ids
}

func (f *fakeSilo) etag(id string) string {
	if f.collRev == nil {
		f.collRev = map[string]int{}
	}
	return fmt.Sprintf(`"r%d"`, f.collRev[id])
}

func (f *fakeSilo) bump(id string) {
	if f.collRev == nil {
		f.collRev = map[string]int{}
	}
	f.collRev[id]++
}

func stringIDs(ids []int) []string {
	out := []string{}
	for _, id := range ids {
		out = append(out, strconv.Itoa(id))
	}
	return out
}

func intIDs(ids []string) []int {
	var out []int
	for _, id := range ids {
		n, _ := strconv.Atoi(id)
		out = append(out, n)
	}
	return out
}

func problem(w http.ResponseWriter, status int, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"type": "https://silo/problems/x", "title": http.StatusText(status), "status": status, "detail": detail, "instance": "urn:silo:request:1"})
}

// handler fakes Silo's /api/v2 admin API, including its ETag preconditions.
// Any /api/v1 call fails the test, as it answers 410 on a current Silo.
func (f *fakeSilo) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/v1/") {
			t.Errorf("plugin called retired API: %s %s", r.Method, r.URL.Path)
			problem(w, http.StatusGone, "client_upgrade_required")
			return
		}
		if r.Header.Get("Authorization") != "Bearer sa_admin" {
			problem(w, http.StatusUnauthorized, "no key")
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		recordRequest(r)
		path, ok := strings.CutPrefix(r.URL.Path, "/api/v2")
		if !ok {
			t.Errorf("unexpected Silo request %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		parts := strings.Split(strings.Trim(path, "/"), "/")
		coll := func() (string, bool) {
			id := parts[2]
			if _, ok := f.collTitle[id]; !ok {
				problem(w, http.StatusNotFound, "Collection not found")
				return id, false
			}
			return id, true
		}
		guard := func(id string) bool {
			switch m := r.Header.Get("If-Match"); {
			case m == "":
				problem(w, http.StatusPreconditionRequired, "If-Match required")
				return false
			case m != "*" && m != f.etag(id):
				w.Header().Set("ETag", f.etag(id))
				problem(w, http.StatusPreconditionFailed, "stale")
				return false
			}
			return true
		}
		switch {
		case r.Method == http.MethodPost && len(parts) == 5 && parts[1] == "items" && parts[3] == "images" && parts[4] == "apply":
			t.Errorf("plugin used images/apply; posters should be uploaded")
			problem(w, http.StatusBadRequest, "no")
		case r.Method == http.MethodPost && len(parts) == 5 && parts[1] == "items" && parts[4] == "upload" && f.goneIDs[parts[2]]:
			problem(w, http.StatusNotFound, "Item not found")
		case r.Method == http.MethodPost && len(parts) == 5 && parts[1] == "items" && parts[4] == "upload":
			f.writes++
			if err := r.ParseMultipartForm(16 << 20); err != nil {
				t.Errorf("upload form: %v", err)
				problem(w, http.StatusBadRequest, "form")
				return
			}
			file, _, err := r.FormFile("image")
			if err != nil || r.FormValue("type") != "poster" {
				t.Errorf("bad upload: type=%q err=%v", r.FormValue("type"), err)
				problem(w, http.StatusBadRequest, "form")
				return
			}
			data, _ := io.ReadAll(file)
			f.applied[parts[2]] = string(data)
			if f.onApply != nil {
				f.onApply()
			}
			_, _ = io.WriteString(w, `{"content_id":"`+parts[2]+`","stored_path":"x","thumbhash":"y"}`)
		case r.Method == http.MethodGet && path == "/admin/collections":
			list := []map[string]any{}
			for id, title := range f.collTitle {
				list = append(list, map[string]any{"id": id, "title": title, "slug": f.collSlug[id], "library_ids": stringIDs(f.collLibs[id])})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"items": list, "groups": []any{}, "page": map[string]any{"has_more": false}})
		case r.Method == http.MethodPost && path == "/admin/collections":
			f.writes++
			var req struct {
				Title          string   `json:"title"`
				Slug           string   `json:"slug"`
				LibraryIDs     []string `json:"library_ids"`
				CollectionType string   `json:"collection_type"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			if req.CollectionType != "manual" {
				t.Errorf("collection type = %q", req.CollectionType)
			}
			f.nextID++
			id := "c" + string(rune('0'+f.nextID))
			if f.collSlug == nil {
				f.collSlug = map[string]string{}
			}
			for other, slug := range f.collSlug {
				if slug == req.Slug && f.collTitle[other] != "" {
					t.Errorf("duplicate slug %q created", req.Slug)
				}
			}
			f.collTitle[id], f.collLibs[id], f.collItems[id], f.collSlug[id] = req.Title, intIDs(req.LibraryIDs), map[string]bool{}, req.Slug
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "title": req.Title, "library_ids": req.LibraryIDs})
		case len(parts) == 3 && parts[1] == "collections" && r.Method == http.MethodGet:
			if id, ok := coll(); ok {
				w.Header().Set("ETag", f.etag(id))
				_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "title": f.collTitle[id]})
				if f.staleOnce { // someone edits it in Silo before our write lands
					f.staleOnce = false
					f.bump(id)
				}
			}
		case len(parts) == 3 && parts[1] == "collections" && r.Method == http.MethodPatch:
			id, ok := coll()
			if !ok || !guard(id) {
				return
			}
			f.writes++
			var upd struct {
				Title      *string   `json:"title"`
				LibraryIDs *[]string `json:"library_ids"`
			}
			_ = json.NewDecoder(r.Body).Decode(&upd)
			if upd.LibraryIDs != nil {
				f.collLibs[id] = intIDs(*upd.LibraryIDs)
			}
			if upd.Title != nil {
				f.collTitle[id] = *upd.Title
			}
			f.bump(id)
			w.Header().Set("ETag", f.etag(id))
			_ = json.NewEncoder(w).Encode(map[string]any{"id": id})
		case len(parts) == 4 && parts[1] == "collections" && parts[3] == "poster" && r.Method == http.MethodPut:
			id, ok := coll()
			if !ok {
				return
			}
			f.writes++
			_ = r.ParseMultipartForm(16 << 20)
			file, hdr, err := r.FormFile("image")
			if err != nil || hdr.Header.Get("Content-Type") != "image/jpeg" {
				t.Errorf("poster part: %v %v", err, hdr)
				problem(w, http.StatusBadRequest, "image")
				return
			}
			data, _ := io.ReadAll(file)
			f.collPost[id] = string(data)
			f.bump(id)
			w.Header().Set("ETag", f.etag(id))
			_ = json.NewEncoder(w).Encode(map[string]any{"id": id})
		case len(parts) == 3 && parts[1] == "collections" && r.Method == http.MethodDelete:
			id, ok := coll()
			if !ok || !guard(id) {
				return
			}
			if f.inUse[id] {
				problem(w, http.StatusConflict, "Collection is used by a page section")
				return
			}
			f.writes++
			delete(f.collTitle, id)
			f.deleted = append(f.deleted, id)
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodGet && len(parts) == 3 && parts[0] == "catalog" && parts[1] == "items":
			_ = json.NewEncoder(w).Encode(map[string]any{"content_id": parts[2], "locked_fields": f.locks[parts[2]]})
		case r.Method == http.MethodPatch && len(parts) == 4 && parts[1] == "items" && parts[3] == "metadata":
			f.writes++
			var req struct {
				LockedFields *[]int `json:"locked_fields"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			if req.LockedFields == nil {
				t.Errorf("metadata patch without locked_fields")
			} else {
				f.locks[parts[2]] = *req.LockedFields
			}
			_, _ = io.WriteString(w, `{}`)
		case r.Method == http.MethodPost && len(parts) == 4 && parts[1] == "items" && parts[3] == "refresh-metadata":
			f.writes++
			if slicesContains(f.locks[parts[2]], 10) {
				t.Errorf("refresh queued while %s still has its image lock", parts[2])
			}
			f.refreshed = append(f.refreshed, parts[2])
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "job-" + parts[2], "state": "queued", "terminal": false})
		case r.Method == http.MethodGet && len(parts) == 3 && parts[1] == "jobs":
			f.jobPolls[parts[2]]++
			state, terminal := "running", false
			if f.jobPolls[parts[2]] > 1 {
				state, terminal = "succeeded", true
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"id": parts[2], "state": state, "terminal": terminal})
		case len(parts) == 5 && parts[1] == "collections" && parts[3] == "items" && r.Method == http.MethodPut && f.fkGone[parts[4]]:
			problem(w, http.StatusNotFound, "Item not found in collection libraries")
		case len(parts) == 5 && parts[1] == "collections" && parts[3] == "items" && parts[4] != "order" && f.throttle > 0:
			f.throttle--
			w.Header().Set("Retry-After", "1")
			problem(w, http.StatusTooManyRequests, "slow down")
		case len(parts) == 5 && parts[1] == "collections" && parts[3] == "items" && parts[4] == "order" && r.Method == http.MethodGet:
			id, ok := coll()
			if !ok {
				return
			}
			ids := f.ordered(id)
			more := len(ids) > orderWindow
			if more {
				ids = ids[:orderWindow]
			}
			w.Header().Set("ETag", f.etag(id))
			_ = json.NewEncoder(w).Encode(map[string]any{"ordered_ids": ids, "has_more": more, "group_id": nil})
		case len(parts) == 5 && parts[1] == "collections" && parts[3] == "items" && parts[4] == "order" && r.Method == http.MethodPut:
			id, ok := coll()
			if !ok || !guard(id) {
				return
			}
			f.writes++
			var req struct {
				OrderedIDs []string `json:"ordered_ids"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			// Like Silo: the list must name exactly the items the collection holds.
			if len(req.OrderedIDs) != len(f.collItems[id]) {
				problem(w, http.StatusUnprocessableEntity, "ordered ids do not match collection items")
				return
			}
			for _, item := range req.OrderedIDs {
				if !f.collItems[id][item] {
					problem(w, http.StatusUnprocessableEntity, "unknown id")
					return
				}
			}
			f.reorders++
			for i, item := range req.OrderedIDs {
				f.pos(id)[item] = i
			}
			f.bump(id)
			w.Header().Set("ETag", f.etag(id))
			_ = json.NewEncoder(w).Encode(map[string]any{"ordered_ids": f.ordered(id), "has_more": false})
		case len(parts) == 5 && parts[1] == "collections" && parts[3] == "items":
			id, ok := coll()
			if !ok {
				return
			}
			f.writes++
			if r.Method == http.MethodPut {
				var req struct {
					Position *int `json:"position"`
				}
				_ = json.NewDecoder(r.Body).Decode(&req)
				if req.Position == nil {
					problem(w, http.StatusUnprocessableEntity, "position required")
					return
				}
				// Like Silo v2: add if absent; an existing item keeps its position.
				if !f.collItems[id][parts[4]] {
					f.collItems[id][parts[4]] = true
					f.pos(id)[parts[4]] = *req.Position
				}
			} else {
				delete(f.collItems[id], parts[4])
			}
			f.bump(id)
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected Silo request %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

// orderWindow is how many members Silo's items/order read returns.
var orderWindow = 200

// ---- test ----

func TestSyncEndToEnd(t *testing.T) {
	plex := &fakePlex{movieThumbA: "/library/metadata/10/thumb/1", collMembers: []string{"100", "200"}, collections: true}
	plexSrv := httptest.NewServer(plex.handler(t))
	defer plexSrv.Close()

	silo := &fakeSilo{
		routes:  &routesServer{p: &plugin{}},
		applied: map[string]string{}, collTitle: map[string]string{}, collLibs: map[string][]int{},
		collItems: map[string]map[string]bool{}, collPost: map[string]string{},
		locks: map[string][]int{}, jobPolls: map[string]int{},
	}
	siloSrv := httptest.NewServer(silo.handler(t))
	defer siloSrv.Close()

	catalog := &fakeCatalog{byType: map[string]map[string]runtimehost.MediaPresence{
		"movie": {"100": {MediaID: "mv-a", LibraryID: "3"}, "200": {MediaID: "mv-b", LibraryID: "3"}, "999": {MediaID: "should-never-be-used", LibraryID: "3"}},
		"tv":    {"300": {MediaID: "sr-s", LibraryID: "4"}},
	}}
	state := &memState{m: map[string][]byte{}}
	cfg := Config{PlexURL: plexSrv.URL, PlexToken: "plex-token", SiloAPIKey: "sa_admin", WebhookSecret: "s",
		OnlyOverlaid: true, SyncCollections: true, DeleteRemoved: true}

	run := func() *runSummary {
		t.Helper()
		st, err := loadState(context.Background(), state)
		if err != nil {
			t.Fatal(err)
		}
		summary := &runSummary{}
		s := &syncer{cfg: cfg, catalog: catalog, plex: newPlexClient(plexSrv.URL, "plex-token"),
			silo: newSiloClient(siloSrv.URL, "sa_admin"), state: st, log: func(string, ...any) {}, summary: summary}
		if err := s.run(context.Background()); err != nil {
			t.Fatal(err)
		}
		if summary.ErrorCount != 0 {
			t.Fatalf("errors: %v", summary.Errors)
		}
		return summary
	}

	// Run 1: everything is new.
	sum := run()
	if sum.PostersApplied != 3 || sum.NoTMDBID != 1 || sum.CollectionsCreated != 1 {
		t.Fatalf("run 1 summary = %+v", sum)
	}
	if silo.applied["mv-a"] != "IMAGE:/library/metadata/10/thumb/1" || silo.applied["sr-s"] == "" {
		t.Fatalf("applied = %v", silo.applied)
	}
	if _, ok := silo.applied["should-never-be-used"]; ok {
		t.Fatal("an episode-level item was synced")
	}
	if len(silo.collItems["c1"]) != 2 || silo.collPost["c1"] != "IMAGE:/library/metadata/900/thumb/5" || silo.collLibs["c1"][0] != 3 {
		t.Fatalf("collection = items %v poster %q libs %v", silo.collItems["c1"], silo.collPost["c1"], silo.collLibs["c1"])
	}

	// Run 2: nothing changed in Plex, so nothing is written to Silo.
	writes := silo.writes
	sum = run()
	if sum.PostersApplied != 0 || sum.PostersUnchanged != 3 || sum.CollectionsUpdated != 0 || silo.writes != writes {
		t.Fatalf("run 2 should be a no-op: %+v, writes %d -> %d", sum, writes, silo.writes)
	}

	// Run 3: Kometa re-overlaid Movie A and Movie B left the collection.
	plex.mu.Lock()
	plex.movieThumbA = "/library/metadata/10/thumb/2"
	plex.collMembers = []string{"100"}
	plex.mu.Unlock()
	sum = run()
	if sum.PostersApplied != 1 || silo.applied["mv-a"] != "IMAGE:/library/metadata/10/thumb/2" || sum.CollectionsUpdated != 1 {
		t.Fatalf("run 3 summary = %+v", sum)
	}
	if len(silo.collItems["c1"]) != 1 || !silo.collItems["c1"]["mv-a"] {
		t.Fatalf("collection items after removal = %v", silo.collItems["c1"])
	}

	// Run 4: the collection was deleted in Plex, so the plugin deletes its copy.
	plex.mu.Lock()
	plex.collections = false
	plex.mu.Unlock()
	sum = run()
	if sum.CollectionsDeleted != 1 || len(silo.deleted) != 1 || silo.deleted[0] != "c1" {
		t.Fatalf("run 4 summary = %+v deleted %v", sum, silo.deleted)
	}

	// Run 5: Kometa removed Movie B's overlay. Its clean poster goes to Silo
	// once, then it is no longer tracked.
	plex.mu.Lock()
	plex.bUnlabeled = true
	plex.mu.Unlock()
	sum = run()
	if sum.PostersApplied != 1 || silo.applied["mv-b"] != "IMAGE:/library/metadata/11/thumb/clean" {
		t.Fatalf("run 5 summary = %+v applied %v", sum, silo.applied["mv-b"])
	}
	writes = silo.writes
	sum = run()
	if sum.PostersApplied != 0 || silo.writes != writes {
		t.Fatalf("run 6 should be a no-op: %+v", sum)
	}
}

func TestWebhookSecretAndEvents(t *testing.T) {
	p := &plugin{log: hclog.NewNullLogger(), cfg: Config{WebhookSecret: "hunter2", DelayMinutes: 60}}
	r := &routesServer{p: p}
	call := func(secret, body string) int32 {
		q, _ := toStruct(map[string]any{"secret": secret})
		resp, _ := r.Handle(context.Background(), &pluginv1.HandleHTTPRequest{Method: "POST", Path: "/kometa", Query: q, Body: []byte(body)})
		return resp.GetStatusCode()
	}
	if code := call("wrong", `{"event":"run_end"}`); code != http.StatusUnauthorized {
		t.Fatalf("wrong secret -> %d", code)
	}
	if code := call("hunter2", `{"event":"run_start"}`); code != http.StatusOK || !p.due.IsZero() {
		t.Fatalf("run_start should be ignored, got %d", code)
	}
	if code := call("hunter2", `{"event":"run_end","start_time":"x"}`); code != http.StatusAccepted || p.due.IsZero() {
		t.Fatalf("run_end -> %d, due %v", code, p.due)
	}
	p.timer.Stop()
}

func TestNormalizeLoopback(t *testing.T) {
	for in, want := range map[string]string{
		"http://0.0.0.0:8080":   "http://127.0.0.1:8080",
		"http://:8080":          "http://127.0.0.1:8080",
		"http://127.0.0.1:8080": "http://127.0.0.1:8080",
		"http://[::]:8080/":     "http://127.0.0.1:8080",
	} {
		if got := normalizeLoopback(in); got != want {
			t.Errorf("normalizeLoopback(%q) = %q, want %q", in, got, want)
		}
	}
	_ = url.URL{}
}

func TestParseConfigDefaults(t *testing.T) {
	v, _ := toStruct(map[string]any{"plex_url": "http://plex:32400/", "plex_libraries": "Movies, 4K Movies ,", "only_overlaid": false, "delay_minutes": 10})
	cfg := parseConfig([]*pluginv1.ConfigEntry{{Key: "settings", Value: v}})
	if cfg.PlexURL != "http://plex:32400" || len(cfg.PlexLibraries) != 2 || cfg.OnlyOverlaid || !cfg.SyncCollections || cfg.DelayMinutes != 10 {
		t.Fatalf("cfg = %+v", cfg)
	}
	if !cfg.wantsLibrary("4k movies") || cfg.wantsLibrary("TV Shows") {
		t.Fatal("library filter")
	}
}

func TestOversizedPosterIsReencoded(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 2000, 3000))
	rnd := rand.New(rand.NewPCG(1, 2))
	for i := range img.Pix {
		img.Pix[i] = byte(rnd.IntN(256))
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	if buf.Len() <= maxUploadBytes {
		t.Fatalf("test image too small: %d", buf.Len())
	}
	out, err := fitForUpload(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if len(out) > maxUploadBytes || http.DetectContentType(out) != "image/jpeg" {
		t.Fatalf("re-encoded to %d bytes, %s", len(out), http.DetectContentType(out))
	}
}

func TestConnectionCheckPage(t *testing.T) {
	plex := &fakePlex{movieThumbA: "/x"}
	srv := httptest.NewServer(plex.handler(t))
	defer srv.Close()
	p := &plugin{log: hclog.NewNullLogger(), cfg: Config{
		PlexURL: srv.URL, PlexToken: "plex-token", SiloAPIKey: "sa_x", WebhookSecret: "s",
		PlexLibraries: []string{"Movies", "Kids"}, OnlyOverlaid: true}}
	checks := p.connectionCheck(context.Background(), p.cfg)
	byName := map[string]checkResult{}
	for _, c := range checks {
		byName[c.Name] = c
	}
	if !byName["Settings"].OK || !byName["Plex"].OK || byName["Plex"].Detail != "Connected. Libraries to sync: Movies" {
		t.Fatalf("checks = %+v", checks)
	}
	p.cfg.PlexLibraries = nil
	checks = p.connectionCheck(context.Background(), p.cfg)
	byName = map[string]checkResult{}
	for _, c := range checks {
		byName[c.Name] = c
	}
	if !byName["Plex"].OK || !byName["Kometa Overlay label"].OK || byName["Silo"].OK {
		t.Fatalf("checks = %+v", checks)
	}
	q, _ := toStruct(map[string]any{"check": "1"})
	resp := (&routesServer{p: p}).status(&pluginv1.HandleHTTPRequest{Method: "GET", Path: "/status", Query: q})
	if !strings.Contains(string(resp.GetBody()), "Kometa Overlay label") || !strings.Contains(string(resp.GetBody()), "Run again") {
		t.Fatalf("status page missing check results:\n%s", resp.GetBody())
	}
}

func TestTaskRunReturnsImmediately(t *testing.T) {
	// No host and no config: the background sync fails fast, but Run itself
	// must return at once regardless of how long a sync would take.
	p := &plugin{log: hclog.NewNullLogger()}
	start := time.Now()
	resp, err := (&taskServer{p: p}).Run(context.Background(), &pluginv1.RunScheduledTaskRequest{})
	if err != nil || resp.GetOutput().AsMap()["started"] != true {
		t.Fatalf("Run = %v, %v", resp, err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("Run blocked")
	}
	for range 50 {
		p.mu.Lock()
		done := p.lastRun != nil
		p.mu.Unlock()
		if done {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("background sync never recorded a result")
}

func slicesContains(s []int, v int) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func TestRevertLibrary(t *testing.T) {
	plex := &fakePlex{movieThumbA: "/library/metadata/10/thumb/1", collMembers: []string{"100", "200"}, collections: true}
	plexSrv := httptest.NewServer(plex.handler(t))
	defer plexSrv.Close()
	silo := &fakeSilo{
		routes:  &routesServer{p: &plugin{}},
		applied: map[string]string{}, collTitle: map[string]string{}, collLibs: map[string][]int{},
		collItems: map[string]map[string]bool{}, collPost: map[string]string{},
		locks: map[string][]int{}, jobPolls: map[string]int{},
	}
	siloSrv := httptest.NewServer(silo.handler(t))
	defer siloSrv.Close()
	catalog := &fakeCatalog{byType: map[string]map[string]runtimehost.MediaPresence{
		"movie": {"100": {MediaID: "mv-a", LibraryID: "3"}, "200": {MediaID: "mv-b", LibraryID: "3"}},
		"tv":    {"300": {MediaID: "sr-s", LibraryID: "4"}},
	}}
	state := &memState{m: map[string][]byte{}}
	cfg := Config{PlexURL: plexSrv.URL, PlexToken: "plex-token", SiloAPIKey: "sa_admin", WebhookSecret: "s",
		OnlyOverlaid: true, SyncCollections: true, DeleteRemoved: true}

	st, _ := loadState(context.Background(), state)
	sum := &runSummary{}
	s := &syncer{cfg: cfg, catalog: catalog, plex: newPlexClient(plexSrv.URL, "plex-token"),
		silo: newSiloClient(siloSrv.URL, "sa_admin"), state: st, log: func(string, ...any) {}, summary: sum}
	if err := s.run(context.Background()); err != nil || sum.PostersApplied != 3 {
		t.Fatalf("sync: %v %+v", err, sum)
	}
	// Silo locked images on apply; mv-a also has an unrelated lock the user set.
	silo.locks["mv-a"] = []int{3, 10}
	silo.locks["mv-b"] = []int{10}
	silo.locks["sr-s"] = []int{10}

	st, _ = loadState(context.Background(), state)
	tracked := trackedSections(st, map[string]string{"uuid-movies": "Movies", "uuid-tv": "TV Shows"})
	if len(tracked) != 2 || tracked[0].Title != "Movies" || tracked[0].Posters != 2 || tracked[0].Collections != 1 {
		t.Fatalf("tracked = %+v", tracked)
	}

	rsum := &runSummary{}
	r := &reverter{silo: newSiloClient(siloSrv.URL, "sa_admin"), state: st, progress: &revertProgress{},
		log: func(string, ...any) {}, summary: rsum}
	if err := r.run(context.Background(), map[string]bool{"uuid-movies": true}, true); err != nil {
		t.Fatal(err)
	}
	if rsum.ErrorCount != 0 || rsum.PostersReverted != 2 || rsum.CollectionsDeleted != 1 {
		t.Fatalf("revert summary = %+v", rsum)
	}
	if len(silo.locks["mv-a"]) != 1 || silo.locks["mv-a"][0] != 3 || len(silo.locks["mv-b"]) != 0 {
		t.Fatalf("locks after revert = %v", silo.locks)
	}
	if !slicesContains(silo.locks["sr-s"], 10) || len(silo.refreshed) != 2 {
		t.Fatalf("TV item was touched: locks %v refreshed %v", silo.locks, silo.refreshed)
	}
	if r.progress.done.Load() != 2 || r.progress.total.Load() != 2 {
		t.Fatalf("progress = %d/%d", r.progress.done.Load(), r.progress.total.Load())
	}

	// Movies is forgotten; TV is still tracked.
	st, _ = loadState(context.Background(), state)
	tracked = trackedSections(st, map[string]string{"uuid-tv": "TV Shows"})
	if len(tracked) != 1 || tracked[0].Title != "TV Shows" || tracked[0].Posters != 1 {
		t.Fatalf("tracked after revert = %+v", tracked)
	}
}

func TestRevertFormGuards(t *testing.T) {
	p := &plugin{log: hclog.NewNullLogger()}
	r := &routesServer{p: p}
	post := func(body string) int32 {
		resp, _ := r.Handle(context.Background(), &pluginv1.HandleHTTPRequest{Method: "POST", Path: "/revert", Body: []byte(body)})
		return resp.GetStatusCode()
	}
	tok := p.token()
	if code := post("library=uuid-movies&confirm=on"); code != http.StatusForbidden {
		t.Fatalf("missing token -> %d", code)
	}
	if code := post("token=" + tok + "&library=uuid-movies"); code != http.StatusBadRequest {
		t.Fatalf("missing confirm -> %d", code)
	}
	if code := post("token=" + tok + "&confirm=on"); code != http.StatusBadRequest {
		t.Fatalf("no library -> %d", code)
	}
	p.mu.Lock()
	p.running = true
	p.mu.Unlock()
	if code := post("token=" + tok + "&confirm=on&library=uuid-movies"); code != http.StatusConflict {
		t.Fatalf("busy -> %d", code)
	}
}

func TestStatusPageRendersRevertSection(t *testing.T) {
	var b strings.Builder
	err := statusPage.Execute(&b, map[string]any{
		"WebhookURL": "x", "FormToken": "tok", "Running": true, "Mode": "revert",
		"Progress":         progressView{Running: true, Mode: "revert", Phase: "Reverting posters", Done: 3, Total: 9, Started: "2026-10-24T02:40:13Z"},
		"Last":             &runSummary{StartedAt: "2026-10-24T02:40:13Z", FinishedAt: "2026-10-24T02:43:14Z", Trigger: "revert", PostersReverted: 5},
		"Tracked":          []trackedSection{{ID: "uuid-movies", Title: "Movies", Posters: 10, Collections: 2}},
		"Libraries":        &libraryView{Winner: "highest", Positions: []int{1}, Options: []libraryOption{{ID: "uuid-movies", Title: "Movies", Kind: "Movies"}}},
		"CollectionCounts": map[string]int{"uuid-movies": 2},
	})
	out := b.String()
	for _, want := range []string{`value="uuid-movies"`, "3 of 9", "Posters reverted", `name="token" value="tok"`,
		"Reverting</span>", `action="reset-collections"`, `datetime="2026-10-24T02:40:13Z"`, "took 3m 1s", "Sat, Oct 24, 2:40 AM UTC"} {
		if err != nil || !strings.Contains(out, want) {
			t.Fatalf("render missing %q (err %v)\n%s", want, err, out)
		}
	}
}

func TestTook(t *testing.T) {
	for _, c := range []struct{ a, b, want string }{
		{"2026-10-24T02:40:13Z", "2026-10-24T02:40:40Z", "27s"},
		{"2026-10-24T02:40:13Z", "2026-10-24T02:43:14Z", "3m 1s"},
		{"2026-10-24T02:40:13Z", "2026-10-24T04:05:13Z", "1h 25m"},
		{"bad", "2026-10-24T04:05:13Z", ""},
	} {
		if got := took(c.a, c.b); got != c.want {
			t.Errorf("took(%s,%s) = %q, want %q", c.a, c.b, got, c.want)
		}
	}
}

func TestProgressEndpoint(t *testing.T) {
	p := &plugin{log: hclog.NewNullLogger()}
	r := &routesServer{p: p}
	get := func() progressView {
		resp, _ := r.Handle(context.Background(), &pluginv1.HandleHTTPRequest{Method: "GET", Path: "/progress"})
		var v progressView
		_ = json.Unmarshal(resp.GetBody(), &v)
		return v
	}
	if v := get(); v.Running {
		t.Fatalf("idle plugin reports running: %+v", v)
	}
	p.mu.Lock()
	p.running, p.mode = true, "sync"
	p.mu.Unlock()
	p.prog.start()
	p.prog.set("Applying posters", 40)
	p.prog.add(7)
	if v := get(); !v.Running || v.Phase != "Applying posters" || v.Done != 7 || v.Total != 40 || v.Mode != "sync" {
		t.Fatalf("progress = %+v", v)
	}
}

func TestResetCollectionsDeletesOnlyPluginCollections(t *testing.T) {
	silo := &fakeSilo{collTitle: map[string]string{"c1": "Marvel", "c2": "Lost one", "c3": "My own", "c4": "TV one"},
		collSlug: map[string]string{"c1": "kometa-1-marvel", "c2": "kometa-1-lost-one", "c3": "favorites", "c4": "kometa-2-tv-one"},
		collLibs: map[string][]int{}, collItems: map[string]map[string]bool{}}
	srv := httptest.NewServer(silo.handler(t))
	defer srv.Close()
	state := &memState{m: map[string][]byte{}}
	st, _ := loadState(context.Background(), state)
	st.setCollection("uuid-movies/marvel", &collectionRecord{SiloID: "c1"})
	st.setCollection("uuid-tv/tv one", &collectionRecord{SiloID: "c4"})
	st.setPosterThumb("uuid-movies/10", "/t|mv-a")
	_ = st.flush(context.Background())

	sum := &runSummary{}
	movies := plexSection{Key: "1", Title: "Movies", Type: "movie", UUID: "uuid-movies"}
	if err := resetCollections(context.Background(), newSiloClient(srv.URL, "sa_admin"), st, []plexSection{movies}, sum, &syncProgress{}); err != nil {
		t.Fatal(err)
	}
	if sum.CollectionsDeleted != 2 || silo.collTitle["c1"] != "" || silo.collTitle["c2"] != "" {
		t.Fatalf("should delete tracked and lost Movies collections: %+v %v", sum, silo.collTitle)
	}
	if silo.collTitle["c3"] == "" || silo.collTitle["c4"] == "" {
		t.Fatalf("must not touch user or other-library collections: %v", silo.collTitle)
	}
	st, _ = loadState(context.Background(), state)
	if st.collection("uuid-movies/marvel") != nil || st.collection("uuid-tv/tv one") == nil || st.posterThumb("uuid-movies/10") == "" {
		t.Fatal("state should forget only the reset library's collections, and keep posters")
	}
}

func TestRateLimitedRequestsAreRetried(t *testing.T) {
	silo := &fakeSilo{collItems: map[string]map[string]bool{"c1": {}}, collTitle: map[string]string{"c1": "Marvel"}, throttle: 2}
	srv := httptest.NewServer(silo.handler(t))
	defer srv.Close()
	c := newSiloClient(srv.URL, "sa_admin")
	start := time.Now()
	if err := c.AddCollectionItem(context.Background(), "c1", "mv-a", 0); err != nil {
		t.Fatalf("add after 429s: %v", err)
	}
	if !silo.collItems["c1"]["mv-a"] || time.Since(start) < 2*time.Second {
		t.Fatalf("expected two waits honoring Retry-After; items %v after %v", silo.collItems["c1"], time.Since(start))
	}
}

func TestPacerStaysUnderSiloLimit(t *testing.T) {
	p := newPacer(siloRequestsPerSecond)
	start := time.Now()
	var wg sync.WaitGroup
	for range 31 {
		wg.Add(1)
		go func() { defer wg.Done(); _ = p.wait(context.Background()) }()
	}
	wg.Wait()
	// 31 calls at 15/s need at least 2 seconds, so never more than 20 in any second.
	if elapsed := time.Since(start); elapsed < 1900*time.Millisecond {
		t.Fatalf("31 paced calls took only %v", elapsed)
	}
}

func TestLibrarySelection(t *testing.T) {
	st := &memState{m: map[string][]byte{}}
	ctx := context.Background()
	movies := plexSection{Key: "1", Title: "Movies", Type: "movie", UUID: "uuid-movies"}
	tv := plexSection{Key: "2", Title: "TV Shows", Type: "show", UUID: "uuid-tv"}

	// Never saved: the legacy text setting applies (blank = everything).
	sel, explicit, err := loadSelection(ctx, st)
	if err != nil || explicit {
		t.Fatalf("fresh selection: %v %v", explicit, err)
	}
	pick := selector(Config{}, sel, explicit)
	if !pick(movies) || !pick(tv) {
		t.Fatal("blank legacy setting should include every library")
	}
	pick = selector(Config{PlexLibraries: []string{"tv shows"}}, sel, explicit)
	if pick(movies) || !pick(tv) {
		t.Fatal("legacy text setting not honored")
	}

	// Saved with nothing ticked: nothing syncs, even with a legacy value set.
	if err := saveSelection(ctx, st, []string{}); err != nil {
		t.Fatal(err)
	}
	sel, explicit, _ = loadSelection(ctx, st)
	pick = selector(Config{PlexLibraries: []string{"TV Shows"}}, sel, explicit)
	if !explicit || pick(movies) || pick(tv) {
		t.Fatal("an empty saved checklist must select nothing")
	}

	// Saved by UUID: renaming the library in Plex keeps it selected.
	_ = saveSelection(ctx, st, []string{"uuid-tv"})
	sel, explicit, _ = loadSelection(ctx, st)
	pick = selector(Config{}, sel, explicit)
	renamed := tv
	renamed.Title = "Series"
	if pick(movies) || !pick(renamed) {
		t.Fatal("selection should follow the library UUID")
	}
}

func TestSyncWithNoLibrariesSelected(t *testing.T) {
	plex := &fakePlex{movieThumbA: "/x"}
	plexSrv := httptest.NewServer(plex.handler(t))
	defer plexSrv.Close()
	silo := &fakeSilo{applied: map[string]string{}, collTitle: map[string]string{}}
	siloSrv := httptest.NewServer(silo.handler(t))
	defer siloSrv.Close()
	st, _ := loadState(context.Background(), &memState{m: map[string][]byte{}})
	sum := &runSummary{}
	s := &syncer{cfg: Config{SyncCollections: true}, catalog: &fakeCatalog{}, plex: newPlexClient(plexSrv.URL, "plex-token"),
		silo: newSiloClient(siloSrv.URL, "sa_admin"), state: st, log: func(string, ...any) {},
		summary: sum, selected: func(plexSection) bool { return false }}
	if err := s.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if sum.Note == "" || sum.ErrorCount != 0 || silo.writes != 0 || sum.PlexItems != 0 {
		t.Fatalf("expected a quiet no-op: %+v writes %d", sum, silo.writes)
	}
}

// twoLibraryPlex serves "Movies" and "Movies 4k", both holding the same film
// (TMDB 100), plus two shows in "TV Shows" and "TV Shows 4K".
type twoLibraryPlex struct {
	mu                 sync.Mutex
	hdThumb, uhdThumb  string
	tvThumb, tv4kThumb string
}

func (f *twoLibraryPlex) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		write := func(v any) { _ = json.NewEncoder(w).Encode(map[string]any{"MediaContainer": v}) }
		item := func(rk, typ, tmdb, thumb, res string) map[string]any {
			m := map[string]any{"ratingKey": rk, "title": "Film", "type": typ, "thumb": thumb, "guid": "plex://x/" + rk,
				"Guid": []map[string]string{{"id": "tmdb://" + tmdb}}}
			if res != "" {
				m["Media"] = []map[string]string{{"videoResolution": res}}
			}
			return m
		}
		switch r.URL.Path {
		case "/library/sections":
			// Plex lists the 4K library first here, so library order alone would pick the wrong copy for TV.
			write(map[string]any{"Directory": []map[string]string{
				{"key": "1", "title": "Movies", "type": "movie", "uuid": "u-hd"},
				{"key": "2", "title": "Movies 4k", "type": "movie", "uuid": "u-uhd"},
				{"key": "3", "title": "TV Shows", "type": "show", "uuid": "u-tv"},
				{"key": "4", "title": "TV Shows 4K", "type": "show", "uuid": "u-tv4k"},
			}})
		case "/library/sections/1/label", "/library/sections/2/label", "/library/sections/3/label", "/library/sections/4/label":
			write(map[string]any{"Directory": []map[string]string{{"key": "9", "title": "Overlay"}}})
		case "/library/sections/1/all":
			write(map[string]any{"Metadata": []any{item("10", "movie", "100", f.hdThumb, "1080")}})
		case "/library/sections/2/all":
			// Named "Movies 4k" but resolution comes from the file itself.
			write(map[string]any{"Metadata": []any{item("20", "movie", "100", f.uhdThumb, "4k")}})
		case "/library/sections/3/all":
			write(map[string]any{"Metadata": []any{item("30", "show", "300", f.tvThumb, "")}})
		case "/library/sections/4/all":
			write(map[string]any{"Metadata": []any{item("40", "show", "300", f.tv4kThumb, "")}})
		case "/library/sections/1/collections", "/library/sections/2/collections", "/library/sections/3/collections", "/library/sections/4/collections":
			write(map[string]any{"Metadata": []any{}})
		default:
			if strings.Contains(r.URL.Path, "/thumb/") {
				_, _ = io.WriteString(w, "IMAGE:"+r.URL.Path)
				return
			}
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

func TestHighestResolutionWins(t *testing.T) {
	plex := &twoLibraryPlex{hdThumb: "/library/metadata/10/thumb/1", uhdThumb: "/library/metadata/20/thumb/1",
		tvThumb: "/library/metadata/30/thumb/1", tv4kThumb: "/library/metadata/40/thumb/1"}
	plexSrv := httptest.NewServer(plex.handler())
	defer plexSrv.Close()
	silo := &fakeSilo{routes: &routesServer{p: &plugin{}}, applied: map[string]string{}, collTitle: map[string]string{}}
	siloSrv := httptest.NewServer(silo.handler(t))
	defer siloSrv.Close()
	catalog := &fakeCatalog{byType: map[string]map[string]runtimehost.MediaPresence{
		"movie": {"100": {MediaID: "mv-film", LibraryID: "3"}},
		"tv":    {"300": {MediaID: "sr-show", LibraryID: "4"}},
	}}
	state := &memState{m: map[string][]byte{}}
	cfg := Config{OnlyOverlaid: true}
	run := func() *runSummary {
		t.Helper()
		st, _ := loadState(context.Background(), state)
		sum := &runSummary{}
		s := &syncer{cfg: cfg, catalog: catalog, plex: newPlexClient(plexSrv.URL, "t"), silo: newSiloClient(siloSrv.URL, "sa_admin"),
			state: st, log: func(string, ...any) {}, summary: sum}
		if err := s.run(context.Background()); err != nil || sum.ErrorCount != 0 {
			t.Fatalf("run: %v %+v", err, sum)
		}
		return sum
	}
	set := func(fn func()) { plex.mu.Lock(); fn(); plex.mu.Unlock() }

	sum := run()
	if silo.applied["mv-film"] != "IMAGE:/library/metadata/20/thumb/1" || silo.applied["sr-show"] != "IMAGE:/library/metadata/40/thumb/1" {
		t.Fatalf("4K copies should win: %v", silo.applied)
	}
	if sum.PostersApplied != 2 || sum.DuplicatesSkipped != 2 {
		t.Fatalf("run 1 = %+v", sum)
	}

	// Kometa updates only the 1080p copy: Silo keeps the 4K poster.
	set(func() { plex.hdThumb = "/library/metadata/10/thumb/2" })
	writes := silo.writes
	if sum = run(); sum.PostersApplied != 0 || silo.writes != writes {
		t.Fatalf("1080p change must not override 4K: %+v", sum)
	}

	// Kometa updates the 4K copy: Silo gets it.
	set(func() { plex.uhdThumb = "/library/metadata/20/thumb/2" })
	if sum = run(); sum.PostersApplied != 1 || silo.applied["mv-film"] != "IMAGE:/library/metadata/20/thumb/2" {
		t.Fatalf("4K change not applied: %+v %v", sum, silo.applied)
	}
}

func TestHighestResolutionCorrectsOlderRuns(t *testing.T) {
	// State as an older version left it: both copies recorded as pushed, and
	// the 1080p one (pushed last) is what Silo shows.
	plex := &twoLibraryPlex{hdThumb: "/library/metadata/10/thumb/1", uhdThumb: "/library/metadata/20/thumb/1",
		tvThumb: "/library/metadata/30/thumb/1", tv4kThumb: "/library/metadata/40/thumb/1"}
	plexSrv := httptest.NewServer(plex.handler())
	defer plexSrv.Close()
	silo := &fakeSilo{routes: &routesServer{p: &plugin{}}, applied: map[string]string{"mv-film": "IMAGE:/library/metadata/10/thumb/1"}, collTitle: map[string]string{}}
	siloSrv := httptest.NewServer(silo.handler(t))
	defer siloSrv.Close()
	catalog := &fakeCatalog{byType: map[string]map[string]runtimehost.MediaPresence{"movie": {"100": {MediaID: "mv-film", LibraryID: "3"}}}}
	state := &memState{m: map[string][]byte{}}
	st, _ := loadState(context.Background(), state)
	st.setPosterThumb("u-hd/10", "/library/metadata/10/thumb/1|mv-film")
	st.setPosterThumb("u-uhd/20", "/library/metadata/20/thumb/1|mv-film")
	_ = st.flush(context.Background())

	run := func() *runSummary {
		st, _ := loadState(context.Background(), state)
		sum := &runSummary{}
		s := &syncer{cfg: Config{OnlyOverlaid: true}, catalog: catalog, plex: newPlexClient(plexSrv.URL, "t"), silo: newSiloClient(siloSrv.URL, "sa_admin"),
			state: st, log: func(string, ...any) {}, summary: sum}
		if err := s.run(context.Background()); err != nil {
			t.Fatal(err)
		}
		return sum
	}
	if sum := run(); sum.PostersApplied != 1 || silo.applied["mv-film"] != "IMAGE:/library/metadata/20/thumb/1" {
		t.Fatalf("first run after upgrade should re-push the 4K poster once: %+v %v", sum, silo.applied)
	}
	if sum := run(); sum.PostersApplied != 0 {
		t.Fatalf("correction should run only once: %+v", sum)
	}
}

func TestDuplicatePolicyChoices(t *testing.T) {
	plex := &twoLibraryPlex{hdThumb: "/library/metadata/10/thumb/1", uhdThumb: "/library/metadata/20/thumb/1",
		tvThumb: "/library/metadata/30/thumb/1", tv4kThumb: "/library/metadata/40/thumb/1"}
	plexSrv := httptest.NewServer(plex.handler())
	defer plexSrv.Close()
	silo := &fakeSilo{routes: &routesServer{p: &plugin{}}, applied: map[string]string{}, collTitle: map[string]string{}}
	siloSrv := httptest.NewServer(silo.handler(t))
	defer siloSrv.Close()
	catalog := &fakeCatalog{byType: map[string]map[string]runtimehost.MediaPresence{
		"movie": {"100": {MediaID: "mv-film", LibraryID: "3"}},
		"tv":    {"300": {MediaID: "sr-show", LibraryID: "4"}},
	}}
	state := &memState{m: map[string][]byte{}}
	run := func(policy librarySelection) *runSummary {
		t.Helper()
		policy.Winner = normalizeWinner(policy.Winner)
		st, _ := loadState(context.Background(), state)
		sum := &runSummary{}
		s := &syncer{cfg: Config{OnlyOverlaid: true}, catalog: catalog, plex: newPlexClient(plexSrv.URL, "t"),
			silo: newSiloClient(siloSrv.URL, "sa_admin"), state: st, log: func(string, ...any) {}, summary: sum, rank: policy.rankFunc(), policy: policy.signature()}
		if err := s.run(context.Background()); err != nil || sum.ErrorCount != 0 {
			t.Fatalf("run: %v %+v", err, sum)
		}
		return sum
	}

	// Lowest resolution: the 1080p copies win.
	run(librarySelection{Winner: winnerLowestRes})
	if silo.applied["mv-film"] != "IMAGE:/library/metadata/10/thumb/1" || silo.applied["sr-show"] != "IMAGE:/library/metadata/30/thumb/1" {
		t.Fatalf("lowest: %v", silo.applied)
	}
	if sum := run(librarySelection{Winner: winnerLowestRes}); sum.PostersApplied != 0 {
		t.Fatalf("same policy again should be a no-op: %+v", sum)
	}

	// Switch to highest: shared items are re-pushed once with the 4K copy.
	if sum := run(librarySelection{Winner: winnerHighestRes}); sum.PostersApplied != 2 ||
		silo.applied["mv-film"] != "IMAGE:/library/metadata/20/thumb/1" || silo.applied["sr-show"] != "IMAGE:/library/metadata/40/thumb/1" {
		t.Fatalf("switch to highest: %+v %v", sum, silo.applied)
	}

	// Library priority: "Movies" first for films, "TV Shows 4K" first for shows.
	prio := librarySelection{Winner: winnerPriority, Priority: map[string]int{"u-hd": 1, "u-uhd": 2, "u-tv4k": 3, "u-tv": 4}}
	if sum := run(prio); sum.PostersApplied != 2 ||
		silo.applied["mv-film"] != "IMAGE:/library/metadata/10/thumb/1" || silo.applied["sr-show"] != "IMAGE:/library/metadata/40/thumb/1" {
		t.Fatalf("priority: %+v %v", sum, silo.applied)
	}
	if sum := run(prio); sum.PostersApplied != 0 {
		t.Fatalf("priority again should be a no-op: %+v", sum)
	}

	// Reordering priorities counts as a new choice.
	prio.Priority = map[string]int{"u-uhd": 1, "u-hd": 2, "u-tv": 3, "u-tv4k": 4}
	if sum := run(prio); silo.applied["mv-film"] != "IMAGE:/library/metadata/20/thumb/1" || silo.applied["sr-show"] != "IMAGE:/library/metadata/30/thumb/1" {
		t.Fatalf("reordered priority: %+v %v", sum, silo.applied)
	}
}

func TestSaveLibrariesForm(t *testing.T) {
	sel := librarySelection{}
	form, _ := url.ParseQuery("library=u-hd&library=u-uhd&winner=priority&priority_u-hd=2&priority_u-uhd=1&priority_bogus=x")
	sel.IDs = form["library"]
	sel.Winner = normalizeWinner(form.Get("winner"))
	if sel.Winner != winnerPriority || normalizeWinner("nonsense") != winnerHighestRes {
		t.Fatal("winner normalization")
	}
	st := &memState{m: map[string][]byte{}}
	_ = savePolicy(context.Background(), st, librarySelection{IDs: []string{"b", "a"}, Winner: "priority", Priority: map[string]int{"a": 1}})
	got, err := loadPolicy(context.Background(), st)
	if err != nil || got.Winner != winnerPriority || got.Priority["a"] != 1 || got.IDs[0] != "a" {
		t.Fatalf("policy round trip: %+v %v", got, err)
	}
	if p, _ := loadPolicy(context.Background(), &memState{m: map[string][]byte{}}); p.Winner != winnerHighestRes {
		t.Fatal("default policy should be highest resolution")
	}
}

// rekeyCatalog returns an item's old Silo ID on the first lookup and its new
// one afterwards, as when Silo re-keys a series mid-sync.
type rekeyCatalog struct {
	mu     sync.Mutex
	calls  map[string]int
	before map[string]string
	after  map[string]string
}

func (c *rekeyCatalog) CheckMediaPresence(_ context.Context, _, _ string, ids []string) (map[string]runtimehost.MediaPresence, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := map[string]runtimehost.MediaPresence{}
	for _, id := range ids {
		c.calls[id]++
		src := c.before
		if c.calls[id] > 1 {
			src = c.after
		}
		if m, ok := src[id]; ok {
			out[id] = runtimehost.MediaPresence{MediaID: m, LibraryID: "3"}
		}
	}
	return out, nil
}

type collectionHarness struct {
	t     *testing.T
	plex  *fakePlex
	silo  *fakeSilo
	cat   presenceLookup
	state *memState
	libs  []siloLibrary
	mapg  map[string]int
	plexS *httptest.Server
	siloS *httptest.Server
}

func newHarness(t *testing.T, cat presenceLookup) *collectionHarness {
	h := &collectionHarness{t: t, cat: cat, state: &memState{m: map[string][]byte{}}}
	h.plex = &fakePlex{movieThumbA: "/library/metadata/10/thumb/1", collMembers: []string{"100", "200"}, collections: true}
	h.plexS = httptest.NewServer(h.plex.handler(t))
	h.silo = &fakeSilo{routes: &routesServer{p: &plugin{}}, applied: map[string]string{}, collTitle: map[string]string{},
		collLibs: map[string][]int{}, collItems: map[string]map[string]bool{}, collPost: map[string]string{}, collSlug: map[string]string{},
		locks: map[string][]int{}, jobPolls: map[string]int{}, goneIDs: map[string]bool{}}
	h.siloS = httptest.NewServer(h.silo.handler(t))
	t.Cleanup(func() { h.plexS.Close(); h.siloS.Close() })
	return h
}

func (h *collectionHarness) run() *runSummary {
	h.t.Helper()
	st, _ := loadState(context.Background(), h.state)
	sum := &runSummary{}
	s := &syncer{cfg: Config{OnlyOverlaid: true, SyncCollections: true, DeleteRemoved: true}, catalog: h.cat,
		plex: newPlexClient(h.plexS.URL, "plex-token"), silo: newSiloClient(h.siloS.URL, "sa_admin"),
		state: st,
		log:   func(string, ...any) {}, summary: sum, siloLibraries: h.libs, libraryMap: h.mapg, progress: &syncProgress{}}
	if err := s.run(context.Background()); err != nil {
		h.t.Fatal(err)
	}
	return sum
}

func TestRekeyedItemIsRetriedWithCurrentID(t *testing.T) {
	cat := &rekeyCatalog{calls: map[string]int{},
		before: map[string]string{"100": "movie-imdb-tt1", "200": "mv-b", "300": "series-imdb-tt9"},
		after:  map[string]string{"100": "movie-imdb-tt1", "200": "mv-b", "300": "series-tvdb-42"}}
	h := newHarness(t, cat)
	h.silo.goneIDs["series-imdb-tt9"] = true
	h.plex.collections = false
	sum := h.run()
	if sum.ErrorCount != 0 || h.silo.applied["series-tvdb-42"] == "" || sum.PostersApplied != 3 {
		t.Fatalf("re-keyed show should be retried with its new ID: %+v applied %v", sum, h.silo.applied)
	}
	st, _ := loadState(context.Background(), h.state)
	if v := st.posterThumb("uuid-tv/20"); !strings.HasSuffix(v, "|series-tvdb-42") {
		t.Fatalf("state should record the new ID, got %q", v)
	}
}

func TestItemRemovedFromSiloIsNotAnError(t *testing.T) {
	cat := &rekeyCatalog{calls: map[string]int{},
		before: map[string]string{"100": "mv-a", "200": "mv-b", "300": "series-gone"},
		after:  map[string]string{"100": "mv-a", "200": "mv-b"}}
	h := newHarness(t, cat)
	h.silo.goneIDs["series-gone"] = true
	h.plex.collections = false
	sum := h.run()
	if sum.ErrorCount != 0 || sum.NotInSilo != 1 {
		t.Fatalf("removed item should count as not in Silo: %+v", sum)
	}
}

func TestCollectionsGoToMappedLibrary(t *testing.T) {
	cat := &fakeCatalog{byType: map[string]map[string]runtimehost.MediaPresence{
		"movie": {"100": {MediaID: "mv-a", LibraryID: "3"}, "200": {MediaID: "mv-b", LibraryID: "3"}},
		"tv":    {"300": {MediaID: "sr-s", LibraryID: "4"}},
	}}
	h := newHarness(t, cat)
	// Silo reports the members in library 3, but Plex "Movies" maps to "Movies" (11).
	h.libs = []siloLibrary{{ID: 3, Name: "Movies 4K", Type: "movie"}, {ID: 11, Name: "Movies", Type: "movie"}, {ID: 4, Name: "TV", Type: "tv"}}
	h.run()
	var id string
	for k, v := range h.silo.collTitle {
		if v == "Marvel" {
			id = k
		}
	}
	if id == "" || len(h.silo.collLibs[id]) != 1 || h.silo.collLibs[id][0] != 11 {
		t.Fatalf("Marvel should be in library 11: %v", h.silo.collLibs)
	}
	// Overriding the mapping moves the existing collection; no new one is made.
	h.mapg = map[string]int{"uuid-movies": 3}
	sum := h.run()
	if len(h.silo.collTitle) != 1 || h.silo.collLibs[id][0] != 3 || sum.CollectionsUpdated != 1 {
		t.Fatalf("override should move Marvel to library 3: %+v %v", sum, h.silo.collLibs)
	}
}

func TestCollectionAdoptedNotDuplicated(t *testing.T) {
	cat := &fakeCatalog{byType: map[string]map[string]runtimehost.MediaPresence{
		"movie": {"100": {MediaID: "mv-a", LibraryID: "3"}, "200": {MediaID: "mv-b", LibraryID: "3"}}}}
	h := newHarness(t, cat)
	// Created by an earlier run that was cut off before it could record it.
	h.silo.collTitle["c9"], h.silo.collSlug["c9"], h.silo.collLibs["c9"] = "Marvel", "kometa-1-marvel", []int{3}
	h.silo.collItems["c9"] = map[string]bool{"mv-a": true}
	sum := h.run()
	if len(h.silo.collTitle) != 1 || sum.CollectionsCreated != 0 || !h.silo.collItems["c9"]["mv-b"] || sum.ErrorCount != 0 {
		t.Fatalf("orphan should be adopted: %+v titles %v items %v", sum, h.silo.collTitle, h.silo.collItems["c9"])
	}
}

func TestCollectionDeletedInSiloIsRecreated(t *testing.T) {
	cat := &fakeCatalog{byType: map[string]map[string]runtimehost.MediaPresence{
		"movie": {"100": {MediaID: "mv-a", LibraryID: "3"}, "200": {MediaID: "mv-b", LibraryID: "3"}}}}
	h := newHarness(t, cat)
	h.run()
	for id := range h.silo.collTitle {
		delete(h.silo.collTitle, id) // deleted by hand in Silo
	}
	sum := h.run()
	if sum.CollectionsCreated != 1 || len(h.silo.collTitle) != 1 || sum.ErrorCount != 0 {
		t.Fatalf("deleted collection should be recreated: %+v %v", sum, h.silo.collTitle)
	}
}

func TestAutoMatch(t *testing.T) {
	libs := []siloLibrary{{1, "Movies", "movie"}, {2, "Movies 4K", "movie"}, {3, "TV", "tv"}, {4, "TV 4K", "tv"}}
	cases := map[string]int{"Movies": 1, "Movies 4k": 2, "TV Shows": 3, "TV Shows 4k": 4}
	for title, want := range cases {
		typ := "movie"
		if strings.HasPrefix(title, "TV") {
			typ = "show"
		}
		if got := autoMatch(plexSection{Title: title, Type: typ}, libs); got != want {
			t.Errorf("autoMatch(%q) = %d, want %d", title, got, want)
		}
	}
	// Two plain TV libraries: ambiguous, so no automatic match.
	amb := append(libs, siloLibrary{5, "Anime", "tv"})
	if got := autoMatch(plexSection{Title: "TV Shows", Type: "show"}, amb); got != 0 {
		t.Errorf("ambiguous match = %d, want 0", got)
	}
	// An explicit choice wins, unless it no longer fits.
	if got := targetLibrary(plexSection{UUID: "u", Title: "TV Shows", Type: "show"}, libs, map[string]int{"u": 4}); got != 4 {
		t.Errorf("explicit = %d", got)
	}
	if got := targetLibrary(plexSection{UUID: "u", Title: "TV Shows", Type: "show"}, libs, map[string]int{"u": 1}); got != 3 {
		t.Errorf("incompatible explicit should fall back to auto, got %d", got)
	}
}

func TestItemLabel(t *testing.T) {
	if got := itemLabel(plexItem{Title: "The Head (2020)", Year: 2020}); got != "The Head (2020)" {
		t.Errorf("got %q", got)
	}
	if got := itemLabel(plexItem{Title: "The Jinx", Year: 2015}); got != "The Jinx (2015)" {
		t.Errorf("got %q", got)
	}
}

// comboPlex has "Marvel" in both Movies and Movies 4k, with different
// members and posters, plus a collection only the 4K library has.
type comboPlex struct {
	mu         sync.Mutex
	noMarvelHD bool
}

func (f *comboPlex) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		write := func(v any) { _ = json.NewEncoder(w).Encode(map[string]any{"MediaContainer": v}) }
		movie := func(tmdb string) map[string]any {
			return map[string]any{"ratingKey": "m" + tmdb, "type": "movie", "title": "t" + tmdb, "Guid": []map[string]string{{"id": "tmdb://" + tmdb}}}
		}
		coll := func(rk, title string) map[string]any {
			return map[string]any{"ratingKey": rk, "title": title, "type": "collection", "thumb": "/library/metadata/" + rk + "/thumb/1"}
		}
		switch r.URL.Path {
		case "/library/sections":
			write(map[string]any{"Directory": []map[string]string{
				{"key": "1", "title": "Movies", "type": "movie", "uuid": "u-hd"},
				{"key": "2", "title": "Movies 4k", "type": "movie", "uuid": "u-uhd"},
			}})
		case "/library/sections/1/label", "/library/sections/2/label":
			write(map[string]any{"Directory": []map[string]string{}})
		case "/library/sections/1/collections":
			var c []any
			if !f.noMarvelHD {
				c = append(c, coll("901", "Marvel"))
			}
			write(map[string]any{"Metadata": c})
		case "/library/sections/2/collections":
			write(map[string]any{"Metadata": []any{coll("902", "Marvel"), coll("903", "4K Only")}})
		case "/library/collections/901/children":
			write(map[string]any{"Metadata": []any{movie("100"), movie("200")}})
		case "/library/collections/902/children":
			write(map[string]any{"Metadata": []any{movie("100"), movie("300")}})
		case "/library/collections/903/children":
			write(map[string]any{"Metadata": []any{movie("300")}})
		default:
			if strings.Contains(r.URL.Path, "/thumb/") {
				w.Header().Set("Content-Type", "image/jpeg")
				_, _ = io.WriteString(w, "IMAGE:"+r.URL.Path)
				return
			}
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

type comboHarness struct {
	t     *testing.T
	plex  *comboPlex
	silo  *fakeSilo
	state *memState
	plexS *httptest.Server
	siloS *httptest.Server
}

func newComboHarness(t *testing.T) *comboHarness {
	h := &comboHarness{t: t, plex: &comboPlex{}, state: &memState{m: map[string][]byte{}}}
	h.plexS = httptest.NewServer(h.plex.handler())
	h.silo = &fakeSilo{routes: &routesServer{p: &plugin{}}, applied: map[string]string{}, collTitle: map[string]string{},
		collLibs: map[string][]int{}, collItems: map[string]map[string]bool{}, collPost: map[string]string{}, collSlug: map[string]string{},
		goneIDs: map[string]bool{}}
	h.siloS = httptest.NewServer(h.silo.handler(t))
	t.Cleanup(func() { h.plexS.Close(); h.siloS.Close() })
	return h
}

func (h *comboHarness) run(combined bool) *runSummary {
	h.t.Helper()
	st, _ := loadState(context.Background(), h.state)
	sum := &runSummary{}
	cat := &fakeCatalog{byType: map[string]map[string]runtimehost.MediaPresence{"movie": {
		"100": {MediaID: "mv-a", LibraryID: "1"}, "200": {MediaID: "mv-b", LibraryID: "1"}, "300": {MediaID: "mv-c", LibraryID: "2"}}}}
	s := &syncer{cfg: Config{OnlyOverlaid: true, SyncCollections: true, DeleteRemoved: true}, catalog: cat,
		plex: newPlexClient(h.plexS.URL, "t"), silo: newSiloClient(h.siloS.URL, "sa_admin"), state: st, log: func(string, ...any) {}, summary: sum,
		siloLibraries: []siloLibrary{{1, "Movies", "movie"}, {2, "Movies 4K", "movie"}}, combined: combined, progress: &syncProgress{}}
	if err := s.run(context.Background()); err != nil {
		h.t.Fatal(err)
	}
	if sum.ErrorCount != 0 {
		h.t.Fatalf("errors: %v", sum.Errors)
	}
	return sum
}

// byTitle returns the Silo collection IDs with a title.
func (h *comboHarness) byTitle(title string) []string {
	var ids []string
	for id, t := range h.silo.collTitle {
		if t == title {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

func TestCombinedCollections(t *testing.T) {
	h := newComboHarness(t)
	sum := h.run(true)
	marvel := h.byTitle("Marvel")
	if len(marvel) != 1 || sum.CollectionsCreated != 2 || sum.CollectionsChecked != 2 {
		t.Fatalf("one Marvel expected: %v %+v", marvel, sum)
	}
	id := marvel[0]
	if !slices.Equal(h.silo.collLibs[id], []int{1, 2}) {
		t.Fatalf("combined Marvel should be in both libraries: %v", h.silo.collLibs[id])
	}
	if len(h.silo.collItems[id]) != 3 || h.silo.collPost[id] != "IMAGE:/library/metadata/902/thumb/1" {
		t.Fatalf("members should be the union and the poster the 4K one: %v %q", h.silo.collItems[id], h.silo.collPost[id])
	}
	only := h.byTitle("4K Only")
	if len(only) != 1 || !slices.Equal(h.silo.collLibs[only[0]], []int{2}) {
		t.Fatalf("a 4K-only collection stays in the 4K library: %v", h.silo.collLibs)
	}
	if sum := h.run(true); sum.CollectionsCreated+sum.CollectionsUpdated != 0 {
		t.Fatalf("second run should change nothing: %+v", sum)
	}
	// "Marvel" removed from the 1080p library: the combined one stays, now 4K only.
	h.plex.mu.Lock()
	h.plex.noMarvelHD = true
	h.plex.mu.Unlock()
	h.run(true)
	if got := h.byTitle("Marvel"); len(got) != 1 || !slices.Equal(h.silo.collLibs[got[0]], []int{2}) {
		t.Fatalf("Marvel should remain, in the 4K library only: %v %v", got, h.silo.collLibs)
	}
}

func TestSwitchingCollectionModes(t *testing.T) {
	h := newComboHarness(t)
	h.run(false)
	if n := len(h.byTitle("Marvel")); n != 2 || len(h.silo.collTitle) != 3 {
		t.Fatalf("separate: two Marvels expected, got %d (%v)", n, h.silo.collTitle)
	}
	uhdMarvel := ""
	for _, id := range h.byTitle("Marvel") {
		if slices.Equal(h.silo.collLibs[id], []int{2}) {
			uhdMarvel = id
		}
	}

	// Separate -> combined: the 4K Marvel is kept and widened; the 1080p one goes.
	sum := h.run(true)
	if got := h.byTitle("Marvel"); len(got) != 1 || got[0] != uhdMarvel || sum.CollectionsCreated != 0 || sum.CollectionsDeleted != 1 {
		t.Fatalf("combine should keep %s: %v %+v", uhdMarvel, got, sum)
	}
	if !slices.Equal(h.silo.collLibs[uhdMarvel], []int{1, 2}) || len(h.silo.collItems[uhdMarvel]) != 3 {
		t.Fatalf("kept Marvel should span both libraries with all titles: %v %v", h.silo.collLibs[uhdMarvel], h.silo.collItems[uhdMarvel])
	}
	if len(h.silo.collTitle) != 2 {
		t.Fatalf("expected Marvel + 4K Only: %v", h.silo.collTitle)
	}
	if sum := h.run(true); sum.CollectionsCreated+sum.CollectionsUpdated+sum.CollectionsDeleted != 0 {
		t.Fatalf("settled combined run should change nothing: %+v", sum)
	}

	// Combined -> separate: the 4K library takes the combined Marvel back; 1080p gets a new one.
	sum = h.run(false)
	marvels := h.byTitle("Marvel")
	if len(marvels) != 2 || !slices.Contains(marvels, uhdMarvel) || sum.CollectionsCreated != 1 {
		t.Fatalf("separate again: %v %+v", marvels, sum)
	}
	if !slices.Equal(h.silo.collLibs[uhdMarvel], []int{2}) {
		t.Fatalf("4K Marvel should be back in the 4K library only: %v", h.silo.collLibs[uhdMarvel])
	}
	if sum := h.run(false); sum.CollectionsCreated+sum.CollectionsUpdated+sum.CollectionsDeleted != 0 {
		t.Fatalf("settled separate run should change nothing: %+v", sum)
	}
}

func TestResetTouchesCombinedCollections(t *testing.T) {
	h := newComboHarness(t)
	h.run(true)
	st, _ := loadState(context.Background(), h.state)
	tracked := trackedSections(st, map[string]string{"u-hd": "Movies", "u-uhd": "Movies 4k"})
	counts := map[string]int{}
	for _, tr := range tracked {
		counts[tr.ID] = tr.Collections
	}
	if counts["u-hd"] != 1 || counts["u-uhd"] != 2 {
		t.Fatalf("combined Marvel should count for both libraries: %v", counts)
	}
	sum := &runSummary{}
	hd := plexSection{Key: "1", Title: "Movies", Type: "movie", UUID: "u-hd"}
	if err := resetCollections(context.Background(), newSiloClient(h.siloS.URL, "sa_admin"), st, []plexSection{hd}, sum, &syncProgress{}); err != nil {
		t.Fatal(err)
	}
	if len(h.byTitle("Marvel")) != 0 || len(h.byTitle("4K Only")) != 1 || sum.CollectionsDeleted != 1 {
		t.Fatalf("reset of Movies should remove combined Marvel only: %v %+v", h.silo.collTitle, sum)
	}
}

func TestCollectionsOffNote(t *testing.T) {
	h := newComboHarness(t)
	st, _ := loadState(context.Background(), h.state)
	sum := &runSummary{}
	s := &syncer{cfg: Config{OnlyOverlaid: true}, catalog: &fakeCatalog{}, plex: newPlexClient(h.plexS.URL, "t"),
		silo: newSiloClient(h.siloS.URL, "sa_admin"), state: st, log: func(string, ...any) {}, summary: sum}
	if err := s.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sum.Note, "turned off") || sum.CollectionsChecked != 0 || len(h.silo.collTitle) != 0 {
		t.Fatalf("collections off: %+v", sum)
	}
}

func TestCollectionMemberRekeyedMidRun(t *testing.T) {
	cat := &rekeyCatalog{calls: map[string]int{},
		before: map[string]string{"100": "mv-a", "200": "movie-imdb-old"},
		after:  map[string]string{"100": "mv-a", "200": "movie-tvdb-new"}}
	h := newHarness(t, cat)
	h.silo.fkGone = map[string]bool{"movie-imdb-old": true}
	cat.calls["200"] = -1 // posters and the collection lookup both see the old ID; only the retry sees the new one
	sum := h.run()
	if cat.calls["200"] != 2 {
		t.Fatalf("expected a retry lookup, got %d lookups", cat.calls["200"]+1)
	}
	var id string
	for k, v := range h.silo.collTitle {
		if v == "Marvel" {
			id = k
		}
	}
	if sum.ErrorCount != 0 || !h.silo.collItems[id]["movie-tvdb-new"] || h.silo.collItems[id]["movie-imdb-old"] {
		t.Fatalf("member should be added under its new ID: %+v %v", sum, h.silo.collItems[id])
	}
}

// rankPlex serves one Movies collection whose order changes between runs,
// as UMTK re-ranks it.
type rankPlex struct {
	mu    sync.Mutex
	order []string // TMDB IDs in collection order
}

func (f *rankPlex) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		write := func(v any) { _ = json.NewEncoder(w).Encode(map[string]any{"MediaContainer": v}) }
		switch r.URL.Path {
		case "/library/sections":
			write(map[string]any{"Directory": []map[string]string{{"key": "1", "title": "Movies", "type": "movie", "uuid": "u-hd"}}})
		case "/library/sections/1/label":
			write(map[string]any{"Directory": []map[string]string{}})
		case "/library/sections/1/collections":
			write(map[string]any{"Metadata": []any{map[string]any{"ratingKey": "950", "title": "Trending", "type": "collection"}}})
		case "/library/collections/950/children":
			var kids []any
			for _, id := range f.order {
				kids = append(kids, map[string]any{"ratingKey": "m" + id, "type": "movie", "title": id, "Guid": []map[string]string{{"id": "tmdb://" + id}}})
			}
			write(map[string]any{"Metadata": kids})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

func TestCollectionOrderFollowsPlex(t *testing.T) {
	plex := &rankPlex{order: []string{"1", "2", "3", "4", "5"}}
	plexS := httptest.NewServer(plex.handler())
	defer plexS.Close()
	silo := &fakeSilo{collTitle: map[string]string{}, collLibs: map[string][]int{}, collItems: map[string]map[string]bool{},
		collPost: map[string]string{}, collSlug: map[string]string{}}
	siloS := httptest.NewServer(silo.handler(t))
	defer siloS.Close()
	movies := map[string]runtimehost.MediaPresence{}
	for _, id := range []string{"1", "2", "3", "4", "5", "6"} {
		movies[id] = runtimehost.MediaPresence{MediaID: "mv-" + id, LibraryID: "1"}
	}
	state := &memState{m: map[string][]byte{}}
	run := func() *runSummary {
		t.Helper()
		st, _ := loadState(context.Background(), state)
		sum := &runSummary{}
		s := &syncer{cfg: Config{OnlyOverlaid: true, SyncCollections: true}, catalog: &fakeCatalog{byType: map[string]map[string]runtimehost.MediaPresence{"movie": movies}},
			plex: newPlexClient(plexS.URL, "t"), silo: newSiloClient(siloS.URL, "sa_admin"), state: st,
			log: func(string, ...any) {}, summary: sum, siloLibraries: []siloLibrary{{1, "Movies", "movie"}}}
		if err := s.run(context.Background()); err != nil || sum.ErrorCount != 0 {
			t.Fatalf("run: %v %+v", err, sum)
		}
		return sum
	}
	set := func(o ...string) { plex.mu.Lock(); plex.order = o; plex.mu.Unlock() }
	want := func(o ...string) []string {
		var ids []string
		for _, id := range o {
			ids = append(ids, "mv-"+id)
		}
		return ids
	}
	var cid string
	run()
	for id := range silo.collTitle {
		cid = id
	}
	if got := silo.ordered(cid); !slices.Equal(got, want("1", "2", "3", "4", "5")) {
		t.Fatalf("first run order = %v", got)
	}

	// UMTK re-ranks, drops 5 and adds 6 at the top: Silo must match exactly.
	set("6", "3", "1", "4", "2")
	sum := run()
	if got := silo.ordered(cid); !slices.Equal(got, want("6", "3", "1", "4", "2")) || sum.CollectionsUpdated != 1 {
		t.Fatalf("re-ranked order = %v (%+v)", got, sum)
	}

	// Same order again: no reorder call.
	before := silo.reorders
	if sum := run(); silo.reorders != before || sum.CollectionsUpdated != 0 {
		t.Fatalf("unchanged order should not be resent: %d -> %d, %+v", before, silo.reorders, sum)
	}

	// Pure re-rank, same titles.
	set("2", "4", "1", "3", "6")
	run()
	if got := silo.ordered(cid); !slices.Equal(got, want("2", "4", "1", "3", "6")) {
		t.Fatalf("pure re-rank = %v", got)
	}

	// Someone added a title by hand in Silo: the one-call reorder is refused,
	// and positions are set one by one instead.
	silo.collItems[cid]["mv-extra"] = true
	silo.pos(cid)["mv-extra"] = 0
	set("6", "1", "2", "3", "4")
	if sum := run(); sum.ErrorCount != 0 {
		t.Fatalf("fallback errored: %+v", sum)
	}
	got := silo.ordered(cid)
	var mine []string
	for _, id := range got {
		if id != "mv-extra" {
			mine = append(mine, id)
		}
	}
	if !slices.Equal(mine, want("6", "1", "2", "3", "4")) {
		t.Fatalf("fallback order = %v", got)
	}
}

func TestCancelledSyncKeepsProgress(t *testing.T) {
	cat := &fakeCatalog{byType: map[string]map[string]runtimehost.MediaPresence{
		"movie": {"100": {MediaID: "mv-a", LibraryID: "3"}, "200": {MediaID: "mv-b", LibraryID: "3"}},
		"tv":    {"300": {MediaID: "sr-s", LibraryID: "4"}},
	}}
	h := newHarness(t, cat)
	h.plex.collections = false
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	applies := 0
	h.silo.onApply = func() { // cancel while the second poster is being applied
		if applies++; applies == 2 {
			cancel()
		}
	}

	st, _ := loadState(context.Background(), h.state)
	sum := &runSummary{}
	s := &syncer{cfg: Config{OnlyOverlaid: true}, catalog: h.cat,
		plex: newPlexClient(h.plexS.URL, "plex-token"), silo: newSiloClient(h.siloS.URL, "sa_admin"),
		state: st,
		log:   func(string, ...any) {}, summary: sum, progress: &syncProgress{}}
	err := s.run(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("run should report cancellation, got %v", err)
	}
	if sum.ErrorCount != 0 {
		t.Fatalf("cancellation should not count as errors: %v", sum.Errors)
	}
	if sum.PostersApplied < 1 || sum.PostersApplied == 3 {
		t.Fatalf("expected a partial run, applied %d", sum.PostersApplied)
	}
	if markCancelled(sum, err) != nil || !sum.Cancelled {
		t.Fatal("markCancelled should swallow the cancellation and flag the run")
	}

	// The next run skips what the cancelled one finished.
	first := sum.PostersApplied
	h.silo.onApply = nil
	again := h.run()
	if again.PostersApplied != 3-first || again.ErrorCount != 0 {
		t.Fatalf("resumed run applied %d (want %d), errors %v", again.PostersApplied, 3-first, again.Errors)
	}
}

func TestCancelEndpoint(t *testing.T) {
	p := &plugin{formToken: "tok", log: hclog.NewNullLogger()}
	r := &routesServer{p: p}
	post := func(body string) int {
		resp, _ := r.Handle(context.Background(), &pluginv1.HandleHTTPRequest{Method: "POST", Path: "/cancel", Body: []byte(body)})
		return int(resp.GetStatusCode())
	}
	if code := post("token=wrong"); code != http.StatusForbidden {
		t.Fatalf("bad token: %d", code)
	}
	if code := post("token=tok"); code != http.StatusOK {
		t.Fatalf("nothing running: %d", code)
	}
	p.mu.Lock()
	p.running = true
	ctx := p.withStop(context.Background())
	p.rerun = true
	p.mu.Unlock()
	if code := post("token=tok"); code != http.StatusAccepted {
		t.Fatalf("cancel running: %d", code)
	}
	if ctx.Err() == nil || p.rerun {
		t.Fatal("cancel should stop the run and drop the queued rerun")
	}
	if !p.endStop() {
		t.Fatal("endStop should report the cancellation")
	}
}

func newV2Silo(t *testing.T) (*fakeSilo, *SiloClient) {
	t.Helper()
	f := &fakeSilo{applied: map[string]string{}, collTitle: map[string]string{}, collLibs: map[string][]int{},
		collItems: map[string]map[string]bool{}, collPost: map[string]string{}, locks: map[string][]int{}, jobPolls: map[string]int{}}
	srv := httptest.NewServer(f.handler(t))
	t.Cleanup(srv.Close)
	return f, newSiloClient(srv.URL, "sa_admin")
}

func TestV2CollectionWritesUseETags(t *testing.T) {
	f, c := newV2Silo(t)
	ctx := context.Background()
	id, err := c.CreateCollection(ctx, "Marvel", "kometa-c-movie-marvel", []int{3, 5})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(f.collLibs[id], []int{3, 5}) {
		t.Fatalf("library ids sent as strings should round-trip: %v", f.collLibs[id])
	}
	list, err := c.ListCollections(ctx)
	if err != nil || len(list) != 1 || list[0].Slug != "kometa-c-movie-marvel" || !slices.Equal(list[0].LibraryIDs, []int{3, 5}) {
		t.Fatalf("list = %+v, %v", list, err)
	}

	// A collection edited in Silo between our read and write: 412, then one retry.
	f.staleOnce = true
	if err := c.UpdateCollection(ctx, id, "Marvel Cinematic Universe", []int{3}); err != nil {
		t.Fatalf("update should retry once after a stale ETag: %v", err)
	}
	if f.collTitle[id] != "Marvel Cinematic Universe" || !slices.Equal(f.collLibs[id], []int{3}) {
		t.Fatalf("update not applied: %q %v", f.collTitle[id], f.collLibs[id])
	}

	if err := c.SetCollectionPoster(ctx, id, []byte("\xff\xd8\xff\xe0poster")); err != nil || f.collPost[id] == "" {
		t.Fatalf("poster upload: %v", err)
	}

	f.inUse = map[string]bool{id: true}
	if err := c.DeleteCollection(ctx, id); !errors.Is(err, errConflict) || !strings.Contains(err.Error(), "page row") {
		t.Fatalf("delete of a collection a row shows should explain the 409: %v", err)
	}
	f.inUse = nil
	if err := c.DeleteCollection(ctx, id); err != nil || f.collTitle[id] != "" {
		t.Fatalf("delete: %v", err)
	}
	if err := c.DeleteCollection(ctx, id); err != nil {
		t.Fatalf("deleting a collection that's already gone is fine: %v", err)
	}
}

func TestV2ReorderKeepsUnknownItemsAndSkipsLargeCollections(t *testing.T) {
	f, c := newV2Silo(t)
	ctx := context.Background()
	id, _ := c.CreateCollection(ctx, "Trending", "kometa-1-trending", []int{3})
	for i, item := range []string{"a", "b", "mine", "c"} {
		if err := c.AddCollectionItem(ctx, id, item, i); err != nil {
			t.Fatal(err)
		}
	}
	// "mine" was added in Silo by hand; the plugin's order doesn't name it.
	if err := c.ReorderCollection(ctx, id, []string{"c", "b", "a"}); err != nil {
		t.Fatal(err)
	}
	if got := f.ordered(id); !slices.Equal(got, []string{"c", "b", "a", "mine"}) {
		t.Fatalf("order = %v", got)
	}
	writes := f.reorders
	if err := c.ReorderCollection(ctx, id, []string{"c", "b", "a"}); err != nil || f.reorders != writes {
		t.Fatalf("an unchanged order should not be written again: %v", err)
	}

	defer func(n int) { orderWindow = n }(orderWindow)
	orderWindow = 2
	if err := c.ReorderCollection(ctx, id, []string{"a", "b", "c"}); !errors.Is(err, errOrderTooLarge) {
		t.Fatalf("over Silo's reorder window should be errOrderTooLarge, got %v", err)
	}
}

func TestV2PosterUploadAndRefreshJob(t *testing.T) {
	f, c := newV2Silo(t)
	ctx := context.Background()
	if err := c.UploadPoster(ctx, "movie-tmdb-1", []byte("\xff\xd8\xff\xe0img")); err != nil || f.applied["movie-tmdb-1"] == "" {
		t.Fatalf("upload: %v", err)
	}
	f.goneIDs = map[string]bool{"movie-tmdb-2": true}
	if err := c.UploadPoster(ctx, "movie-tmdb-2", []byte("\xff\xd8\xff\xe0img")); !errors.Is(err, errNotFound) {
		t.Fatalf("re-keyed item should be errNotFound: %v", err)
	}
	job, err := c.RefreshItem(ctx, "movie-tmdb-1")
	if err != nil || job == "" {
		t.Fatal(err)
	}
	for {
		state, terminal, _, err := c.JobStatus(ctx, job)
		if err != nil {
			t.Fatal(err)
		}
		if terminal {
			if state != "succeeded" {
				t.Fatalf("state %q", state)
			}
			break
		}
	}
}

func TestRetiredAPIIsExplained(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		problem(w, http.StatusGone, "client_upgrade_required")
	}))
	defer srv.Close()
	err := newSiloClient(srv.URL, "k").Ping(context.Background())
	if err == nil || !strings.Contains(err.Error(), "update the plugin") {
		t.Fatalf("410 should tell the admin to update: %v", err)
	}
}

func TestHeartbeatOnlyWhileBusy(t *testing.T) {
	p := &plugin{log: hclog.NewNullLogger()}
	if p.busy() {
		t.Fatal("idle plugin should not keep Silo awake")
	}
	p.running = true
	if !p.busy() {
		t.Fatal("a running sync must keep the plugin alive")
	}
	p.running = false
	p.timer = time.AfterFunc(time.Hour, func() {})
	defer p.timer.Stop()
	if !p.busy() {
		t.Fatal("a scheduled webhook sync must keep the plugin alive")
	}
}

// recordRequest appends each request's method, path, headers and body to
// $RECORD_REQUESTS (one JSON object per line), for checking against Silo's
// OpenAPI contract outside the test.
var recordMu sync.Mutex

func recordRequest(r *http.Request) {
	file := os.Getenv("RECORD_REQUESTS")
	if file == "" {
		return
	}
	body, _ := io.ReadAll(r.Body)
	r.Body = io.NopCloser(bytes.NewReader(body))
	rec := map[string]any{"method": r.Method, "path": r.URL.Path, "content_type": r.Header.Get("Content-Type"),
		"if_match": r.Header.Get("If-Match")}
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/") {
		if mr, err := multipart.NewReader(bytes.NewReader(body), strings.Split(r.Header.Get("Content-Type"), "boundary=")[1]).ReadForm(32 << 20); err == nil {
			fields := map[string]any{}
			for k, v := range mr.Value {
				fields[k] = v[0]
			}
			for k := range mr.File {
				fields[k] = "<file>"
			}
			rec["form"] = fields
		}
	} else if len(body) > 0 {
		rec["json"] = json.RawMessage(body)
	}
	line, _ := json.Marshal(rec)
	recordMu.Lock()
	defer recordMu.Unlock()
	fh, err := os.OpenFile(file, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer fh.Close()
	_, _ = fh.Write(append(line, '\n'))
}
