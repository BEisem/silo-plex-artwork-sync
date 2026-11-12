package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	sdkruntime "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/runtime"
)

func (p *plugin) token() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.formToken == "" {
		b := make([]byte, 16)
		_, _ = rand.Read(b)
		p.formToken = hex.EncodeToString(b)
	}
	return p.formToken
}

// runRevert reverts the given sections. It shares the "running" guard with
// syncs, so a sync can't start halfway through a revert or the reverse.
func (p *plugin) runRevert(ctx context.Context, sections map[string]bool, deleteCollections bool) (*runSummary, error) {
	p.mu.Lock()
	if p.running {
		p.mu.Unlock()
		return nil, errBusy
	}
	p.running = true
	p.mode = "revert"
	ctx = p.withStop(ctx)
	cfg := p.cfg
	p.mu.Unlock()
	p.prog.start()

	summary := &runSummary{StartedAt: nowRFC3339(), Trigger: "revert"}
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
		r := &reverter{silo: newSiloClient(base, cfg.SiloAPIKey), state: state, progress: &p.revertProg, log: p.logf, summary: summary}
		return r.run(ctx, sections, deleteCollections)
	}()
	if p.endStop() {
		err = markCancelled(summary, err)
	}
	summary.FinishedAt = nowRFC3339()
	if err != nil {
		summary.ErrorCount++
		summary.Errors = append(summary.Errors, err.Error())
	}
	p.log.Info("revert finished", "posters_reverted", summary.PostersReverted,
		"collections_deleted", summary.CollectionsDeleted, "errors", summary.ErrorCount)

	p.mu.Lock()
	p.running = false
	p.mode = ""
	p.lastRun = summary
	p.mu.Unlock()
	if host := sdkruntime.Host(); host != nil {
		saveCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		meta, _ := readMeta(saveCtx, host)
		meta.LastRun = summary
		_ = writeMeta(saveCtx, host, meta)
		cancel()
	}
	return summary, err
}

// revert handles the status page's Revert form.
func (r *routesServer) revert(req *pluginv1.HandleHTTPRequest) *pluginv1.HandleHTTPResponse {
	p := r.p
	form, err := url.ParseQuery(string(req.GetBody()))
	if err != nil {
		return htmlMessage(http.StatusBadRequest, "Could not read the form.")
	}
	if subtle.ConstantTimeCompare([]byte(form.Get("token")), []byte(p.token())) != 1 {
		return htmlMessage(http.StatusForbidden, "This form has expired. Reload the status page and try again.")
	}
	if form.Get("confirm") != "on" {
		return htmlMessage(http.StatusBadRequest, "Tick the confirmation box to revert.")
	}
	sections := map[string]bool{}
	for _, id := range form["library"] {
		if id = strings.TrimSpace(id); id != "" {
			sections[id] = true
		}
	}
	if len(sections) == 0 {
		return htmlMessage(http.StatusBadRequest, "Choose at least one library to revert.")
	}
	deleteCollections := form.Get("delete_collections") == "on"

	p.mu.Lock()
	busy := p.running
	p.mu.Unlock()
	if busy {
		return htmlMessage(http.StatusConflict, "A sync or revert is already running. Wait for it to finish.")
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 12*time.Hour)
		defer cancel()
		if _, err := p.runRevert(ctx, sections, deleteCollections); err != nil && !errors.Is(err, errBusy) {
			p.log.Error("revert failed", "error", err)
		}
	}()
	return htmlMessage(http.StatusAccepted, "Revert started. Progress shows on the status page.")
}

func htmlMessage(code int, msg string) *pluginv1.HandleHTTPResponse {
	var b strings.Builder
	_ = messagePage.Execute(&b, msg)
	return &pluginv1.HandleHTTPResponse{
		StatusCode: int32(code),
		Headers:    map[string]string{"Content-Type": "text/html; charset=utf-8", "Cache-Control": "no-store"},
		Body:       []byte(b.String()),
	}
}

// statusRevertData gathers what the Revert section of the status page shows.
func (p *plugin) statusRevertData(ctx context.Context, cfg Config) []trackedSection {
	host := sdkruntime.Host()
	if host == nil {
		return nil
	}
	state, err := loadState(ctx, host)
	if err != nil {
		return nil
	}
	titles := map[string]string{}
	if cfg.validate() == nil {
		if sections, err := newPlexClient(cfg.PlexURL, cfg.PlexToken).Sections(ctx); err == nil {
			for _, sec := range sections {
				titles[sectionID(sec)] = sec.Title
			}
		}
	}
	return trackedSections(state, titles)
}
