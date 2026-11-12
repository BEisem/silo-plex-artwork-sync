package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// errRateLimited is returned when Silo still answers 429 after retries.
var errRateLimited = errors.New("rate limited by Silo")

// errBadRequest is returned when Silo answers 400.
var errBadRequest = errors.New("rejected by Silo")

// errNotFound is returned when Silo answers 404 (e.g. a collection someone
// deleted by hand in Silo, or an item that isn't in the collection's
// libraries).
var errNotFound = errors.New("not found in Silo")

// errConflict is returned when Silo answers 409.
var errConflict = errors.New("conflict in Silo")

// errStale is returned when Silo answers 412: the resource changed between
// reading its ETag and writing with If-Match.
var errStale = errors.New("changed in Silo since it was read")

// errOrderTooLarge means a collection holds more titles than Silo lets a
// client reorder in one request.
var errOrderTooLarge = errors.New("collection is too large to reorder")

// siloAPI is the prefix of Silo's REST API. Silo retired /api/v1 (every v1
// route answers 410 Gone), so everything here is v2.
const siloAPI = "/api/v2"

// SiloClient calls Silo's own admin REST API with an admin API key. The plugin
// SDK has no host call for writing artwork or collections, so this is the only
// supported write path.
//
// Silo rate-limits each API key (20 requests/second by default), so every call
// goes through a pacer that stays under that, and a 429 is retried after the
// delay Silo asks for.
type SiloClient struct {
	baseURL string // e.g. http://127.0.0.1:8080
	apiKey  string
	http    *http.Client
	pace    *pacer
}

const (
	siloRequestsPerSecond = 15
	maxRateLimitRetries   = 6
)

func newSiloClient(baseURL, apiKey string) *SiloClient {
	return &SiloClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		http:    &http.Client{Timeout: 2 * time.Minute},
		pace:    newPacer(siloRequestsPerSecond),
	}
}

// pacer spaces calls evenly at a fixed rate across all goroutines.
type pacer struct {
	mu       sync.Mutex
	interval time.Duration
	next     time.Time
}

func newPacer(perSecond int) *pacer {
	return &pacer{interval: time.Second / time.Duration(perSecond)}
}

func (p *pacer) wait(ctx context.Context) error {
	p.mu.Lock()
	now := time.Now()
	if p.next.Before(now) {
		p.next = now
	}
	slot := p.next
	p.next = p.next.Add(p.interval)
	p.mu.Unlock()
	return sleepCtx(ctx, time.Until(slot))
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// retryAfter reads how long Silo asked us to wait on a 429.
func retryAfter(resp *http.Response, body []byte) time.Duration {
	if n, err := strconv.Atoi(strings.TrimSpace(resp.Header.Get("Retry-After"))); err == nil && n > 0 {
		return time.Duration(n) * time.Second
	}
	var parsed struct {
		RetryAfter int `json:"retry_after"`
	}
	if json.Unmarshal(body, &parsed) == nil && parsed.RetryAfter > 0 {
		return time.Duration(parsed.RetryAfter) * time.Second
	}
	return time.Second
}

// request is one call to Silo. etag receives the response's ETag header.
type request struct {
	method, path string
	contentType  string
	body         []byte
	ifMatch      string
	out          any
	etag         *string
}

func (s *SiloClient) do(ctx context.Context, r request) error {
	for attempt := 0; ; attempt++ {
		if err := s.pace.wait(ctx); err != nil {
			return err
		}
		var reader io.Reader
		if r.body != nil {
			reader = bytes.NewReader(r.body)
		}
		req, err := http.NewRequestWithContext(ctx, r.method, s.baseURL+siloAPI+r.path, reader)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+s.apiKey)
		req.Header.Set("Accept", "application/json, application/problem+json")
		if r.contentType != "" {
			req.Header.Set("Content-Type", r.contentType)
		}
		if r.ifMatch != "" {
			req.Header.Set("If-Match", r.ifMatch)
		}
		resp, err := s.http.Do(req)
		if err != nil {
			return fmt.Errorf("silo %s %s: %w", r.method, r.path, err)
		}
		if resp.StatusCode == http.StatusTooManyRequests && attempt < maxRateLimitRetries {
			snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
			resp.Body.Close()
			if err := sleepCtx(ctx, retryAfter(resp, snippet)); err != nil {
				return err
			}
			continue
		}
		err = s.handle(resp, r)
		resp.Body.Close()
		return err
	}
}

