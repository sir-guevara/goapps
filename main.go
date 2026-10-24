package main

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

//go:embed web/*
var webFiles embed.FS

type Bookmark struct {
	ID           string `json:"id"`
	Title        string `json:"title"`
	URL          string `json:"url"`
	Description  string `json:"description"`
	Category     string `json:"category"`
	Clicks       int    `json:"clicks"`
	IconURL      string `json:"icon_url,omitempty"`
	PreviewURL   string `json:"preview_url,omitempty"`
	MediaPending bool   `json:"media_pending"`
}

type app struct {
	store    *bookmarkStore
	sessions *sessionManager
	media    *mediaWorker
}

func main() {
	store, err := openStore(filepath.Join("data", "commonplace.db"), filepath.Join("data", "bookmarks.json"))
	if err != nil {
		log.Fatalf("initialize database: %v", err)
	}
	defer store.db.Close()
	if err := store.bootstrapAdmin(); err != nil {
		log.Fatalf("initialize admin: %v", err)
	}

	staticFiles, err := fs.Sub(webFiles, "web")
	if err != nil {
		log.Fatalf("load web files: %v", err)
	}
	application := &app{store: store, sessions: &sessionManager{db: store.db}, media: newMediaWorker(store)}
	mediaContext, cancelMedia := context.WithCancel(context.Background())
	defer cancelMedia()
	go application.media.run(mediaContext)

	mux := http.NewServeMux()
	mux.HandleFunc("/api/session", application.session)
	mux.HandleFunc("/api/login", application.login)
	mux.HandleFunc("/api/logout", application.logout)
	mux.HandleFunc("/api/bookmarks", application.bookmarks)
	mux.HandleFunc("/api/bookmarks/", application.bookmarkAction)
	mux.Handle("/", http.FileServer(http.FS(staticFiles)))

	address := os.Getenv("ADDR")
	if address == "" {
		address = ":5001"
	}
	server := &http.Server{
		Addr:              address,
		Handler:           securityHeaders(mux),
		ReadHeaderTimeout: 5 * time.Second,
	}
	log.Printf("Bookmark launcher listening on %s", address)
	log.Fatal(server.ListenAndServe())
}

func (a *app) isAdmin(r *http.Request) bool {
	cookie, err := r.Cookie("bookmark_session")
	return err == nil && a.sessions.valid(cookie.Value)
}

func (a *app) session(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"admin": a.isAdmin(r)})
}

func (a *app) login(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	var credentials struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := decodeJSON(w, r, &credentials); err != nil {
		writeError(w, http.StatusBadRequest, "Please enter a valid username and password.")
		return
	}
	if !a.store.authenticate(credentials.Username, credentials.Password) {
		writeError(w, http.StatusUnauthorized, "That username or password is not correct.")
		return
	}

	token, err := a.sessions.create()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Could not start your session.")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     "bookmark_session",
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   r.TLS != nil,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   12 * 60 * 60,
	})
	writeJSON(w, http.StatusOK, map[string]bool{"admin": true})
}

func (a *app) logout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	if cookie, err := r.Cookie("bookmark_session"); err == nil {
		if err := a.sessions.remove(cookie.Value); err != nil {
			writeError(w, http.StatusInternalServerError, "Could not sign out. Please try again.")
			return
		}
	}
	http.SetCookie(w, &http.Cookie{
		Name:     "bookmark_session",
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   r.TLS != nil,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
	writeJSON(w, http.StatusOK, map[string]bool{"admin": false})
}

func (a *app) bookmarks(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		items, err := a.store.list()
		if err != nil {
			writeError(w, http.StatusInternalServerError, "Could not load sites.")
			return
		}
		writeJSON(w, http.StatusOK, items)
	case http.MethodPost:
		if !a.requireAdmin(w, r) {
			return
		}
		bookmark, ok := readBookmark(w, r)
		if !ok {
			return
		}
		id, err := newID()
		if err != nil {
			writeError(w, http.StatusInternalServerError, "Could not create this bookmark.")
			return
		}
		bookmark.ID = id
		if err := a.store.create(bookmark); err != nil {
			writeError(w, http.StatusInternalServerError, "Could not save this bookmark.")
			return
		}
		if a.media != nil {
			a.media.notify()
		}
		bookmark.MediaPending = true
		writeJSON(w, http.StatusCreated, bookmark)
	default:
		methodNotAllowed(w)
	}
}

