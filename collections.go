package main

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// Collections can be kept two ways:
//
//   - Separate: each Plex library's collections become their own Silo
//     collections, in that library's Silo library. "Marvel" in Movies and
//     "Marvel" in Movies 4k are two Silo collections.
//   - Combined: same-named collections from libraries of the same type become
//     one Silo collection, assigned to every Silo library they map to. Its
//     members are the union; its poster comes from the library that wins under
//     the duplicate-title choice (highest resolution, lowest, or priority).
//
// Switching reuses Silo collections where it can: combining keeps the winning
// library's collection and deletes the others; separating gives the combined
// collection to the library its poster came from, and the rest are created.

const (
	collectionsSeparate = "separate"
	collectionsCombined = "combined"
	combinedPrefix      = "combined:"
)

func normalizeCollectionMode(m string) string {
	if m == collectionsCombined {
		return m
	}
	return collectionsSeparate
}

func collectionKey(sec plexSection, title string) string {
	return sectionID(sec) + "/" + strings.ToLower(strings.TrimSpace(title))
}

func combinedKey(sectionType, title string) string {
	return combinedPrefix + sectionType + "/" + strings.ToLower(strings.TrimSpace(title))
}

// keyScope is the part of a record key before the title: a Plex library ID,
// or "combined:movie" / "combined:show".
func keyScope(key string) string {
	scope, _, _ := strings.Cut(key, "/")
	return scope
}

func isCombinedKey(key string) bool { return strings.HasPrefix(key, combinedPrefix) }

var slugJunk = regexp.MustCompile(`[^a-z0-9]+`)

func slugPart(title string) string {
	slug := strings.Trim(slugJunk.ReplaceAllString(strings.ToLower(title), "-"), "-")
	if slug == "" {
		slug = "collection"
	}
	return slug
}

func collectionSlug(sec plexSection, title string) string {
	return "kometa-" + sec.Key + "-" + slugPart(title)
}

func combinedSlug(sectionType, title string) string {
	return "kometa-c-" + sectionType + "-" + slugPart(title)
}

// collPart is one Plex library's copy of a collection.
type collPart struct {
	sec  plexSection
	coll plexItem
}

// collSpec is one Silo collection to keep in step: parts[0] is the copy whose
// poster and order win.
type collSpec struct {
	key, title, slug string
	parts            []collPart
}

func (c collSpec) sectionIDs() []string {
	ids := make([]string, 0, len(c.parts))
	for _, p := range c.parts {
		ids = append(ids, sectionID(p.sec))
	}
	return ids
}

// loadExistingCollections reads Silo's collections, so the run can tell when
// one it created was deleted in Silo and adopt one it created but never got
// to record.
func (s *syncer) loadExistingCollections(ctx context.Context) error {
	s.existing, s.bySlug = nil, nil
	if s.silo == nil {
		return nil
	}
	list, err := s.silo.ListCollections(ctx)
	if err != nil {
		return err
	}
	s.existing = map[string]*siloCollectionInfo{}
	s.bySlug = map[string]*siloCollectionInfo{}
	for i := range list {
		c := &list[i]
		s.existing[c.ID] = c
		if strings.HasPrefix(c.Slug, "kometa-") {
			s.bySlug[c.Slug] = c
		}
	}
	return nil
}

