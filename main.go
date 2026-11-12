// Command plex-artwork-sync is a Silo plugin that mirrors Plex posters
// (movies and series only, by default the ones Kometa has overlaid) and Plex
// collections into Silo. It runs when Kometa's run_end webhook arrives, after
// a configurable delay, or on the schedule given to its task in Silo.
package main

import (
	"context"
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/go-hclog"
	"google.golang.org/protobuf/types/known/structpb"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	sdkruntime "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/runtime"
)

//go:embed manifest.json
var manifestJSON []byte

// version is stamped at build time with -ldflags "-X main.version=...".
var version = ""

const maxRunTime = 2 * time.Hour

type plugin struct {
	log hclog.Logger

	mu      sync.Mutex
	cfg     Config
	timer   *time.Timer
	due     time.Time
	running bool
	mode    string // "sync" or "revert" while running
	rerun   bool   // a webhook arrived during a run
	lastRun *runSummary

	stop      context.CancelFunc // cancels the current run
	cancelled bool               // the current run was cancelled from the status page

	revertProg revertProgress
	prog       syncProgress
	formToken  string // guards the revert form against cross-site posts
}

func main() {
	p := &plugin{
		log: hclog.New(&hclog.LoggerOptions{Name: "plex-artwork-sync", Output: os.Stderr, JSONFormat: true}),
		cfg: defaultConfig(),
	}
	sdkruntime.ServeManifestWithOptions(manifestJSON, version, sdkruntime.CapabilityServers{
		ScheduledTask: &taskServer{p: p},
		HttpRoutes:    &routesServer{p: p},
	}, sdkruntime.WithConfigure(p.configure))
}

func (p *plugin) logf(msg string, args ...any) { p.log.Info(msg, args...) }

func (p *plugin) configure(_ context.Context, entries []*pluginv1.ConfigEntry) error {
	cfg := parseConfig(entries)
	p.mu.Lock()
	p.cfg = cfg
	p.mu.Unlock()
	if err := cfg.validate(); err != nil {
		p.log.Warn(err.Error())
	}
	// Resume a webhook-requested run that a restart interrupted.
	go p.resumePending()
	p.startHeartbeat()
	return nil
}

func (p *plugin) config() Config {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cfg
}

