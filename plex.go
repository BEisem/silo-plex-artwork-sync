package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const plexPageSize = 500

// PlexClient is a minimal read-only client for the Plex endpoints the sync
// needs. It never writes to Plex.
type PlexClient struct {
	baseURL string
	token   string
	http    *http.Client
}

func newPlexClient(baseURL, token string) *PlexClient {
	return &PlexClient{baseURL: baseURL, token: token, http: &http.Client{Timeout: 60 * time.Second}}
}

type plexSection struct {
	Key   string `json:"key"`
	Title string `json:"title"`
	Type  string `json:"type"` // "movie" or "show"
	UUID  string `json:"uuid"`
}

type plexGUID struct {
	ID string `json:"id"`
}

type plexItem struct {
	RatingKey string `json:"ratingKey"`
	Title     string `json:"title"`
	Type      string `json:"type"` // "movie", "show", "collection", ...
	Thumb     string `json:"thumb"`
	Art       string `json:"art"`   // the selected backdrop
	Index     *int   `json:"index"` // season number on a season
	Year      int    `json:"year"`
	// Image lists the item's currently selected artwork by type ("coverPoster",
	// "background", "clearLogo"). Plex only lists what is set.
	Images []plexImage `json:"Image"`
	// Plex sends both "guid" (one string, e.g. plex://movie/...) and "Guid"
	// (the external ID list). encoding/json matches keys case-insensitively,
	// so "guid" needs its own field or it would be decoded into GUIDs.
	PlexGUID string     `json:"guid"`
	GUIDs    []plexGUID `json:"Guid"`
	Media    []struct {
		VideoResolution string `json:"videoResolution"`
	} `json:"Media"`
}

// tmdbID returns the item's TMDB id from its Plex GUIDs, or "".
func (i plexItem) tmdbID() string {
	for _, g := range i.GUIDs {
		if id, ok := strings.CutPrefix(g.ID, "tmdb://"); ok && id != "" {
			return id
		}
	}
	return ""
}

type plexImage struct {
	Type string `json:"type"`
	URL  string `json:"url"`
}

// clearLogo returns the path of the item's selected clear logo, and whether
// the listing reported the item's image list at all.
func (i plexItem) clearLogo() (path string, listed bool) {
	for _, im := range i.Images {
		if im.Type == "clearLogo" {
			return im.URL, true
		}
	}
	return "", len(i.Images) > 0
}

type plexDirectory struct {
	Key   string `json:"key"`
	Title string `json:"title"`
}

type plexContainer struct {
	MediaContainer struct {
		TotalSize int             `json:"totalSize"`
		Size      int             `json:"size"`
		Directory []plexDirectory `json:"Directory"`
		Metadata  []plexItem      `json:"Metadata"`
	} `json:"MediaContainer"`
}

type plexSectionsContainer struct {
	MediaContainer struct {
		Directory []plexSection `json:"Directory"`
	} `json:"MediaContainer"`
}

func (p *PlexClient) newRequest(ctx context.Context, path string, query url.Values) (*http.Request, error) {
	u := p.baseURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Plex-Token", p.token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Plex-Client-Identifier", "silo-kometa-sync")
	req.Header.Set("X-Plex-Product", "Silo Plex Artwork Sync")
	return req, nil
}

