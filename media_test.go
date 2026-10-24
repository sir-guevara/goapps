package main

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func testPNG(t *testing.T) []byte {
	t.Helper()
	var b bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	img.Set(0, 0, color.RGBA{R: 100, G: 120, B: 220, A: 255})
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}
func fixtureWorker(a *app, handle func(*http.Request) (int, []byte, string)) *mediaWorker {
	worker := newMediaWorker(a.store)
	worker.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		code, data, location := handle(r)
		headers := make(http.Header)
		if location != "" {
			headers.Set("Location", location)
		}
		return &http.Response{StatusCode: code, Header: headers, Body: io.NopCloser(bytes.NewReader(data)), Request: r}, nil
	})
	return worker
}
func createMediaBookmark(t *testing.T, a *app) Bookmark {
	t.Helper()
	b := Bookmark{ID: "media-site", Title: "Media site", URL: "https://public.example/start", Description: "A useful site", Category: "Tools"}
	if err := a.store.create(b); err != nil {
		t.Fatal(err)
	}
	return b
}
func TestDiscoverCacheServeAndDeleteMedia(t *testing.T) {
	a := testApp(t)
	b := createMediaBookmark(t, a)
	img := testPNG(t)
	var hits []string
	worker := fixtureWorker(a, func(r *http.Request) (int, []byte, string) {
		hits = append(hits, r.URL.String())
		switch r.URL.String() {
		case b.URL:
			return 302, nil, "/pages/home"
		case "https://public.example/pages/home":
			return 200, []byte(`<html><head><link href="../assets/icon.png" rel="shortcut icon"><meta content="/assets/preview.png" property="og:image"></head><body></body></html>`), ""
		case "https://public.example/assets/icon.png", "https://public.example/assets/preview.png":
			return 200, img, ""
		}
		return 404, nil, ""
	})
	media := worker.fetch(context.Background(), b.URL)
	if !bytes.Equal(media.icon.data, img) || !bytes.Equal(media.preview.data, img) {
		t.Fatalf("missing fetched media; requests: %v", hits)
	}
	if err := a.store.saveMedia(context.Background(), b, media); err != nil {
		t.Fatal(err)
	}
	items, err := a.store.list()
	if err != nil {
		t.Fatal(err)
	}
	saved := items[len(items)-1]
	if saved.MediaPending || saved.IconURL == "" || saved.PreviewURL == "" {
		t.Fatalf("media unavailable in list: %+v", saved)
	}
	for _, path := range []string{saved.IconURL, saved.PreviewURL} {
		w := request(a, "GET", path, "", nil)
		if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), img) || w.Header().Get("Content-Type") != "image/png" {
			t.Fatalf("cached image: %d %s", w.Code, w.Body.String())
		}
		if w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("Content-Security-Policy") == "" {
			t.Fatal("missing cached image protections")
		}
	}
	cookie := signIn(t, a)
	if got := request(a, "DELETE", "/api/bookmarks/"+b.ID, "", cookie).Code; got != 204 {
		t.Fatalf("delete: %d", got)
	}
	var count int
	if err := a.store.db.QueryRow("SELECT count(*) FROM bookmark_media WHERE bookmark_id=?", b.ID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("orphaned images: %d %v", count, err)
	}
	for _, path := range []string{saved.IconURL, saved.PreviewURL} {
		if got := request(a, "GET", path, "", nil).Code; got != 404 {
			t.Fatalf("deleted image still served: %d", got)
		}
	}
	// A fetch finishing after deletion must never recreate the deleted cache.
	if err := a.store.saveMedia(context.Background(), b, media); err != nil {
		t.Fatal(err)
	}
	if err := a.store.db.QueryRow("SELECT count(*) FROM bookmark_media WHERE bookmark_id=?", b.ID).Scan(&count); err != nil || count != 0 {
		t.Fatal("late fetch restored deleted cache")
	}
}
func TestURLChangesInvalidateImagesAndDiscardLateFetch(t *testing.T) {
	a := testApp(t)
	old := createMediaBookmark(t, a)
	media := siteMedia{icon: cachedImage{data: testPNG(t), contentType: "image/png"}}
	if err := a.store.saveMedia(context.Background(), old, media); err != nil {
		t.Fatal(err)
	}
	updated := old
	updated.Title = "Renamed"
	if ok, err := a.store.update(old.ID, updated); err != nil || !ok {
		t.Fatal(err)
	}
	w := request(a, "GET", "/api/bookmarks/"+old.ID+"/icon", "", nil)
	if w.Code != 200 {
		t.Fatal("title-only edit removed icon")
	}
	updated.URL = "https://different.example"
	if _, err := a.store.update(old.ID, updated); err != nil {
		t.Fatal(err)
	}
	if err := a.store.saveMedia(context.Background(), old, media); err != nil {
		t.Fatal(err)
	}
	if got := request(a, "GET", "/api/bookmarks/"+old.ID+"/icon", "", nil).Code; got != 404 {
		t.Fatalf("stale icon survived URL edit: %d", got)
	}
	items, err := a.store.list()
	if err != nil {
		t.Fatal(err)
	}
	if !items[len(items)-1].MediaPending {
		t.Fatal("edited URL not queued for images")
	}
}
func TestMediaPersistsAfterReopen(t *testing.T) {
	a := testApp(t)
	b := createMediaBookmark(t, a)
	img := testPNG(t)
	if err := a.store.saveMedia(context.Background(), b, siteMedia{preview: cachedImage{data: img, contentType: "image/png"}}); err != nil {
		t.Fatal(err)
	}
	var seq int
	var name, path string
	if err := a.store.db.QueryRow("PRAGMA database_list").Scan(&seq, &name, &path); err != nil {
		t.Fatal(err)
	}
	a.store.db.Close()
	store, err := openStore(path, "")
	if err != nil {
		t.Fatal(err)
	}
	defer store.db.Close()
	a.store = store
	w := request(a, "GET", "/api/bookmarks/"+b.ID+"/preview", "", nil)
	if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), img) {
		t.Fatal("cached preview lost after restart")
	}
}
func TestMediaFallbacksAndResponseLimits(t *testing.T) {
	a := testApp(t)
	b := createMediaBookmark(t, a)
	img := testPNG(t)
	worker := fixtureWorker(a, func(r *http.Request) (int, []byte, string) {
		switch r.URL.Path {
		case "/start":
			return 200, []byte(`<head><link rel="icon" href="/not-image"><meta name="twitter:image" content="/too-big"></head>`), ""
		case "/not-image":
			return 200, []byte("<html>Not an image</html>"), ""
		case "/too-big":
			return 200, bytes.Repeat([]byte("x"), maxPreviewBytes+1), ""
		case "/favicon.ico":
			return 200, img, ""
		}
		return 404, nil, ""
	})
	media := worker.fetch(context.Background(), b.URL)
	if !bytes.Equal(media.icon.data, img) || len(media.preview.data) != 0 {
		t.Fatal("favicon fallback or preview size limit failed")
	}
	worker = fixtureWorker(a, func(r *http.Request) (int, []byte, string) { return 503, nil, "" })
	media = worker.fetch(context.Background(), b.URL)
	if err := a.store.saveMedia(context.Background(), b, media); err != nil {
		t.Fatal(err)
	}
	items, err := a.store.list()
	if err != nil {
		t.Fatal(err)
	}
	if items[len(items)-1].MediaPending || items[len(items)-1].IconURL != "" {
		t.Fatal("unavailable site did not settle to fallback")
	}
	u, _ := url.Parse(b.URL)
	worker = fixtureWorker(a, func(r *http.Request) (int, []byte, string) { return 200, bytes.Repeat([]byte("x"), maxPageBytes+1), "" })
	if _, _, err := worker.get(context.Background(), u, maxPageBytes); err == nil {
		t.Fatal("oversized page accepted")
	}
}
func TestBlockedMediaDestinations(t *testing.T) {
	for _, raw := range []string{"127.0.0.1", "10.1.2.3", "172.16.0.1", "192.168.1.1", "169.254.169.254", "100.100.100.200", "0.0.0.0", "224.0.0.1", "198.18.0.1", "::1", "fe80::1", "fc00::1", "::ffff:127.0.0.1", "64:ff9b::7f00:1"} {
		if isPublicIP(netip.MustParseAddr(raw)) {
			t.Errorf("allowed non-public IP: %s", raw)
		}
	}
	for _, raw := range []string{"8.8.8.8", "1.1.1.1", "2606:4700:4700::1111"} {
		if !isPublicIP(netip.MustParseAddr(raw)) {
			t.Errorf("blocked public IP: %s", raw)
		}
	}
	for _, raw := range []string{"file:///etc/passwd", "data:image/png;base64,AA==", "http://127.0.0.1/", "http://[::1]/", "https://user:secret@public.example/", "https://public.example:8080/"} {
		u, err := url.Parse(raw)
		if err == nil && validateMediaURL(u) == nil {
			t.Errorf("allowed unsafe URL: %s", raw)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if conn, err := publicDialContext(ctx, "tcp", "127.0.0.1:80"); err == nil {
		conn.Close()
		t.Fatal("dialed loopback")
	}
	a := testApp(t)
	worker := fixtureWorker(a, func(r *http.Request) (int, []byte, string) { return 302, nil, "http://127.0.0.1/secret" })
	u, _ := url.Parse("https://public.example/")
	if _, _, err := worker.get(context.Background(), u, maxPageBytes); err == nil {
		t.Fatal("followed redirect to loopback")
	}
}
func TestMetadataParsingAndSVGHandling(t *testing.T) {
	base, _ := url.Parse("https://public.example/page")
	got := parsePageImages([]byte(`<HEAD><base href="/assets/"><LINK REL="SHORTCUT ICON" HREF="icon.png?a=1&amp;b=2"><meta name="twitter:image" content="/twitter.png"><meta property="og:image" content="preview.png"></HEAD><body><meta property="og:image" content="/ignored.png">`), base)
	if len(got.icons) != 1 || got.icons[0] != "https://public.example/assets/icon.png?a=1&b=2" {
		t.Fatalf("relative icon/base: %v", got.icons)
	}
	if len(got.previews) != 2 || got.previews[0] != "https://public.example/assets/preview.png" {
		t.Fatalf("preview priority: %v", got.previews)
	}
	if got := imageContentType([]byte(`<svg xmlns="http://www.w3.org/2000/svg"></svg>`), true); got != "image/svg+xml" {
		t.Fatalf("SVG icon: %s", got)
	}
	if got := imageContentType([]byte(`<svg></svg>`), false); got != "" {
		t.Fatal("SVG accepted as a page preview")
	}
	if got := imageContentType([]byte(`<html><svg></svg></html>`), true); got != "" {
		t.Fatal("HTML accepted as SVG")
	}
	// Cached SVGs are served with a sandbox even when opened directly.
	a := testApp(t)
	b := createMediaBookmark(t, a)
	if err := a.store.saveMedia(context.Background(), b, siteMedia{icon: cachedImage{data: []byte(`<svg></svg>`), contentType: "image/svg+xml"}}); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	a.serveMedia(w, httptest.NewRequest("GET", "/", nil), b.ID, "icon")
	if !strings.Contains(w.Header().Get("Content-Security-Policy"), "sandbox") {
		t.Fatal("SVG not sandboxed")
	}
}

func TestBackgroundBackfillAndRetrySchedule(t *testing.T) {
	a := testApp(t)
	if _, err := a.store.db.Exec("DELETE FROM bookmarks"); err != nil {
		t.Fatal(err)
	}
	b := createMediaBookmark(t, a)
	calls := 0
	worker := fixtureWorker(a, func(r *http.Request) (int, []byte, string) { calls++; return 404, nil, "" })
	if err := worker.processPending(context.Background()); err != nil {
		t.Fatal(err)
	}
	initialCalls := calls
	if initialCalls == 0 {
		t.Fatal("existing bookmark was not backfilled")
	}
	if err := worker.processPending(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls != initialCalls {
		t.Fatal("unavailable media retried immediately")
	}
	if _, err := a.store.db.Exec("UPDATE bookmark_media SET checked_at=? WHERE bookmark_id=?", time.Now().Add(-25*time.Hour).Unix(), b.ID); err != nil {
		t.Fatal(err)
	}
	if err := worker.processPending(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls <= initialCalls {
		t.Fatal("unavailable media was not retried after 24 hours")
	}
	b.URL = "https://new.example/"
	if _, err := a.store.update(b.ID, b); err != nil {
		t.Fatal(err)
	}
	calls = 0
	if err := worker.processPending(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls == 0 {
		t.Fatal("changed URL was not backfilled")
	}
}

func TestMetadataFromLargePageHead(t *testing.T) {
	a := testApp(t)
	b := createMediaBookmark(t, a)
	img := testPNG(t)
	worker := fixtureWorker(a, func(r *http.Request) (int, []byte, string) {
		if r.URL.Path == "/start" {
			body := append([]byte(`<head><meta property="og:image" content="/preview.png"></head><body>`), bytes.Repeat([]byte("x"), maxPageBytes+100)...)
			return 200, body, ""
		}
		if r.URL.Path == "/preview.png" || r.URL.Path == "/favicon.ico" {
			return 200, img, ""
		}
		return 404, nil, ""
	})
	media := worker.fetch(context.Background(), b.URL)
	if !bytes.Equal(media.preview.data, img) {
		t.Fatal("metadata in a large page's head was discarded")
	}
}