// syncCollections mirrors Plex collections for the wanted libraries.
func (s *syncer) syncCollections(ctx context.Context, wanted []plexSection) error {
	if err := s.loadExistingCollections(ctx); err != nil {
		s.addError("read Silo collections: %v", err)
		return nil // without the list, creating could duplicate
	}

	// Read every library's collections first.
	type listed struct {
		sec   plexSection
		colls []plexItem
		order int
	}
	var libs []listed
	synced := map[string]bool{} // scopes whose collections were all read
	failedType := map[string]bool{}
	s.progress.set("Reading Plex collections", len(wanted))
	for i, sec := range wanted {
		if err := ctx.Err(); err != nil {
			return err
		}
		colls, err := s.plex.Collections(ctx, sec)
		s.progress.add(1)
		if err != nil {
			s.addError("collections in %q: %v", sec.Title, err)
			failedType[sec.Type] = true
			continue // nothing in this library is deleted this run
		}
		libs = append(libs, listed{sec: sec, colls: colls, order: i})
		synced[sectionID(sec)] = true
	}

	// Build one spec per Silo collection.
	var specs []collSpec
	if s.combined {
		groups := map[string]*collSpec{}
		var order []string
		for _, l := range libs {
			for _, c := range l.colls {
				key := combinedKey(l.sec.Type, c.Title)
				g := groups[key]
				if g == nil {
					g = &collSpec{key: key, title: c.Title, slug: combinedSlug(l.sec.Type, c.Title)}
					groups[key] = g
					order = append(order, key)
				}
				g.parts = append(g.parts, collPart{sec: l.sec, coll: c})
			}
		}
		for _, key := range order {
			g := groups[key]
			// The winning library's copy goes first: it supplies the poster.
			orderOf := map[string]int{}
			for _, l := range libs {
				orderOf[sectionID(l.sec)] = l.order
			}
			sort.SliceStable(g.parts, func(i, j int) bool {
				ri, rj := s.rankOf(g.parts[i].sec, plexItem{}), s.rankOf(g.parts[j].sec, plexItem{})
				if ri != rj {
					return ri > rj
				}
				return orderOf[sectionID(g.parts[i].sec)] < orderOf[sectionID(g.parts[j].sec)]
			})
			g.title = g.parts[0].coll.Title
			specs = append(specs, *g)
		}
		for _, typ := range []string{"movie", "show"} {
			if !failedType[typ] {
				synced[combinedPrefix+typ] = true
			}
		}
	} else {
		for _, l := range libs {
			for _, c := range l.colls {
				specs = append(specs, collSpec{key: collectionKey(l.sec, c.Title), title: c.Title,
					slug: collectionSlug(l.sec, c.Title), parts: []collPart{{sec: l.sec, coll: c}}})
			}
		}
	}

	present := map[string]bool{}
	s.progress.set("Syncing collections", len(specs))
	for _, spec := range specs {
		if err := ctx.Err(); err != nil {
			return err
		}
		present[spec.key] = true
		s.progress.detailf(spec.title)
		if err := s.syncCollection(ctx, spec); err != nil {
			s.addError("collection %q: %v", spec.title, err)
		}
		s.mu.Lock()
		s.summary.CollectionsChecked++
		s.mu.Unlock()
		s.progress.add(1)
	}

	// Records left over from the other mode are duplicates now.
	s.removeOtherModeRecords(ctx, synced)
	if s.cfg.DeleteRemoved {
		s.deleteRemovedCollections(ctx, synced, present)
	}
	return s.state.flush(ctx)
}

// adoptFromOtherMode finds a Silo collection made under the other mode that
// this spec can take over, so switching modes moves collections rather than
// recreating them. It returns the old record's key and the record.
func (s *syncer) adoptFromOtherMode(spec collSpec) (string, *collectionRecord) {
	if s.combined {
		// Take the winning library's separate collection first.
		for _, p := range spec.parts {
			key := collectionKey(p.sec, p.coll.Title)
			if rec := s.state.collection(key); rec != nil && s.stillExists(rec) {
				return key, rec
			}
		}
		return "", nil
	}
	p := spec.parts[0]
	key := combinedKey(p.sec.Type, p.coll.Title)
	rec := s.state.collection(key)
	if rec == nil || !s.stillExists(rec) {
		return "", nil
	}
	// The combined collection goes to the library whose poster it carried.
	if len(rec.Sections) > 0 && rec.Sections[0] != sectionID(p.sec) {
		return "", nil
	}
	return key, rec
}

func (s *syncer) stillExists(rec *collectionRecord) bool {
	if s.existing == nil {
		return true
	}
	_, ok := s.existing[rec.SiloID]
	return ok
}

