package main

import (
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
)

// Config is the plugin's "settings" global config, as saved in Silo's admin UI.
type Config struct {
	PlexURL         string
	PlexToken       string
	PlexLibraries   []string // empty = every movie/show library
	SiloAPIKey      string
	SiloInternalURL string // optional override for auto-detected internal URL
	WebhookSecret   string
	DelayMinutes    int
	OnlyOverlaid    bool
	SyncCollections bool
	DeleteRemoved   bool
	DryRun          bool

	// Artwork types. Only posters are on by default, as in 0.5.0; a saved
	// 0.5.0 config has none of these keys, so it keeps exactly that.
	NoPosters               bool // "sync_posters" off; the zero value keeps 0.5.0's behaviour
	SyncBackdrops           bool
	SyncLogos               bool
	SyncSeasonPosters       bool
	SyncCollectionBackdrops bool
}

// wantsItemExtras reports whether any per-item artwork beyond posters is on.
func (c Config) wantsItemExtras() bool {
	return c.SyncBackdrops || c.SyncLogos || c.SyncSeasonPosters
}

func defaultConfig() Config {
	return Config{
		DelayMinutes:    5,
		OnlyOverlaid:    true,
		SyncCollections: true,
		DeleteRemoved:   true,
	}
}

// parseConfig merges every config entry the host sends. The admin form saves
// one "settings" object, but flattening all entries keeps this tolerant of
// hosts that deliver fields individually.
func parseConfig(entries []*pluginv1.ConfigEntry) Config {
	values := map[string]any{}
	for _, entry := range entries {
		if entry == nil || entry.GetValue() == nil {
			continue
		}
		m := entry.GetValue().AsMap()
		if v, ok := m["value"]; ok && len(m) == 1 && entry.GetKey() != "settings" {
			values[entry.GetKey()] = v
			continue
		}
		for k, v := range m {
			values[k] = v
		}
	}

	cfg := defaultConfig()
	cfg.PlexURL = strings.TrimRight(str(values["plex_url"]), "/")
	cfg.PlexToken = str(values["plex_token"])
	cfg.SiloAPIKey = str(values["silo_api_key"])
	cfg.SiloInternalURL = strings.TrimRight(str(values["silo_internal_url"]), "/")
	cfg.WebhookSecret = str(values["webhook_secret"])
	for _, name := range strings.Split(str(values["plex_libraries"]), ",") {
		if name = strings.TrimSpace(name); name != "" {
			cfg.PlexLibraries = append(cfg.PlexLibraries, name)
		}
	}
	if n, ok := num(values["delay_minutes"]); ok && n >= 0 && n <= 240 {
		cfg.DelayMinutes = n
	}
	if b, ok := values["only_overlaid"].(bool); ok {
		cfg.OnlyOverlaid = b
	}
	if b, ok := values["sync_collections"].(bool); ok {
		cfg.SyncCollections = b
	}
	if b, ok := values["delete_removed_collections"].(bool); ok {
		cfg.DeleteRemoved = b
	}
	if b, ok := values["dry_run"].(bool); ok {
		cfg.DryRun = b
	}
	if b, ok := values["sync_posters"].(bool); ok {
		cfg.NoPosters = !b
	}
	for key, dst := range map[string]*bool{
		"sync_backdrops":            &cfg.SyncBackdrops,
		"sync_logos":                &cfg.SyncLogos,
		"sync_season_posters":       &cfg.SyncSeasonPosters,
		"sync_collection_backdrops": &cfg.SyncCollectionBackdrops,
	} {
		if b, ok := values[key].(bool); ok {
			*dst = b
		}
	}
	return cfg
}

func (c Config) validate() error {
	var problems []string
	if c.PlexURL == "" {
		problems = append(problems, "Plex URL is not set")
	} else if u, err := url.Parse(c.PlexURL); err != nil || u.Scheme == "" || u.Host == "" {
		problems = append(problems, "Plex URL must look like http://host:32400")
	}
	if c.PlexToken == "" {
		problems = append(problems, "Plex token is not set")
	}
	if c.SiloAPIKey == "" {
		problems = append(problems, "Silo admin API key is not set")
	}
	if c.WebhookSecret == "" {
		problems = append(problems, "webhook secret is not set")
	}
	if len(problems) > 0 {
		return errors.New("plugin is not configured: " + strings.Join(problems, "; "))
	}
	return nil
}

func (c Config) wantsLibrary(title string) bool {
	if len(c.PlexLibraries) == 0 {
		return true
	}
	for _, name := range c.PlexLibraries {
		if strings.EqualFold(name, title) {
			return true
		}
	}
	return false
}

func str(v any) string {
	switch t := v.(type) {
	case string:
		return strings.TrimSpace(t)
	case nil:
		return ""
	default:
		return strings.TrimSpace(fmt.Sprint(t))
	}
}

func num(v any) (int, bool) {
	switch t := v.(type) {
	case float64:
		return int(t), true
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(t))
		return n, err == nil
	}
	return 0, false
}
