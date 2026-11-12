package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Revert undoes this plugin's changes for chosen Plex libraries. Silo has no
// bulk "reset artwork" action, and the image lock Silo sets on apply makes
// library refreshes skip these items, so each item is reverted on its own:
//
//  1. remove only the image lock (other locks the user set are kept),
//  2. queue Silo's own manual refresh, which re-picks the default artwork,
//  3. wait for that job before starting more, so Silo's queue isn't flooded,
//  4. forget the item, so the plugin won't push it again unless synced again.
const (
	revertWorkers  = 3
	jobPollEvery   = 3 * time.Second
	jobWaitTimeout = 15 * time.Minute
)

// revertProgress is read by the status page while a revert runs.
type revertProgress struct {
	total atomic.Int64
	done  atomic.Int64
}

// trackedSection summarizes what the plugin has changed for one Plex library.
type trackedSection struct {
	ID          string
	Title       string
	Posters     int
	Collections int
}

// trackedSections lists every library the plugin has state for.
func trackedSections(state *stateStore, titles map[string]string) []trackedSection {
	bySection := map[string]*trackedSection{}
	get := func(id string) *trackedSection {
		if t, ok := bySection[id]; ok {
			return t
		}
		t := &trackedSection{ID: id, Title: titles[id]}
		if t.Title == "" {
			t.Title = id
		}
		bySection[id] = t
		return t
	}
	for _, key := range state.posterKeys("") {
		id, _, _ := strings.Cut(key, "/")
		get(id).Posters++
	}
	for _, key := range state.collectionKeys() {
		if !isCombinedKey(key) {
			get(keyScope(key)).Collections++
			continue
		}
		// A combined collection counts toward every library it draws on.
		if rec := state.collection(key); rec != nil {
			for _, id := range rec.Sections {
				get(id).Collections++
			}
		}
	}
	out := make([]trackedSection, 0, len(bySection))
	for _, t := range bySection {
		out = append(out, *t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Title < out[j].Title })
	return out
}

type reverter struct {
	silo     *SiloClient
	state    *stateStore
	progress *revertProgress
	log      func(msg string, args ...any)

	mu      sync.Mutex
	summary *runSummary
}

func (r *reverter) addError(format string, args ...any) {
	if isCancellation(args) {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.summary.ErrorCount++
	if len(r.summary.Errors) < maxErrorMessages {
		r.summary.Errors = append(r.summary.Errors, fmt.Sprintf(format, args...))
	}
	r.log("revert error", "error", fmt.Sprintf(format, args...))
}

// run reverts posters (and optionally collections) for the given section IDs.
func (r *reverter) run(ctx context.Context, sections map[string]bool, deleteCollections bool) error {
	// Silo item -> every state key pointing at it. A movie in both "Movies"
	// and "Movies 4k" is one Silo item, so reverting it clears both entries;
	// a library still being synced pushes its poster again on the next run.
	keysByItem := map[string][]string{}
	for _, key := range r.state.posterKeys("") {
		if _, contentID, _ := strings.Cut(r.state.posterThumb(key), "|"); contentID != "" {
			keysByItem[contentID] = append(keysByItem[contentID], key)
		}
	}
	var items []string
	for contentID, keys := range keysByItem {
		if slices.ContainsFunc(keys, func(key string) bool {
			section, _, _ := strings.Cut(key, "/")
			return sections[section]
		}) {
			items = append(items, contentID)
		}
	}
	sort.Strings(items)
	r.progress.total.Store(int64(len(items)))
	r.progress.done.Store(0)

	work := make(chan string)
	var wg sync.WaitGroup
	for range revertWorkers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for contentID := range work {
				err := r.revertItem(ctx, contentID)
				r.progress.done.Add(1)
				if err != nil {
					r.addError("item %s: %v", contentID, err)
					continue
				}
				r.mu.Lock()
				for _, key := range keysByItem[contentID] {
					r.state.deletePoster(key)
				}
				r.summary.PostersReverted++
				flush := r.summary.PostersReverted%flushEvery == 0
				var ferr error
				if flush {
					ferr = r.state.flush(ctx)
				}
				r.mu.Unlock()
				if ferr != nil {
					r.addError("save progress: %v", ferr)
				}
			}
		}()
	}
	for _, id := range items {
		if ctx.Err() != nil {
			break
		}
		work <- id
	}
	close(work)
	wg.Wait()

	if deleteCollections {
		for _, key := range r.state.collectionKeys() {
			rec := r.state.collection(key)
			if !recordTouches(key, rec, sections) || ctx.Err() != nil {
				continue
			}
			if err := r.silo.DeleteCollection(ctx, rec.SiloID); err != nil {
				r.addError("delete collection %q: %v", key, err)
				continue
			}
			r.state.setCollection(key, nil)
			r.summary.CollectionsDeleted++
		}
	}
	if err := r.state.flush(ctx); err != nil {
		return err
	}
	return ctx.Err()
}

func (r *reverter) revertItem(ctx context.Context, contentID string) error {
	locks, err := r.silo.LockedFields(ctx, contentID)
	if errors.Is(err, errNotFound) {
		return nil // gone from Silo; nothing to revert
	}
	if err != nil {
		return err
	}
	if slices.Contains(locks, fieldImagesLock) {
		kept := slices.DeleteFunc(slices.Clone(locks), func(f int) bool { return f == fieldImagesLock })
		if err := r.silo.SetLockedFields(ctx, contentID, kept); err != nil {
			return fmt.Errorf("remove image lock: %w", err)
		}
	}
	jobID, err := r.silo.RefreshItem(ctx, contentID)
	if err != nil {
		return fmt.Errorf("queue refresh: %w", err)
	}
	if jobID == "" {
		return nil
	}
	waitCtx, cancel := context.WithTimeout(ctx, jobWaitTimeout)
	defer cancel()
	for {
		status, terminal, msg, err := r.silo.JobStatus(waitCtx, jobID)
		if err != nil {
			return fmt.Errorf("refresh job %s: %w", jobID, err)
		}
		if terminal {
			if status == "succeeded" {
				return nil
			}
			return fmt.Errorf("Silo refresh job %s: %s %s", jobID, status, msg)
		}
		select {
		case <-waitCtx.Done():
			return fmt.Errorf("Silo refresh job %s still %s; image lock is already removed, so the item will pick up its default poster when the job runs", jobID, status)
		case <-time.After(jobPollEvery):
		}
	}
}