// removeOtherModeRecords deletes Silo collections recorded under the mode
// not in use, for libraries read in full this run.
func (s *syncer) removeOtherModeRecords(ctx context.Context, synced map[string]bool) {
	// A collection the current mode took over (by its slug) may still have an
	// old record pointing at it; that record goes, the collection stays.
	inUse := map[string]bool{}
	for _, key := range s.state.collectionKeys() {
		if isCombinedKey(key) == s.combined {
			if rec := s.state.collection(key); rec != nil {
				inUse[rec.SiloID] = true
			}
		}
	}
	for _, key := range s.state.collectionKeys() {
		if isCombinedKey(key) == s.combined {
			continue
		}
		scope := keyScope(key)
		rec := s.state.collection(key)
		if s.combined {
			if !synced[scope] { // a separate record: its library must have been read
				continue
			}
		} else {
			// A combined record: every library it drew from must have been read.
			all := len(rec.Sections) > 0
			for _, id := range rec.Sections {
				all = all && synced[id]
			}
			if !all {
				continue
			}
		}
		if inUse[rec.SiloID] {
			if !s.cfg.DryRun {
				s.state.setCollection(key, nil)
			}
			continue
		}
		if s.cfg.DryRun {
			s.mu.Lock()
			s.summary.CollectionsDeleted++
			s.mu.Unlock()
			continue
		}
		if s.stillExists(rec) {
			if err := s.silo.DeleteCollection(ctx, rec.SiloID); err != nil {
				s.addError("remove collection %q: %v", key, err)
				continue
			}
			s.mu.Lock()
			s.summary.CollectionsDeleted++
			s.mu.Unlock()
		}
		s.state.setCollection(key, nil)
	}
}