func (p *PlexClient) getJSON(ctx context.Context, path string, query url.Values, headers map[string]string, out any) error {
	req, err := p.newRequest(ctx, path, query)
	if err != nil {
		return err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := p.http.Do(req)
	if err != nil {
		return fmt.Errorf("plex %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return fmt.Errorf("plex %s: unauthorized (check the Plex token)", path)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("plex %s: HTTP %d: %s", path, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// Sections returns the movie and show libraries.
func (p *PlexClient) Sections(ctx context.Context) ([]plexSection, error) {
	var c plexSectionsContainer
	if err := p.getJSON(ctx, "/library/sections", nil, nil, &c); err != nil {
		return nil, err
	}
	out := make([]plexSection, 0, len(c.MediaContainer.Directory))
	for _, s := range c.MediaContainer.Directory {
		if s.Type == "movie" || s.Type == "show" {
			out = append(out, s)
		}
	}
	return out, nil
}

// paged walks a Plex list endpoint using container start/size headers.
func (p *PlexClient) paged(ctx context.Context, path string, query url.Values) ([]plexItem, error) {
	var all []plexItem
	for start := 0; ; start += plexPageSize {
		var c plexContainer
		headers := map[string]string{
			"X-Plex-Container-Start": strconv.Itoa(start),
			"X-Plex-Container-Size":  strconv.Itoa(plexPageSize),
		}
		if err := p.getJSON(ctx, path, query, headers, &c); err != nil {
			return nil, err
		}
		all = append(all, c.MediaContainer.Metadata...)
		got := len(c.MediaContainer.Metadata)
		total := c.MediaContainer.TotalSize
		if got == 0 || got < plexPageSize || (total > 0 && len(all) >= total) {
			return all, nil
		}
	}
}

// labelKey finds the filter key for a label (e.g. Kometa's "Overlay") in a
// section. ok is false when no item in the section carries the label.
func (p *PlexClient) labelKey(ctx context.Context, sectionKey, label string) (string, bool, error) {
	var c plexContainer
	if err := p.getJSON(ctx, "/library/sections/"+url.PathEscape(sectionKey)+"/label", nil, nil, &c); err != nil {
		return "", false, err
	}
	for _, d := range c.MediaContainer.Directory {
		if strings.EqualFold(d.Title, label) {
			return d.Key, true, nil
		}
	}
	return "", false, nil
}

// Items lists the section's top-level movies or shows. Seasons and episodes
// are never requested. When onlyLabel is set, only items with that label are
// returned.
func (p *PlexClient) Items(ctx context.Context, section plexSection, onlyLabel string) ([]plexItem, error) {
	plexType := "1" // movie
	if section.Type == "show" {
		plexType = "2" // show
	}
	query := url.Values{"type": {plexType}, "includeGuids": {"1"}}
	if onlyLabel != "" {
		key, ok, err := p.labelKey(ctx, section.Key, onlyLabel)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, nil
		}
		query.Set("label", key)
	}
	items, err := p.paged(ctx, "/library/sections/"+url.PathEscape(section.Key)+"/all", query)
	if err != nil {
		return nil, err
	}
	// Defense in depth: keep only top-level movies/shows.
	out := items[:0]
	for _, it := range items {
		if it.Type == "movie" || it.Type == "show" {
			out = append(out, it)
		}
	}
	return out, nil
}

// Item fetches one item's current metadata. found is false when the item no
// longer exists in Plex.
func (p *PlexClient) Item(ctx context.Context, ratingKey string) (plexItem, bool, error) {
	var c plexContainer
	err := p.getJSON(ctx, "/library/metadata/"+url.PathEscape(ratingKey), url.Values{"includeGuids": {"1"}}, nil, &c)
	if err != nil {
		if strings.Contains(err.Error(), "HTTP 404") {
			return plexItem{}, false, nil
		}
		return plexItem{}, false, err
	}
	if len(c.MediaContainer.Metadata) == 0 {
		return plexItem{}, false, nil
	}
	return c.MediaContainer.Metadata[0], true, nil
}

// Collections lists a section's collections.
func (p *PlexClient) Collections(ctx context.Context, section plexSection) ([]plexItem, error) {
	return p.paged(ctx, "/library/sections/"+url.PathEscape(section.Key)+"/collections", nil)
}

// CollectionChildren lists a collection's members.
func (p *PlexClient) CollectionChildren(ctx context.Context, ratingKey string) ([]plexItem, error) {
	return p.paged(ctx, "/library/collections/"+url.PathEscape(ratingKey)+"/children", url.Values{"includeGuids": {"1"}})
}

// Image downloads an image path such as /library/metadata/123/thumb/1698000000.
func (p *PlexClient) Image(ctx context.Context, thumbPath string) ([]byte, error) {
	if !isPlexPath(thumbPath) {
		return nil, fmt.Errorf("unexpected image path %q", thumbPath)
	}
	req, err := p.newRequest(ctx, thumbPath, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "image/*")
	resp, err := p.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("plex image: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("plex image %s: HTTP %d", thumbPath, resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 40<<20))
}

// Seasons lists a show's seasons (never its episodes). Season 0 is Specials.
func (p *PlexClient) Seasons(ctx context.Context, showRatingKey string) ([]plexItem, error) {
	items, err := p.paged(ctx, "/library/metadata/"+url.PathEscape(showRatingKey)+"/children", nil)
	if err != nil {
		return nil, err
	}
	out := items[:0]
	for _, it := range items {
		if it.Type == "season" && it.Index != nil {
			out = append(out, it)
		}
	}
	return out, nil
}

// plexLogoChoice is one entry of /library/metadata/{id}/clearLogos.
type plexLogoChoice struct {
	Key      string `json:"key"`
	Selected bool   `json:"selected"`
}

// SelectedClearLogo asks Plex which clear logo is selected for an item. It is
// the fallback for servers whose listings omit the Image array. ok is false
// when no logo, or more than one, is marked selected, or when the selected
// one is not a local Plex path: the plugin never guesses.
func (p *PlexClient) SelectedClearLogo(ctx context.Context, ratingKey string) (path string, ok bool, err error) {
	var c struct {
		MediaContainer struct {
			Metadata []plexLogoChoice `json:"Metadata"`
		} `json:"MediaContainer"`
	}
	if err := p.getJSON(ctx, "/library/metadata/"+url.PathEscape(ratingKey)+"/clearLogos", nil, nil, &c); err != nil {
		return "", false, err
	}
	var sel []string
	for _, l := range c.MediaContainer.Metadata {
		if l.Selected {
			sel = append(sel, l.Key)
		}
	}
	if len(sel) != 1 || !isPlexPath(sel[0]) {
		return "", false, nil
	}
	return sel[0], true, nil
}

// isPlexPath accepts only server-relative paths, so a download never follows
// a URL to another host.
func isPlexPath(path string) bool {
	return strings.HasPrefix(path, "/") && !strings.HasPrefix(path, "//") && !strings.Contains(path, "://")
}
