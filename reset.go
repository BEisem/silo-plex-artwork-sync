package main

import (
	"context"
	"crypto/subtle"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	sdkruntime "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/runtime"
)

// resetCollections deletes the Silo collections this plugin made for the
// given Plex libraries and forgets them, leaving posters alone. Besides the
// collections it has on record, it removes any it lost track of, found by
// the slug every plugin collection carries ("kometa-<plex library key>-...").
func resetCollections(ctx context.Context, silo *SiloClient, state *stateStore, sections []plexSection, summary *runSummary, prog *syncProgress) error {
	existing, err := silo.ListCollections(ctx)
	if err != nil {
		return err
	}
	byID := map[string]bool{}
	for _, c := range existing {
		byID[c.ID] = true
	}
	selected := map[string]bool{}
	for _, sec := range sections {
		selected[sectionID(sec)] = true
	}
	toDelete := map[string]bool{}
	for _, key := range state.collectionKeys() {
		if rec := state.collection(key); rec != nil && byID[rec.SiloID] && recordTouches(key, rec, selected) {
			toDelete[rec.SiloID] = true
		}
	}
	for _, sec := range sections {
		// Collections the plugin lost track of, recognised by slug: this
		// library's own, and combined ones for its type.
		for _, prefix := range []string{"kometa-" + sec.Key + "-", "kometa-c-" + sec.Type + "-"} {
			for _, c := range existing {
				if strings.HasPrefix(c.Slug, prefix) && !trackedElsewhere(state, c.ID, selected) {
					toDelete[c.ID] = true
				}
			}
		}
	}
	prog.set("Deleting collections", len(toDelete))
	for id := range toDelete {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := silo.DeleteCollection(ctx, id); err != nil {
			if errors.Is(err, context.Canceled) {
				return err
			}
			summary.ErrorCount++
			if len(summary.Errors) < maxErrorMessages {
				summary.Errors = append(summary.Errors, "delete collection "+id+": "+err.Error())
			}
		} else {
			summary.CollectionsDeleted++
		}
		prog.add(1)
	}
	for _, key := range state.collectionKeys() {
		rec := state.collection(key)
		if recordTouches(key, rec, selected) && (rec == nil || !byID[rec.SiloID] || toDelete[rec.SiloID]) {
			state.setCollection(key, nil)
		}
	}
	return state.flush(ctx)
}

// trackedElsewhere reports whether a Silo collection is on record for
// libraries other than the selected ones only, so a reset must leave it.
func trackedElsewhere(state *stateStore, siloID string, selected map[string]bool) bool {
	for _, key := range state.collectionKeys() {
		if rec := state.collection(key); rec != nil && rec.SiloID == siloID {
			return !recordTouches(key, rec, selected)
		}
	}
	return false
}

func (p *plugin) runReset(ctx context.Context, ids map[string]bool) (*runSummary, error) {
	p.mu.Lock()
	if p.running {
		p.mu.Unlock()
		return nil, errBusy
	}
	p.running, p.mode = true, "reset"
	ctx = p.withStop(ctx)
	cfg := p.cfg
	p.mu.Unlock()
	p.prog.start()

	summary := &runSummary{StartedAt: nowRFC3339(), Trigger: "reset collections"}
	err := func() error {
		if err := cfg.validate(); err != nil {
			return err
		}
		host := sdkruntime.Host()
		if host == nil {
			return errors.New("Silo host connection is not ready yet; try again in a moment")
		}
		base, err := resolveSiloBase(ctx, host, cfg.SiloInternalURL)
		if err != nil {
			return err
		}
		state, err := loadState(ctx, host)
		if err != nil {
			return err
		}
		all, err := newPlexClient(cfg.PlexURL, cfg.PlexToken).Sections(ctx)
		if err != nil {
			return err
		}
		var sections []plexSection
		for _, sec := range all {
			if ids[sectionID(sec)] {
				sections = append(sections, sec)
			}
		}
		return resetCollections(ctx, newSiloClient(base, cfg.SiloAPIKey), state, sections, summary, &p.prog)
	}()
	if p.endStop() {
		err = markCancelled(summary, err)
	}
	summary.FinishedAt = nowRFC3339()
	if err != nil {
		summary.ErrorCount++
		summary.Errors = append(summary.Errors, err.Error())
	}
	p.finish(summary)
	return summary, err
}

// finish records a completed run of any kind.
func (p *plugin) finish(summary *runSummary) {
	p.mu.Lock()
	p.running, p.mode = false, ""
	p.lastRun = summary
	p.mu.Unlock()
	if host := sdkruntime.Host(); host != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		meta, _ := readMeta(ctx, host)
		meta.LastRun = summary
		_ = writeMeta(ctx, host, meta)
	}
}

func (r *routesServer) resetCollectionsForm(req *pluginv1.HandleHTTPRequest) *pluginv1.HandleHTTPResponse {
	p := r.p
	form, err := url.ParseQuery(string(req.GetBody()))
	if err != nil {
		return htmlMessage(http.StatusBadRequest, "Could not read the form.")
	}
	if subtle.ConstantTimeCompare([]byte(form.Get("token")), []byte(p.token())) != 1 {
		return htmlMessage(http.StatusForbidden, "This form has expired. Reload the status page and try again.")
	}
	if form.Get("confirm") != "on" {
		return htmlMessage(http.StatusBadRequest, "Tick the confirmation box to reset collections.")
	}
	ids := map[string]bool{}
	for _, id := range form["library"] {
		if id = strings.TrimSpace(id); id != "" {
			ids[id] = true
		}
	}
	if len(ids) == 0 {
		return htmlMessage(http.StatusBadRequest, "Choose at least one library.")
	}
	p.mu.Lock()
	busy := p.running
	p.mu.Unlock()
	if busy {
		return htmlMessage(http.StatusConflict, "A sync or revert is already running. Wait for it to finish.")
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
		defer cancel()
		if _, err := p.runReset(ctx, ids); err != nil && !errors.Is(err, errBusy) {
			p.log.Error("reset collections failed", "error", err)
		}
	}()
	return htmlMessage(http.StatusAccepted, "Resetting collections. The next sync rebuilds them in the right Silo libraries.")
}
