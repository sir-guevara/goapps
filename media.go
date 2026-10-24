package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"golang.org/x/net/html"
)

const (
	maxPageBytes    = 1 << 20
	maxIconBytes    = 512 << 10
	maxPreviewBytes = 4 << 20
)

type cachedImage struct {
	data        []byte
	contentType string
}
type siteMedia struct{ icon, preview cachedImage }
type mediaWorker struct {
	store  *bookmarkStore
	client *http.Client
	wake   chan struct{}
}

// No proxy or ambient cookies: every connection, including redirects and image
// hosts, is resolved and pinned to a public IP before dialing.
func newMediaWorker(store *bookmarkStore) *mediaWorker {
	transport := &http.Transport{
		DialContext:            publicDialContext,
		TLSHandshakeTimeout:    4 * time.Second,
		ResponseHeaderTimeout:  5 * time.Second,
		MaxResponseHeaderBytes: 64 << 10,
		MaxIdleConns:           4, MaxIdleConnsPerHost: 2,
		IdleConnTimeout: 30 * time.Second,
	}
	return &mediaWorker{store: store, wake: make(chan struct{}, 1), client: &http.Client{
		Transport: transport, Timeout: 6 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 4 {
				return errors.New("too many redirects")
			}
			return validateMediaURL(req.URL)
		},
	}}
}

func validateMediaURL(u *url.URL) error {
	if u == nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil {
		return errors.New("media URL must be public HTTP or HTTPS without credentials")
	}
	if port := u.Port(); port != "" && port != "80" && port != "443" {
		return errors.New("unsupported media port")
	}
	if ip, err := netip.ParseAddr(u.Hostname()); err == nil && !isPublicIP(ip) {
		return errors.New("non-public media address")
	}
	return nil
}

var blockedNetworks = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"), netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"), netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"), netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("2001::/23"), netip.MustParsePrefix("2002::/16"),
	netip.MustParsePrefix("64:ff9b::/96"), netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("2001:db8::/32"),
}

func isPublicIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsValid() || ip.Zone() != "" || !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		return false
	}
	for _, prefix := range blockedNetworks {
		if prefix.Contains(ip) {
			return false
		}
	}
	return true
}
func publicDialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	if port != "80" && port != "443" {
		return nil, errors.New("unsupported media port")
	}
	ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, err
	}
	if len(ips) == 0 {
		return nil, errors.New("media host has no addresses")
	}
	for _, ip := range ips {
		if !isPublicIP(ip) {
			return nil, errors.New("media host resolves to a non-public address")
		}
	}
	dialer := net.Dialer{Timeout: 3 * time.Second}
	var lastErr error
	for _, ip := range ips {
		conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

func (m *mediaWorker) notify() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}
func (m *mediaWorker) run(ctx context.Context) {
	defer m.client.CloseIdleConnections()
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		if err := m.processPending(ctx); err != nil && ctx.Err() == nil {
			log.Printf("fetch site images: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-m.wake:
		case <-ticker.C:
		}
	}
}
func (m *mediaWorker) processPending(ctx context.Context) error {
	// Read the batch before fetching, so slow sites never hold a SQLite connection.
	rows, err := m.store.db.QueryContext(ctx, `SELECT b.id,b.url FROM bookmarks b
 LEFT JOIN bookmark_media m ON m.bookmark_id=b.id
 WHERE m.bookmark_id IS NULL OR m.source_url!=b.url OR
 ((COALESCE(length(m.icon),0)=0 OR COALESCE(length(m.preview),0)=0) AND m.checked_at<?)
 ORDER BY b.rowid`, time.Now().Add(-24*time.Hour).Unix())
	if err != nil {
		return err
	}
	var pending []Bookmark
	for rows.Next() {
		var b Bookmark
		if err := rows.Scan(&b.ID, &b.URL); err != nil {
			rows.Close()
			return err
		}
		pending = append(pending, b)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, b := range pending {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		fetchCtx, cancel := context.WithTimeout(ctx, 18*time.Second)
		media := m.fetch(fetchCtx, b.URL)
		cancel()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := m.store.saveMedia(ctx, b, media); err != nil {
			return err
		}
	}
	return nil
}
func (s *bookmarkStore) saveMedia(ctx context.Context, b Bookmark, media siteMedia) error {
	// The URL check discards fetches that finished after an edit or deletion.
	_, err := s.db.ExecContext(ctx, `INSERT INTO bookmark_media(bookmark_id,source_url,checked_at,icon,icon_type,preview,preview_type)
 SELECT id,url,?,?,?,?,? FROM bookmarks WHERE id=? AND url=?
 ON CONFLICT(bookmark_id) DO UPDATE SET source_url=excluded.source_url,checked_at=excluded.checked_at,
 icon=COALESCE(excluded.icon,bookmark_media.icon),icon_type=CASE WHEN excluded.icon IS NULL THEN bookmark_media.icon_type ELSE excluded.icon_type END,
 preview=COALESCE(excluded.preview,bookmark_media.preview),preview_type=CASE WHEN excluded.preview IS NULL THEN bookmark_media.preview_type ELSE excluded.preview_type END`,
		time.Now().Unix(), media.icon.data, media.icon.contentType, media.preview.data, media.preview.contentType, b.ID, b.URL)
	return err
}
func (a *app) serveMedia(w http.ResponseWriter, r *http.Request, id, kind string) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		methodNotAllowed(w)
		return
	}
	var data []byte
	var contentType string
	query := "SELECT m.icon,m.icon_type FROM bookmark_media m JOIN bookmarks b ON b.id=m.bookmark_id AND b.url=m.source_url WHERE b.id=?"
	if kind == "preview" {
		query = "SELECT m.preview,m.preview_type FROM bookmark_media m JOIN bookmarks b ON b.id=m.bookmark_id AND b.url=m.source_url WHERE b.id=?"
	}
	err := a.store.db.QueryRowContext(r.Context(), query, id).Scan(&data, &contentType)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && len(data) == 0) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Could not load site image.")
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	http.ServeContent(w, r, kind, time.Time{}, bytes.NewReader(data))
}

