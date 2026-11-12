package main

import (
	"os"
	"strings"
	"testing"
)

func TestWritePreview(t *testing.T) {
	dir := os.Getenv("PREVIEW_DIR")
	if dir == "" {
		t.Skip()
	}
	libs := []siloLibrary{{1, "Movies", "movie"}, {2, "Movies 4K", "movie"}, {3, "TV", "tv"}, {4, "TV 4K", "tv"}}
	opt := func(id, title, kind string, prio int, choices []siloLibrary, auto string) libraryOption {
		return libraryOption{ID: id, Title: title, Kind: kind, Selected: true, Priority: prio, MapChoices: choices, AutoName: auto}
	}
	var b strings.Builder
	err := statusPage.Execute(&b, map[string]any{
		"WebhookURL": "http://silo:8080/api/v2/plugin-content/plugins/12/kometa?secret=YOUR_WEBHOOK_SECRET",
		"FormToken":  "tok", "Running": true, "Mode": "sync",
		"Progress": progressView{Running: true, Mode: "sync", Phase: "Applying posters", Done: 412, Total: 971, Started: "2026-10-24T10:58:03Z"},
		"Libraries": &libraryView{Winner: "highest", CollMode: "combined", Positions: []int{1, 2, 3, 4}, Options: []libraryOption{
			opt("a", "Movies", "Movies", 3, libs[:2], "Movies"), opt("b", "Movies 4k", "Movies", 1, libs[:2], "Movies 4K"),
			opt("c", "TV Shows", "TV", 4, libs[2:], "TV"), opt("d", "TV Shows 4k", "TV", 2, libs[2:], "TV 4K")}},
		"CollectionCounts": map[string]int{"a": 0, "b": 0, "c": 44, "d": 39},
		"Last": &runSummary{StartedAt: "2026-10-24T02:40:13Z", FinishedAt: "2026-10-24T02:43:14Z", Trigger: "scheduled task",
			CollectionsChecked: 163, CollectionsCreated: 41, CollectionsUpdated: 39, PlexItems: 13588, PostersApplied: 971, PostersUnchanged: 11422, NotInSilo: 197, NoTMDBID: 24, DuplicatesSkipped: 974},
		"Tracked": []trackedSection{{ID: "a", Title: "Movies", Posters: 5900}, {ID: "c", Title: "TV Shows", Posters: 3499, Collections: 44}, {ID: "d", Title: "TV Shows 4k", Posters: 460, Collections: 39}},
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(dir+"/status.html", []byte(b.String()), 0o644)
}
