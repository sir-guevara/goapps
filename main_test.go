package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func testApp(t *testing.T) *app {
	t.Helper()
	t.Setenv("ADMIN_USERNAME", "admin")
	t.Setenv("ADMIN_PASSWORD", "test-password")
	store, err := openStore(filepath.Join(t.TempDir(), "test.db"), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.db.Close() })
	if err := store.bootstrapAdmin(); err != nil {
		t.Fatal(err)
	}
	return &app{store: store, sessions: &sessionManager{db: store.db}}
}
func request(a *app, method, path, body string, cookie *http.Cookie) *httptest.ResponseRecorder {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/login", a.login)
	mux.HandleFunc("/api/logout", a.logout)
	mux.HandleFunc("/api/session", a.session)
	mux.HandleFunc("/api/bookmarks", a.bookmarks)
	mux.HandleFunc("/api/bookmarks/", a.bookmarkAction)
	r := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	if cookie != nil {
		r.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	securityHeaders(mux).ServeHTTP(w, r)
	return w
}
func signIn(t *testing.T, a *app) *http.Cookie {
	t.Helper()
	w := request(a, "POST", "/api/login", `{"username":"admin","password":"test-password"}`, nil)
	if w.Code != 200 {
		t.Fatalf("login: %d %s", w.Code, w.Body.String())
	}
	cookie := w.Result().Cookies()[0]
	if !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode {
		t.Fatal("missing cookie protections")
	}
	return cookie
}
func TestAdminOnlySiteManagement(t *testing.T) {
	a := testApp(t)
	payload := `{"title":"Example","url":"https://example.com","description":"Useful","category":"Tools"}`
	for _, tc := range []struct{ method, path string }{{"POST", "/api/bookmarks"}, {"PUT", "/api/bookmarks/linear"}, {"DELETE", "/api/bookmarks/linear"}} {
		for _, cookie := range []*http.Cookie{nil, {Name: "bookmark_session", Value: "forged-token"}} {
			if got := request(a, tc.method, tc.path, payload, cookie).Code; got != 401 {
				t.Fatalf("%s %s: %d", tc.method, tc.path, got)
			}
		}
	}
	if got := request(a, "GET", "/api/bookmarks", "", nil).Code; got != 200 {
		t.Fatalf("public list: %d", got)
	}
	if got := request(a, "POST", "/api/login", `{"username":"admin","password":"wrong"}`, nil).Code; got != 401 {
		t.Fatalf("bad login: %d", got)
	}
	cookie := signIn(t, a)
	w := request(a, "POST", "/api/bookmarks", payload, cookie)
	if w.Code != 201 {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	var b Bookmark
	if err := json.Unmarshal(w.Body.Bytes(), &b); err != nil {
		t.Fatal(err)
	}
	if got := request(a, "PUT", "/api/bookmarks/"+b.ID, payload, cookie).Code; got != 200 {
		t.Fatalf("edit: %d", got)
	}
	if got := request(a, "DELETE", "/api/bookmarks/"+b.ID, "", cookie).Code; got != 204 {
		t.Fatalf("delete: %d", got)
	}
	if got := request(a, "POST", "/api/logout", "", cookie).Code; got != 200 {
		t.Fatalf("logout: %d", got)
	}
	if got := request(a, "POST", "/api/bookmarks", payload, cookie).Code; got != 401 {
		t.Fatalf("revoked session: %d", got)
	}
}
func TestPersistentAuthAndExpiry(t *testing.T) {
	a := testApp(t)
	cookie := signIn(t, a)
	var storedHash string
	if err := a.store.db.QueryRow("SELECT token_hash FROM sessions").Scan(&storedHash); err != nil {
		t.Fatal(err)
	}
	if storedHash == cookie.Value {
		t.Fatal("raw session token stored")
	}
	var passwordHash string
	if err := a.store.db.QueryRow("SELECT password_hash FROM admins").Scan(&passwordHash); err != nil {
		t.Fatal(err)
	}
	if passwordHash == "test-password" {
		t.Fatal("raw password stored")
	}
	var dbPath string
	var seq int
	var name string
	if err := a.store.db.QueryRow("PRAGMA database_list").Scan(&seq, &name, &dbPath); err != nil {
		t.Fatal(err)
	}
	if err := a.store.db.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := openStore(dbPath, "")
	if err != nil {
		t.Fatal(err)
	}
	defer store.db.Close()
	sessions := &sessionManager{db: store.db}
	if !sessions.valid(cookie.Value) || !store.authenticate("admin", "test-password") {
		t.Fatal("auth lost after reopening")
	}
	if _, err := store.db.Exec("UPDATE sessions SET expires_at=?", time.Now().Add(-time.Second).Unix()); err != nil {
		t.Fatal(err)
	}
	if sessions.valid(cookie.Value) {
		t.Fatal("expired session authorized")
	}
}
func TestLegacyImportOnlyOnce(t *testing.T) {
	dir := t.TempDir()
	legacy := filepath.Join(dir, "bookmarks.json")
	if err := os.WriteFile(legacy, []byte(`[{"id":"saved","title":"Saved","url":"https://example.com","description":"Original","category":"Tools","clicks":9}]`), 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "app.db")
	store, err := openStore(path, legacy)
	if err != nil {
		t.Fatal(err)
	}
	items, err := store.list()
	if err != nil || len(items) != 1 || items[0].Clicks != 9 {
		t.Fatalf("migration: %+v %v", items, err)
	}
	if _, err := store.delete("saved"); err != nil {
		t.Fatal(err)
	}
	store.db.Close()
	store, err = openStore(path, legacy)
	if err != nil {
		t.Fatal(err)
	}
	defer store.db.Close()
	items, err = store.list()
	if err != nil || len(items) != 0 {
		t.Fatal("deleted sites restored on restart")
	}
}
func TestCrossSiteMutationRejected(t *testing.T) {
	a := testApp(t)
	cookie := signIn(t, a)
	r := httptest.NewRequest("DELETE", "http://example.com/api/bookmarks/linear", nil)
	r.Header.Set("Origin", "https://other.example")
	r.AddCookie(cookie)
	w := httptest.NewRecorder()
	securityHeaders(http.HandlerFunc(a.bookmarkAction)).ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatalf("cross-site mutation: %d", w.Code)
	}
}