// resumePending re-arms the timer from persisted state after a restart.
func (p *plugin) resumePending() {
	for range 20 {
		if host := sdkruntime.Host(); host != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			meta, err := readMeta(ctx, host)
			cancel()
			if err != nil {
				p.log.Warn("read saved state", "error", err)
				return
			}
			p.mu.Lock()
			p.lastRun = meta.LastRun
			p.mu.Unlock()
			if due, err := time.Parse(time.RFC3339, meta.PendingDue); err == nil {
				p.log.Info("resuming pending sync", "due", due)
				p.arm(due, false)
			}
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// requestRun is called by the Kometa webhook.
func (p *plugin) requestRun() time.Time {
	cfg := p.config()
	due := time.Now().Add(time.Duration(cfg.DelayMinutes) * time.Minute)
	p.arm(due, true)
	return due
}

// arm (re)starts the delay timer. Several webhooks in a row collapse into one
// run, timed from the last one.
func (p *plugin) arm(due time.Time, persist bool) {
	p.mu.Lock()
	if p.running {
		p.rerun = true
	}
	p.due = due
	if p.timer != nil {
		p.timer.Stop()
	}
	p.timer = time.AfterFunc(time.Until(due), func() {
		ctx, cancel := context.WithTimeout(context.Background(), maxRunTime)
		defer cancel()
		if _, err := p.runSync(ctx, "kometa webhook"); err != nil && !errors.Is(err, errBusy) {
			p.log.Error("webhook-triggered sync failed", "error", err)
		}
	})
	p.mu.Unlock()

	if persist {
		if host := sdkruntime.Host(); host != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			meta, _ := readMeta(ctx, host)
			meta.PendingDue = due.UTC().Format(time.RFC3339)
			if err := writeMeta(ctx, host, meta); err != nil {
				p.log.Warn("save pending run", "error", err)
			}
		}
	}
}

var errBusy = errors.New("a sync is already running")

// runSync runs one sync and records its summary.
func (p *plugin) runSync(ctx context.Context, trigger string) (*runSummary, error) {
	p.mu.Lock()
	if p.running {
		p.rerun = true
		p.mu.Unlock()
		return nil, errBusy
	}
	p.running = true
	p.mode = "sync"
	p.rerun = false
	ctx = p.withStop(ctx)
	if p.timer != nil {
		p.timer.Stop()
		p.timer = nil
	}
	p.due = time.Time{}
	cfg := p.cfg
	p.mu.Unlock()

	p.prog.start()
	summary := &runSummary{StartedAt: nowRFC3339(), Trigger: trigger, DryRun: cfg.DryRun}
	err := p.doSync(ctx, cfg, summary)
	if p.endStop() {
		err = markCancelled(summary, err)
	}
	summary.FinishedAt = nowRFC3339()
	if err != nil {
		summary.ErrorCount++
		summary.Errors = append(summary.Errors, err.Error())
	}
	p.log.Info("sync finished", "trigger", trigger, "posters_applied", summary.PostersApplied,
		"unchanged", summary.PostersUnchanged, "not_in_silo", summary.NotInSilo,
		"collections_created", summary.CollectionsCreated, "collections_updated", summary.CollectionsUpdated,
		"collections_deleted", summary.CollectionsDeleted, "errors", summary.ErrorCount)

	p.mu.Lock()
	p.running = false
	p.lastRun = summary
	rerun := p.rerun
	p.mu.Unlock()

	if host := sdkruntime.Host(); host != nil {
		saveCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		meta, _ := readMeta(saveCtx, host)
		meta.LastRun = summary
		if !rerun {
			meta.PendingDue = ""
		}
		if werr := writeMeta(saveCtx, host, meta); werr != nil {
			p.log.Warn("save run summary", "error", werr)
		}
		cancel()
	}
	if rerun {
		p.requestRun()
	}
	return summary, err
}

func (p *plugin) doSync(ctx context.Context, cfg Config, summary *runSummary) error {
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
	pick, err := currentSelector(ctx, cfg)
	if err != nil {
		return err
	}
	policy, err := loadPolicy(ctx, host)
	if err != nil {
		return err
	}
	hostLibs, err := host.ListLibraries(ctx, "")
	if err != nil {
		return fmt.Errorf("list Silo libraries: %w", err)
	}
	s := &syncer{
		selected:      pick,
		rank:          policy.rankFunc(),
		policy:        policy.signature(),
		siloLibraries: hostLibraries(hostLibs),
		libraryMap:    policy.Mapping,
		combined:      normalizeCollectionMode(policy.CollMode) == collectionsCombined,
		progress:      &p.prog,
		cfg:           cfg,
		catalog:       host,
		plex:          newPlexClient(cfg.PlexURL, cfg.PlexToken),
		silo:          newSiloClient(base, cfg.SiloAPIKey),
		state:         state,
		log:           p.logf,
		summary:       summary,
	}
	return s.run(ctx)
}

// ---- scheduled_task.v1 ----

type taskServer struct {
	pluginv1.UnimplementedScheduledTaskServer
	p *plugin
}

// Run is the manual "Run now" button and any fallback schedule set in Silo.
// Silo gives this call only 10 seconds, so the sync runs in the background on
// its own context and Run returns as soon as it has started. Results appear on
// the plugin's status page.
func (t *taskServer) Run(_ context.Context, _ *pluginv1.RunScheduledTaskRequest) (*pluginv1.RunScheduledTaskResponse, error) {
	p := t.p
	p.mu.Lock()
	busy := p.running
	p.mu.Unlock()
	if busy {
		out, _ := toStruct(map[string]any{"started": false, "message": "A sync is already running; see the plugin's status page."})
		return &pluginv1.RunScheduledTaskResponse{Output: out}, nil
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), maxRunTime)
		defer cancel()
		if _, err := p.runSync(ctx, "scheduled task"); err != nil && !errors.Is(err, errBusy) {
			p.log.Error("sync failed", "error", err)
		}
	}()
	out, _ := toStruct(map[string]any{"started": true, "message": "Sync started in the background; results appear on the plugin's status page."})
	return &pluginv1.RunScheduledTaskResponse{Output: out}, nil
}

func toStruct(v any) (*structpb.Struct, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return structpb.NewStruct(m)
}

// ---- http_routes.v1 ----

type routesServer struct {
	pluginv1.UnimplementedHttpRoutesServer
	p *plugin
}