func (m *mediaWorker) get(ctx context.Context, u *url.URL, limit int64) ([]byte, *url.URL, error) {
	return m.getResponse(ctx, u, limit, false)
}

func (m *mediaWorker) getResponse(ctx context.Context, u *url.URL, limit int64, truncate bool) ([]byte, *url.URL, error) {
	if err := validateMediaURL(u); err != nil {
		return nil, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("User-Agent", "G&OApps/1.0 (site preview fetcher)")
	resp, err := m.client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("site returned %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, nil, err
	}
	if int64(len(data)) > limit {
		if !truncate {
			return nil, nil, errors.New("site response exceeds size limit")
		}
		data = data[:limit]
	}
	return data, resp.Request.URL, nil
}

type pageImages struct{ icons, previews []string }

func parsePageImages(data []byte, base *url.URL) pageImages {
	tokenizer := html.NewTokenizer(bytes.NewReader(data))
	var icons, touchIcons, ogImages, twitterImages []string
	resolve := func(raw string) string {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			return ""
		}
		u, err := url.Parse(raw)
		if err != nil {
			return ""
		}
		u = base.ResolveReference(u)
		if validateMediaURL(u) != nil {
			return ""
		}
		u.Fragment = ""
		return u.String()
	}
	baseSet := false
	for {
		tt := tokenizer.Next()
		if tt == html.ErrorToken {
			break
		}
		if tt == html.EndTagToken {
			name, _ := tokenizer.TagName()
			if string(name) == "head" {
				break
			}
		}
		if tt != html.StartTagToken && tt != html.SelfClosingTagToken {
			continue
		}
		token := tokenizer.Token()
		if token.Data == "body" {
			break
		}
		attrs := map[string]string{}
		for _, attr := range token.Attr {
			attrs[attr.Key] = attr.Val
		}
		switch token.Data {
		case "base":
			if !baseSet {
				if raw := resolve(attrs["href"]); raw != "" {
					base, _ = url.Parse(raw)
					baseSet = true
				}
			}
		case "link":
			raw := resolve(attrs["href"])
			if raw == "" {
				continue
			}
			for _, rel := range strings.Fields(strings.ToLower(attrs["rel"])) {
				if rel == "icon" && len(icons) < 4 {
					icons = append(icons, raw)
				}
				if rel == "apple-touch-icon" && len(touchIcons) < 2 {
					touchIcons = append(touchIcons, raw)
				}
			}
		case "meta":
			key := strings.ToLower(attrs["property"])
			if key == "" {
				key = strings.ToLower(attrs["name"])
			}
			raw := resolve(attrs["content"])
			if raw == "" {
				continue
			}
			if (key == "og:image" || key == "og:image:url" || key == "og:image:secure_url") && len(ogImages) < 3 {
				ogImages = append(ogImages, raw)
			}
			if (key == "twitter:image" || key == "twitter:image:src") && len(twitterImages) < 2 {
				twitterImages = append(twitterImages, raw)
			}
		}
	}
	return pageImages{icons: append(icons, touchIcons...), previews: append(ogImages, twitterImages...)}
}
func (m *mediaWorker) fetch(ctx context.Context, raw string) siteMedia {
	var result siteMedia
	base, err := url.Parse(raw)
	if err != nil || validateMediaURL(base) != nil {
		return result
	}
	candidates := pageImages{}
	if data, finalURL, err := m.getResponse(ctx, base, maxPageBytes, true); err == nil {
		base = finalURL
		candidates = parsePageImages(data, base)
	}
	candidates.icons = append(candidates.icons, base.ResolveReference(&url.URL{Path: "/favicon.ico"}).String())
	result.icon = m.firstImage(ctx, candidates.icons, maxIconBytes, true)
	result.preview = m.firstImage(ctx, candidates.previews, maxPreviewBytes, false)
	return result
}
func (m *mediaWorker) firstImage(ctx context.Context, candidates []string, limit int64, allowSVG bool) cachedImage {
	seen := map[string]bool{}
	for _, raw := range candidates {
		if ctx.Err() != nil {
			break
		}
		if seen[raw] {
			continue
		}
		seen[raw] = true
		u, err := url.Parse(raw)
		if err != nil {
			continue
		}
		data, _, err := m.get(ctx, u, limit)
		if err != nil {
			continue
		}
		contentType := imageContentType(data, allowSVG)
		if contentType != "" {
			return cachedImage{data: data, contentType: contentType}
		}
	}
	return cachedImage{}
}
func imageContentType(data []byte, allowSVG bool) string {
	if len(data) == 0 {
		return ""
	}
	kind := http.DetectContentType(data)
	switch kind {
	case "image/png", "image/jpeg", "image/gif", "image/webp", "image/x-icon":
		return kind
	}
	if allowSVG {
		decoder := xml.NewDecoder(bytes.NewReader(data))
		for {
			token, err := decoder.Token()
			if err != nil {
				break
			}
			if start, ok := token.(xml.StartElement); ok {
				if start.Name.Local == "svg" && (start.Name.Space == "" || start.Name.Space == "http://www.w3.org/2000/svg") {
					return "image/svg+xml"
				}
				break
			}
		}
	}
	return ""
}