// problemDetail pulls the human-readable part out of Silo's
// application/problem+json error bodies.
func problemDetail(body []byte) string {
	var p struct {
		Title  string `json:"title"`
		Detail string `json:"detail"`
		Errors []struct {
			Location string `json:"location"`
			Detail   string `json:"detail"`
		} `json:"errors"`
	}
	if json.Unmarshal(body, &p) != nil || (p.Detail == "" && p.Title == "") {
		return strings.TrimSpace(string(body))
	}
	msg := p.Detail
	if msg == "" {
		msg = p.Title
	}
	for _, e := range p.Errors {
		msg += fmt.Sprintf(" (%s: %s)", e.Location, e.Detail)
	}
	return msg
}

func (s *SiloClient) handle(resp *http.Response, r request) error {
	fail := func(kind error) error {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		if kind == nil {
			return fmt.Errorf("silo %s %s: HTTP %d: %s", r.method, r.path, resp.StatusCode, problemDetail(snippet))
		}
		return fmt.Errorf("silo %s %s: %w: %s", r.method, r.path, kind, problemDetail(snippet))
	}
	switch code := resp.StatusCode; {
	case code == http.StatusNotFound:
		return fail(errNotFound)
	case code == http.StatusBadRequest || code == http.StatusUnprocessableEntity:
		return fail(errBadRequest)
	case code == http.StatusConflict:
		return fail(errConflict)
	case code == http.StatusPreconditionFailed:
		return fail(errStale)
	case code == http.StatusTooManyRequests:
		return fail(errRateLimited)
	case code == http.StatusGone:
		return fail(errors.New("this Silo no longer serves the API version the plugin uses; update the plugin"))
	case code == http.StatusUnauthorized || code == http.StatusForbidden:
		return fail(fmt.Errorf("HTTP %d (the API key must be an unscoped admin key)", code))
	case code >= 300:
		return fail(nil)
	}
	if r.etag != nil {
		*r.etag = resp.Header.Get("ETag")
	}
	if r.out != nil {
		return json.NewDecoder(resp.Body).Decode(r.out)
	}
	return nil
}

func (s *SiloClient) doJSON(ctx context.Context, method, path string, payload, out any) error {
	r := request{method: method, path: path, out: out}
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		r.body, r.contentType = data, "application/json"
	}
	return s.do(ctx, r)
}

// multipartImage builds a form with the image under field (plus any plain
// text fields).
func multipartImage(field string, data []byte, extra map[string]string) (string, []byte, error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for k, v := range extra {
		if err := w.WriteField(k, v); err != nil {
			return "", nil, err
		}
	}
	header := textproto.MIMEHeader{}
	header.Set("Content-Disposition", fmt.Sprintf(`form-data; name=%q; filename="poster"`, field))
	header.Set("Content-Type", sniffImageType(data))
	part, err := w.CreatePart(header)
	if err != nil {
		return "", nil, err
	}
	if _, err := part.Write(data); err != nil {
		return "", nil, err
	}
	if err := w.Close(); err != nil {
		return "", nil, err
	}
	return w.FormDataContentType(), buf.Bytes(), nil
}

// UploadPoster makes data the item's poster. Silo stores the bytes as
// uploaded artwork and locks the item's images, so a later metadata refresh
// doesn't replace it. contentID must be a movie or series; season and episode
// IDs are never passed here.
func (s *SiloClient) UploadPoster(ctx context.Context, contentID string, data []byte) error {
	data, err := fitForUpload(data)
	if err != nil {
		return err
	}
	ct, body, err := multipartImage("image", data, map[string]string{"type": "poster"})
	if err != nil {
		return err
	}
	return s.do(ctx, request{method: http.MethodPost, path: "/admin/items/" + url.PathEscape(contentID) + "/images/upload",
		contentType: ct, body: body})
}

// siloCollectionInfo is one entry of Silo's admin collection list.
type siloCollectionInfo struct {
	ID         string
	Slug       string
	Title      string
	LibraryIDs []int
}

// v2 sends every identifier, library ids included, as a string.
func idStrings(ids []int) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = strconv.Itoa(id)
	}
	return out
}

func idInts(ids []string) []int {
	out := make([]int, 0, len(ids))
	for _, id := range ids {
		if n, err := strconv.Atoi(id); err == nil {
			out = append(out, n)
		}
	}
	return out
}