func (r *routesServer) Handle(_ context.Context, req *pluginv1.HandleHTTPRequest) (*pluginv1.HandleHTTPResponse, error) {
	path := req.GetPath()
	switch {
	case req.GetMethod() == http.MethodPost && path == "/kometa":
		return r.webhook(req), nil
	case req.GetMethod() == http.MethodGet && strings.HasPrefix(path, "/assets/"):
		return serveAsset(strings.TrimPrefix(path, "/assets/")), nil
	case req.GetMethod() == http.MethodPost && path == "/libraries":
		return r.saveLibraries(req), nil
	case req.GetMethod() == http.MethodGet && path == "/progress":
		return jsonResponse(http.StatusOK, r.p.progressView()), nil
	case req.GetMethod() == http.MethodPost && path == "/reset-collections":
		return r.resetCollectionsForm(req), nil
	case req.GetMethod() == http.MethodPost && path == "/cancel":
		return r.cancelForm(req), nil
	case req.GetMethod() == http.MethodPost && path == "/revert":
		return r.revert(req), nil
	case req.GetMethod() == http.MethodGet && path == "/status":
		return r.status(req), nil
	}
	return textResponse(http.StatusNotFound, "not found"), nil
}

func (r *routesServer) webhook(req *pluginv1.HandleHTTPRequest) *pluginv1.HandleHTTPResponse {
	cfg := r.p.config()
	if cfg.WebhookSecret == "" {
		return textResponse(http.StatusServiceUnavailable, "plugin is not configured")
	}
	secret := ""
	if q := req.GetQuery(); q != nil {
		secret = str(q.AsMap()["secret"])
	}
	if subtle.ConstantTimeCompare([]byte(secret), []byte(cfg.WebhookSecret)) != 1 {
		return textResponse(http.StatusUnauthorized, "bad secret")
	}

	// Kometa sends JSON with an "event" field. Only run_end triggers a sync;
	// a body without an event (e.g. a manual curl) is treated as run_end.
	var body struct {
		Event string `json:"event"`
	}
	_ = json.Unmarshal(req.GetBody(), &body)
	if body.Event != "" && body.Event != "run_end" {
		return jsonResponse(http.StatusOK, map[string]any{"ignored": body.Event})
	}
	due := r.p.requestRun()
	r.p.log.Info("Kometa run_end received; sync scheduled", "due", due)
	return jsonResponse(http.StatusAccepted, map[string]any{"scheduled_for": due.UTC().Format(time.RFC3339)})
}

type checkResult struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

// connectionCheck tests every link a sync depends on, without writing anything.
func (p *plugin) connectionCheck(ctx context.Context, cfg Config) []checkResult {
	var out []checkResult
	add := func(name string, err error, okDetail string) bool {
		r := checkResult{Name: name, OK: err == nil, Detail: okDetail}
		if err != nil {
			r.Detail = err.Error()
		}
		out = append(out, r)
		return err == nil
	}

	if !add("Settings", cfg.validate(), "All required settings are filled in.") {
		return out
	}

	plex := newPlexClient(cfg.PlexURL, cfg.PlexToken)
	sections, err := plex.Sections(ctx)
	if add("Plex", err, "") {
		pick, perr := currentSelector(ctx, cfg)
		if perr != nil {
			pick = selector(cfg, nil, false)
		}
		var names []string
		var picked []plexSection
		for _, sec := range sections {
			if pick(sec) {
				names = append(names, sec.Title)
				picked = append(picked, sec)
			}
		}
		if len(picked) == 0 {
			out[len(out)-1] = checkResult{Name: "Plex", Detail: "Connected, but no libraries are selected. Choose some under Libraries."}
		} else {
			out[len(out)-1].Detail = "Connected. Libraries to sync: " + strings.Join(names, ", ")
			if cfg.OnlyOverlaid {
				var withLabel, without []string
				for _, sec := range picked {
					if _, ok, err := plex.labelKey(ctx, sec.Key, overlayLabel); err == nil && ok {
						withLabel = append(withLabel, sec.Title)
					} else {
						without = append(without, sec.Title)
					}
				}
				detail := ""
				if len(withLabel) > 0 {
					detail = "Found in: " + strings.Join(withLabel, ", ") + ". "
				}
				if len(without) > 0 {
					detail += "Not found in: " + strings.Join(without, ", ") + " (no Kometa overlays there yet, so nothing will sync from those)."
				}
				out = append(out, checkResult{Name: "Kometa Overlay label", OK: len(withLabel) > 0, Detail: strings.TrimSpace(detail)})
			}
		}
	}

	host := sdkruntime.Host()
	if host == nil {
		add("Silo", errors.New("Silo host connection is not ready yet; reload in a moment"), "")
		return out
	}
	base, err := resolveSiloBase(ctx, host, cfg.SiloInternalURL)
	if !add("Silo internal URL", err, "") {
		return out
	}
	out[len(out)-1].Detail = base

	silo := newSiloClient(base, cfg.SiloAPIKey)
	add("Silo admin API key", silo.Ping(ctx), "Accepted for Silo's admin API (v2).")
	return out
}