func (s *syncer) syncCollection(ctx context.Context, spec collSpec) error {
	// Members in Plex's order, and the Silo libraries they're in. With several
	// copies (Combined), the order follows the copy with the most titles, as
	// the most complete list; titles only in the others follow in their order.
	type fetched struct {
		part     collPart
		children []plexItem
	}
	var copies []fetched
	targets := map[int]bool{}
	fallback := false
	for _, p := range spec.parts {
		if t := targetLibrary(p.sec, s.siloLibraries, s.libraryMap); t > 0 {
			targets[t] = true
		} else {
			fallback = true // no Silo library matched: use where the members are
		}
		children, err := s.plex.CollectionChildren(ctx, p.coll.RatingKey)
		if err != nil {
			return err
		}
		copies = append(copies, fetched{part: p, children: children})
	}
	sort.SliceStable(copies, func(i, j int) bool { return len(copies[i].children) > len(copies[j].children) })

	var desired []string
	refs := map[string]memberRef{} // Silo ID -> how to look the title up again
	seen := map[string]bool{}
	libSet := map[int]bool{}
	for _, c := range copies {
		var ids []string
		for _, ch := range c.children {
			if id := ch.tmdbID(); id != "" {
				ids = append(ids, id)
			}
		}
		matches, err := s.lookup(ctx, mediaTypeFor(c.part.sec), ids)
		if err != nil {
			return err
		}
		for _, ch := range c.children {
			m, ok := matches[ch.tmdbID()]
			if !ok || m.MediaID == "" || seen[m.MediaID] {
				continue
			}
			seen[m.MediaID] = true
			desired = append(desired, m.MediaID)
			refs[m.MediaID] = memberRef{mediaType: mediaTypeFor(c.part.sec), tmdb: ch.tmdbID()}
			if lib, err := strconv.Atoi(m.LibraryID); err == nil {
				libSet[lib] = true
			}
		}
	}
	if len(desired) == 0 {
		// Nothing in Silo to put in it yet; a collection needs a library, so
		// leave any existing Silo collection as is.
		return nil
	}
	if fallback {
		for l := range libSet {
			targets[l] = true
		}
	}
	libs := make([]int, 0, len(targets))
	for l := range targets {
		libs = append(libs, l)
	}
	sort.Ints(libs)

	rec := s.state.collection(spec.key)
	// A collection deleted in Silo is rebuilt; one this plugin created but
	// never recorded (Silo restarted mid-run) is adopted, not duplicated; one
	// made under the other mode is taken over.
	if rec != nil && !s.stillExists(rec) {
		rec = nil
	}
	adopted := false
	if rec == nil {
		if found := s.bySlug[spec.slug]; found != nil {
			rec = &collectionRecord{SiloID: found.ID, Libs: found.LibraryIDs}
			adopted = true
		} else if oldKey, old := s.adoptFromOtherMode(spec); old != nil {
			copied := *old
			copied.Members = nil // re-add everything; adding is idempotent
			rec = &copied
			adopted = true
			if !s.cfg.DryRun {
				s.state.setCollection(oldKey, nil)
				s.state.setCollection(spec.key, rec)
			}
		}
	}

	winner := spec.parts[0].coll
	wantPoster := winner.Thumb != "" && !strings.Contains(winner.Thumb, "/composite/")
	posterChanged := wantPoster && (rec == nil || rec.Thumb != winner.Thumb)
	added, removed := diffMembers(desired, recMembers(rec))
	if adopted {
		added, removed = desired, nil
	}

	if s.cfg.DryRun {
		s.log("dry run: collection", "title", spec.title, "exists", rec != nil, "add", len(added), "remove", len(removed), "poster_changed", posterChanged)
		s.mu.Lock()
		if rec == nil {
			s.summary.CollectionsCreated++
		} else if posterChanged || adopted || len(added)+len(removed) > 0 {
			s.summary.CollectionsUpdated++
		}
		s.mu.Unlock()
		return nil
	}

	created := false
	if rec == nil {
		id, err := s.silo.CreateCollection(ctx, spec.title, spec.slug, libs)
		if err != nil {
			return err
		}
		rec = &collectionRecord{SiloID: id, Title: spec.title, Libs: libs}
		created = true
		added, removed = desired, nil
		// Record it now, so a restart before this collection finishes can't
		// lose track of it.
		s.state.setCollection(spec.key, rec)
		if err := s.state.flush(ctx); err != nil {
			s.addError("save collection %q: %v", spec.title, err)
		}
	}

	var poster []byte
	if posterChanged {
		var err error
		if poster, err = s.plex.Image(ctx, winner.Thumb); err != nil {
			s.addError("collection %q poster: %v", spec.title, err)
			poster = nil
		}
	}
	metaChanged := rec.Title != spec.title || !slices.Equal(rec.Libs, libs)
	gone := func(err error) error {
		// Deleted in Silo during this run: forget it and rebuild next run.
		s.state.setCollection(spec.key, nil)
		return fmt.Errorf("was deleted in Silo; it will be recreated on the next run")
	}
	if !created && metaChanged {
		err := s.silo.UpdateCollection(ctx, rec.SiloID, spec.title, libs)
		if errors.Is(err, errNotFound) {
			return gone(err)
		}
		if err != nil {
			return err
		}
	}
	if poster != nil {
		err := s.silo.SetCollectionPoster(ctx, rec.SiloID, poster)
		switch {
		case errors.Is(err, errNotFound):
			return gone(err)
		case err != nil:
			s.addError("collection %q poster: %v", spec.title, err)
			poster = nil
		default:
			rec.Thumb = winner.Thumb
		}
	}
	rec.Title, rec.Libs, rec.Sections = spec.title, libs, spec.sectionIDs()

	position := map[string]int{}
	for i, id := range desired {
		position[id] = i
	}
	members := map[string]bool{}
	for _, m := range recMembers(rec) {
		members[m] = true
	}
	renamed := map[string]string{} // desired ID -> the ID it was added under
	outside := 0                   // titles Silo has, but not in this collection's libraries
	for _, id := range added {
		orig := id
		err := s.silo.AddCollectionItem(ctx, rec.SiloID, id, position[id])
		if err != nil && !errors.Is(err, errRateLimited) {
			// Silo refuses an ID that stopped existing after the lookup (it
			// re-keys titles it matches better), answering 404. Look the
			// title up again and add it under its current ID. A 404 that
			// survives that means the title isn't in any of the collection's
			// Silo libraries.
			current, gone, lerr := s.currentID(ctx, refs[id])
			switch {
			case gone:
				continue
			case lerr == nil && current != "" && current != id:
				if err = s.silo.AddCollectionItem(ctx, rec.SiloID, current, position[id]); err == nil {
					id = current
					renamed[orig] = current
				}
			case errors.Is(err, errNotFound):
				outside++
				continue
			}
		}
		if err != nil {
			s.addError("collection %q add item: %v", spec.title, err)
			continue
		}
		members[id] = true
	}
	if outside > 0 {
		s.log("collection titles skipped: not in the collection's Silo libraries", "collection", spec.title, "titles", outside)
	}
	for _, id := range removed {
		if err := s.silo.RemoveCollectionItem(ctx, rec.SiloID, id); err != nil {
			s.addError("collection %q remove item: %v", spec.title, err)
			continue
		}
		delete(members, id)
	}
	rec.Members = rec.Members[:0]
	for id := range members {
		rec.Members = append(rec.Members, id)
	}

	// Put members in Plex's order. A title's position is set when it's added,
	// so titles already in the collection keep an old spot when Plex re-ranks
	// (UMTK does daily) unless the whole order is sent again.
	var order []string
	for _, id := range desired {
		if r, ok := renamed[id]; ok {
			id = r
		}
		if members[id] {
			order = append(order, id)
		}
	}
	reordered := false
	if len(order) > 1 && (created || adopted || !slices.Equal(order, rec.Order)) {
		err := s.silo.ReorderCollection(ctx, rec.SiloID, order)
		switch {
		case errors.Is(err, errOrderTooLarge):
			// Silo can't reorder more than 200 titles at once; the titles
			// keep the positions they were added at.
			s.log("collection order left as added: over 200 titles", "collection", spec.title)
		case err != nil:
			s.addError("collection %q order: %v", spec.title, err)
		default:
			rec.Order = order
			reordered = !created
		}
	}
	s.state.setCollection(spec.key, rec)
	if err := s.state.flush(ctx); err != nil {
		s.addError("save collection %q: %v", spec.title, err)
	}

	s.mu.Lock()
	if created {
		s.summary.CollectionsCreated++
	} else if poster != nil || metaChanged || adopted || reordered || len(added)+len(removed) > 0 {
		s.summary.CollectionsUpdated++
	}
	s.mu.Unlock()
	return nil
}