// ListCollections returns every server collection, across all libraries.
func (s *SiloClient) ListCollections(ctx context.Context) ([]siloCollectionInfo, error) {
	var out struct {
		Items []struct {
			ID         string   `json:"id"`
			Slug       string   `json:"slug"`
			Title      string   `json:"title"`
			LibraryID  string   `json:"library_id"`
			LibraryIDs []string `json:"library_ids"`
		} `json:"items"`
	}
	if err := s.doJSON(ctx, http.MethodGet, "/admin/collections", nil, &out); err != nil {
		return nil, err
	}
	list := make([]siloCollectionInfo, 0, len(out.Items))
	for _, c := range out.Items {
		libs := c.LibraryIDs
		if len(libs) == 0 && c.LibraryID != "" {
			libs = []string{c.LibraryID}
		}
		list = append(list, siloCollectionInfo{ID: c.ID, Slug: c.Slug, Title: c.Title, LibraryIDs: idInts(libs)})
	}
	return list, nil
}

// CreateCollection creates a manual (hand-curated) server collection.
func (s *SiloClient) CreateCollection(ctx context.Context, title, slug string, libraryIDs []int) (string, error) {
	var out struct {
		ID string `json:"id"`
	}
	err := s.doJSON(ctx, http.MethodPost, "/admin/collections", map[string]any{
		"title":           title,
		"slug":            slug,
		"library_ids":     idStrings(libraryIDs),
		"collection_type": "manual",
		"description":     "Synced from Plex by Plex Artwork Sync.",
	}, &out)
	if err != nil {
		return "", err
	}
	if out.ID == "" {
		return "", fmt.Errorf("silo create collection %q: response had no id", title)
	}
	return out.ID, nil
}

// collectionETag reads a collection's current validator, which Silo requires
// (as If-Match) on every edit and delete.
func (s *SiloClient) collectionETag(ctx context.Context, id string) (string, error) {
	var etag string
	err := s.do(ctx, request{method: http.MethodGet, path: "/admin/collections/" + url.PathEscape(id), etag: &etag})
	if err == nil && etag == "" {
		err = fmt.Errorf("silo GET /admin/collections/%s: no ETag in the response", id)
	}
	return etag, err
}

// withETag runs write with the collection's current ETag, reading it again
// and retrying once if the collection changed in between.
func (s *SiloClient) withETag(ctx context.Context, id string, write func(etag string) error) error {
	for attempt := 0; ; attempt++ {
		etag, err := s.collectionETag(ctx, id)
		if err != nil {
			return err
		}
		err = write(etag)
		if errors.Is(err, errStale) && attempt == 0 {
			continue
		}
		return err
	}
}

// UpdateCollection sets a collection's title and libraries.
func (s *SiloClient) UpdateCollection(ctx context.Context, id, title string, libraryIDs []int) error {
	data, err := json.Marshal(map[string]any{"title": title, "library_ids": idStrings(libraryIDs)})
	if err != nil {
		return err
	}
	return s.withETag(ctx, id, func(etag string) error {
		return s.do(ctx, request{method: http.MethodPatch, path: "/admin/collections/" + url.PathEscape(id),
			contentType: "application/json", body: data, ifMatch: etag})
	})
}

// SetCollectionPoster uploads a collection's poster.
func (s *SiloClient) SetCollectionPoster(ctx context.Context, id string, poster []byte) error {
	poster, err := fitForUpload(poster)
	if err != nil {
		return err
	}
	ct, body, err := multipartImage("image", poster, nil)
	if err != nil {
		return err
	}
	return s.do(ctx, request{method: http.MethodPut, path: "/admin/collections/" + url.PathEscape(id) + "/poster",
		contentType: ct, body: body})
}

// AddCollectionItem adds an item at position if the collection doesn't hold
// it yet; an item already there keeps its position. Silo answers 404 when the
// item isn't in any of the collection's libraries.
func (s *SiloClient) AddCollectionItem(ctx context.Context, collectionID, contentID string, position int) error {
	return s.doJSON(ctx, http.MethodPut,
		"/admin/collections/"+url.PathEscape(collectionID)+"/items/"+url.PathEscape(contentID),
		map[string]int{"position": position}, nil)
}

// ReorderCollection puts a collection's items in the given order. Silo only
// accepts a complete order, so items the collection holds that aren't in
// ordered (added in Silo by hand, say) keep their relative order after them.
// Silo reads and reorders at most 200 items; a larger collection returns
// errOrderTooLarge.
func (s *SiloClient) ReorderCollection(ctx context.Context, collectionID string, ordered []string) error {
	path := "/admin/collections/" + url.PathEscape(collectionID) + "/items/order"
	for attempt := 0; ; attempt++ {
		var current struct {
			OrderedIDs []string `json:"ordered_ids"`
			HasMore    bool     `json:"has_more"`
		}
		var etag string
		if err := s.do(ctx, request{method: http.MethodGet, path: path, out: &current, etag: &etag}); err != nil {
			return err
		}
		if current.HasMore {
			return errOrderTooLarge
		}
		want := completeOrder(ordered, current.OrderedIDs)
		if slices.Equal(want, current.OrderedIDs) {
			return nil
		}
		data, err := json.Marshal(map[string][]string{"ordered_ids": want})
		if err != nil {
			return err
		}
		err = s.do(ctx, request{method: http.MethodPut, path: path, contentType: "application/json", body: data, ifMatch: etag})
		if errors.Is(err, errStale) && attempt == 0 {
			continue
		}
		return err
	}
}