func (r *routesServer) status(req *pluginv1.HandleHTTPRequest) *pluginv1.HandleHTTPResponse {
	p := r.p
	p.mu.Lock()
	cfg := p.cfg
	running := p.running
	due := p.due
	last := p.lastRun
	p.mu.Unlock()

	webhook := "http://SILO_HOST" + defaultPluginContentPrefix + "/plugins/INSTALLATION_ID/kometa?secret=YOUR_WEBHOOK_SECRET"
	if host := sdkruntime.Host(); host != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		if base, err := pluginBaseURL(ctx, host); err == nil {
			webhook = base + "/kometa?secret=YOUR_WEBHOOK_SECRET"
		}
		cancel()
	}

	p.mu.Lock()
	reverting := p.running && p.mode == "revert"
	p.mu.Unlock()
	_ = reverting
	prog := p.progressView()
	data := map[string]any{"WebhookURL": webhook, "Running": running, "Last": last,
		"Mode": prog.Mode, "Progress": prog, "FormToken": p.token()}
	{
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		tracked := p.statusRevertData(ctx, cfg)
		data["Tracked"] = tracked
		counts := map[string]int{}
		for _, t := range tracked {
			counts[t.ID] = t.Collections
		}
		data["CollectionCounts"] = counts
		data["Libraries"], data["LibrariesError"] = libraryOptions(ctx, cfg)
		cancel()
	}
	if !due.IsZero() {
		data["Due"] = due.UTC().Format(time.RFC3339)
	}
	if err := cfg.validate(); err != nil {
		data["ConfigError"] = err.Error()
	}

	if q := req.GetQuery(); q != nil && str(q.AsMap()["check"]) == "1" {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		data["Checks"] = p.connectionCheck(ctx, cfg)
		cancel()
	}
	if q := req.GetQuery(); q != nil && str(q.AsMap()["format"]) == "json" {
		return jsonResponse(http.StatusOK, data)
	}
	var b strings.Builder
	if err := statusPage.Execute(&b, data); err != nil {
		return textResponse(http.StatusInternalServerError, err.Error())
	}
	return &pluginv1.HandleHTTPResponse{
		StatusCode: http.StatusOK,
		Headers:    map[string]string{"Content-Type": "text/html; charset=utf-8", "Cache-Control": "no-store"},
		Body:       []byte(b.String()),
	}
}

func textResponse(code int, msg string) *pluginv1.HandleHTTPResponse {
	return &pluginv1.HandleHTTPResponse{
		StatusCode: int32(code),
		Headers:    map[string]string{"Content-Type": "text/plain; charset=utf-8"},
		Body:       []byte(msg + "\n"),
	}
}

func jsonResponse(code int, v any) *pluginv1.HandleHTTPResponse {
	data, _ := json.Marshal(v)
	return &pluginv1.HandleHTTPResponse{
		StatusCode: int32(code),
		Headers:    map[string]string{"Content-Type": "application/json"},
		Body:       data,
	}
}

func revertPct(done, total int64) int64 {
	if total <= 0 {
		return 0
	}
	return done * 100 / total
}

// progressView reports what is running now, for the status page.
func (p *plugin) progressView() progressView {
	p.mu.Lock()
	running, mode := p.running, p.mode
	p.mu.Unlock()
	if !running {
		return progressView{}
	}
	v := p.prog.snapshot()
	if mode == "revert" {
		v.Phase = "Reverting posters"
		v.Done, v.Total = int(p.revertProg.done.Load()), int(p.revertProg.total.Load())
	}
	v.Running, v.Mode = true, mode
	return v
}