func (a *app) bookmarkAction(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/bookmarks/"), "/"), "/")
	if len(parts) == 2 && (parts[1] == "icon" || parts[1] == "preview") {
		a.serveMedia(w, r, parts[0], parts[1])
		return
	}
	if len(parts) == 2 && parts[1] == "click" && r.Method == http.MethodPost {
		if err := a.store.recordClick(parts[0]); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				writeError(w, http.StatusNotFound, "Bookmark not found.")
				return
			}
			writeError(w, http.StatusInternalServerError, "Could not update the bookmark count.")
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if len(parts) != 1 || parts[0] == "" {
		http.NotFound(w, r)
		return
	}
	if !a.requireAdmin(w, r) {
		return
	}
	switch r.Method {
	case http.MethodPut:
		bookmark, ok := readBookmark(w, r)
		if !ok {
			return
		}
		updated, err := a.store.update(parts[0], bookmark)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "Could not save this bookmark.")
			return
		}
		if !updated {
			writeError(w, http.StatusNotFound, "Bookmark not found.")
			return
		}
		if a.media != nil {
			a.media.notify()
		}
		bookmark.ID = parts[0]
		writeJSON(w, http.StatusOK, bookmark)
	case http.MethodDelete:
		deleted, err := a.store.delete(parts[0])
		if err != nil {
			writeError(w, http.StatusInternalServerError, "Could not delete this bookmark.")
			return
		}
		if !deleted {
			writeError(w, http.StatusNotFound, "Bookmark not found.")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		methodNotAllowed(w)
	}
}

func (a *app) requireAdmin(w http.ResponseWriter, r *http.Request) bool {
	if a.isAdmin(r) {
		return true
	}
	writeError(w, http.StatusUnauthorized, "Please sign in as an admin to manage bookmarks.")
	return false
}

func readBookmark(w http.ResponseWriter, r *http.Request) (Bookmark, bool) {
	var bookmark Bookmark
	if err := decodeJSON(w, r, &bookmark); err != nil {
		writeError(w, http.StatusBadRequest, "Please check the bookmark details and try again.")
		return Bookmark{}, false
	}
	bookmark.IconURL = ""
	bookmark.PreviewURL = ""
	bookmark.MediaPending = false
	bookmark.Title = strings.TrimSpace(bookmark.Title)
	bookmark.URL = strings.TrimSpace(bookmark.URL)
	bookmark.Description = strings.TrimSpace(bookmark.Description)
	bookmark.Category = strings.TrimSpace(bookmark.Category)
	parsedURL, err := url.ParseRequestURI(bookmark.URL)
	if bookmark.Title == "" || len(bookmark.Title) > 80 ||
		bookmark.Description == "" || len(bookmark.Description) > 180 ||
		bookmark.Category == "" || len(bookmark.Category) > 32 ||
		err != nil || (parsedURL.Scheme != "http" && parsedURL.Scheme != "https") || parsedURL.Host == "" {
		writeError(w, http.StatusBadRequest, "Add a title, short description, category, and a valid http or https link.")
		return Bookmark{}, false
	}
	return bookmark, true
}

func decodeJSON(w http.ResponseWriter, r *http.Request, target any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	defer r.Body.Close()
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return errors.New("request must contain one JSON object")
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		log.Printf("write JSON response: %v", err)
	}
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func methodNotAllowed(w http.ResponseWriter) {
	writeError(w, http.StatusMethodNotAllowed, "Method not allowed.")
}

func newID() (string, error) {
	var bytes [12]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes[:]), nil
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
		w.Header().Set("X-Frame-Options", "DENY")
		if strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("Cache-Control", "no-store")
			if r.Method != http.MethodGet && r.Method != http.MethodHead {
				if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
					writeError(w, http.StatusForbidden, "Cross-site requests are not allowed.")
					return
				}
				if origin := r.Header.Get("Origin"); origin != "" {
					parsed, err := url.Parse(origin)
					if err != nil || parsed.Host != r.Host || (parsed.Scheme != "http" && parsed.Scheme != "https") {
						writeError(w, http.StatusForbidden, "Cross-site requests are not allowed.")
						return
					}
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

func starterBookmarks() []Bookmark {
	return []Bookmark{
		{ID: "linear", Title: "Linear", URL: "https://linear.app", Description: "The issue tracker that keeps product teams moving.", Category: "Work"},
		{ID: "figma", Title: "Figma", URL: "https://figma.com", Description: "A shared space to design, prototype, and make it real.", Category: "Design"},
		{ID: "are-na", Title: "Are.na", URL: "https://www.are.na", Description: "A quieter corner of the internet for collecting ideas.", Category: "Inspiration"},
		{ID: "read-cv", Title: "Read.cv", URL: "https://read.cv", Description: "Thoughtful profiles and work from interesting people.", Category: "Community"},
		{ID: "raycast", Title: "Raycast", URL: "https://www.raycast.com", Description: "A faster way to get things done on your Mac.", Category: "Tools"},
		{ID: "notion", Title: "Notion", URL: "https://www.notion.so", Description: "One place for notes, docs, and all the little things.", Category: "Work"},
	}
}