// deleteRemovedCollections deletes Silo collections this plugin created whose
// Plex collection no longer exists. Only scopes read in full this run are
// considered.
func (s *syncer) deleteRemovedCollections(ctx context.Context, synced, present map[string]bool) {
	for _, key := range s.state.collectionKeys() {
		if isCombinedKey(key) != s.combined || !synced[keyScope(key)] || present[key] {
			continue
		}
		rec := s.state.collection(key)
		if !s.stillExists(rec) {
			s.state.setCollection(key, nil) // already gone from Silo
			continue
		}
		if s.cfg.DryRun {
			s.log("dry run: would delete collection", "key", key)
			s.summary.CollectionsDeleted++
			continue
		}
		if err := s.silo.DeleteCollection(ctx, rec.SiloID); err != nil {
			s.addError("delete collection %q: %v", key, err)
			continue
		}
		s.state.setCollection(key, nil)
		s.summary.CollectionsDeleted++
	}
}

// recordTouches reports whether a collection record draws on any of the given
// Plex libraries.
func recordTouches(key string, rec *collectionRecord, sections map[string]bool) bool {
	if !isCombinedKey(key) {
		return sections[keyScope(key)]
	}
	if rec == nil {
		return false
	}
	for _, id := range rec.Sections {
		if sections[id] {
			return true
		}
	}
	return false
}

// memberRef is enough to look a collection member up again.
type memberRef struct {
	mediaType string
	tmdb      string
}

// currentID returns a title's Silo ID now; gone reports Silo no longer has it.
func (s *syncer) currentID(ctx context.Context, ref memberRef) (string, bool, error) {
	if ref.tmdb == "" {
		return "", false, nil
	}
	found, err := s.lookup(ctx, ref.mediaType, []string{ref.tmdb})
	if err != nil {
		return "", false, err
	}
	m, ok := found[ref.tmdb]
	if !ok || m.MediaID == "" {
		return "", true, nil
	}
	return m.MediaID, false, nil
}