// completeOrder is ordered restricted to the items Silo holds, followed by
// the items Silo holds that ordered doesn't mention, in Silo's order.
func completeOrder(ordered, held []string) []string {
	have := make(map[string]bool, len(held))
	for _, id := range held {
		have[id] = true
	}
	out := make([]string, 0, len(held))
	seen := map[string]bool{}
	for _, id := range ordered {
		if have[id] && !seen[id] {
			out = append(out, id)
			seen[id] = true
		}
	}
	for _, id := range held {
		if !seen[id] {
			out = append(out, id)
		}
	}
	return out
}

func (s *SiloClient) RemoveCollectionItem(ctx context.Context, collectionID, contentID string) error {
	err := s.doJSON(ctx, http.MethodDelete,
		"/admin/collections/"+url.PathEscape(collectionID)+"/items/"+url.PathEscape(contentID), nil, nil)
	if errors.Is(err, errNotFound) {
		return nil
	}
	return err
}

// DeleteCollection deletes a collection. Silo refuses (409) while a Home or
// library page row still shows it.
func (s *SiloClient) DeleteCollection(ctx context.Context, collectionID string) error {
	err := s.withETag(ctx, collectionID, func(etag string) error {
		return s.do(ctx, request{method: http.MethodDelete, path: "/admin/collections/" + url.PathEscape(collectionID), ifMatch: etag})
	})
	switch {
	case errors.Is(err, errNotFound):
		return nil
	case errors.Is(err, errConflict):
		return fmt.Errorf("%w (a Home or library page row still shows it; remove it from that row in Silo first)", err)
	}
	return err
}

// fieldImagesLock is Silo's FieldImages lock (metadata.FieldImages). Silo sets
// it when a poster is applied or uploaded, which is what stops a refresh
// replacing it.
const fieldImagesLock = 10

// LockedFields returns the item's locked metadata fields.
func (s *SiloClient) LockedFields(ctx context.Context, contentID string) ([]int, error) {
	var out struct {
		LockedFields []int `json:"locked_fields"`
	}
	err := s.doJSON(ctx, http.MethodGet, "/catalog/items/"+url.PathEscape(contentID), nil, &out)
	return out.LockedFields, err
}

// SetLockedFields replaces the item's locked fields (Silo stores the list
// whole, so callers pass every lock they want to keep).
func (s *SiloClient) SetLockedFields(ctx context.Context, contentID string, fields []int) error {
	if fields == nil {
		fields = []int{}
	}
	return s.doJSON(ctx, http.MethodPatch, "/admin/items/"+url.PathEscape(contentID)+"/metadata",
		map[string]any{"locked_fields": fields}, nil)
}

// RefreshItem queues Silo's quick metadata refresh for an item and returns
// the job id. A quick refresh replaces every unlocked field, so with the
// image lock removed Silo picks its default artwork again.
func (s *SiloClient) RefreshItem(ctx context.Context, contentID string) (string, error) {
	var out struct {
		ID string `json:"id"`
	}
	err := s.doJSON(ctx, http.MethodPost, "/admin/items/"+url.PathEscape(contentID)+"/refresh-metadata",
		map[string]string{"mode": "quick"}, &out)
	if err == nil && out.ID == "" {
		err = fmt.Errorf("silo refresh %s: response had no job id", contentID)
	}
	return out.ID, err
}

// JobStatus reports an admin job's state (queued, running, canceling,
// succeeded, failed or canceled), whether it has finished, and why it failed.
func (s *SiloClient) JobStatus(ctx context.Context, jobID string) (state string, terminal bool, failure string, err error) {
	var out struct {
		State    string `json:"state"`
		Terminal bool   `json:"terminal"`
		Failure  *struct {
			Detail string `json:"detail"`
		} `json:"failure"`
	}
	err = s.doJSON(ctx, http.MethodGet, "/admin/jobs/"+url.PathEscape(jobID), nil, &out)
	if out.Failure != nil {
		failure = out.Failure.Detail
	}
	return out.State, out.Terminal, failure, err
}

// Ping confirms the API key is accepted for admin routes.
func (s *SiloClient) Ping(ctx context.Context) error {
	return s.doJSON(ctx, http.MethodGet, "/admin/collections", nil, nil)
}
