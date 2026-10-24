package main

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/crypto/bcrypt"
	_ "modernc.org/sqlite"
)

type bookmarkStore struct{ db *sql.DB }

func openStore(path, legacyPath string) (*bookmarkStore, error) {
	absolutePath, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	path = absolutePath
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := file.Close(); err != nil {
		return nil, err
	}
	dsn := (&url.URL{Scheme: "file", Path: path}).String() + "?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &bookmarkStore{db: db}
	if err := s.initialize(legacyPath); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *bookmarkStore) initialize(legacyPath string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.Exec(`
 CREATE TABLE IF NOT EXISTS admins (id INTEGER PRIMARY KEY CHECK(id = 1), username TEXT NOT NULL UNIQUE, password_hash BLOB NOT NULL);
 CREATE TABLE IF NOT EXISTS sessions (token_hash TEXT PRIMARY KEY, admin_id INTEGER NOT NULL REFERENCES admins(id), expires_at INTEGER NOT NULL);
 CREATE TABLE IF NOT EXISTS bookmarks (id TEXT PRIMARY KEY, title TEXT NOT NULL, url TEXT NOT NULL, description TEXT NOT NULL, category TEXT NOT NULL, clicks INTEGER NOT NULL DEFAULT 0);
 CREATE TABLE IF NOT EXISTS bookmark_media (
  bookmark_id TEXT PRIMARY KEY REFERENCES bookmarks(id) ON DELETE CASCADE,
  source_url TEXT NOT NULL, checked_at INTEGER NOT NULL,
  icon BLOB, icon_type TEXT NOT NULL DEFAULT '',
  preview BLOB, preview_type TEXT NOT NULL DEFAULT ''
 );
 CREATE TRIGGER IF NOT EXISTS clear_bookmark_media AFTER UPDATE OF url ON bookmarks
 WHEN OLD.url != NEW.url BEGIN DELETE FROM bookmark_media WHERE bookmark_id=NEW.id; END;
 CREATE TABLE IF NOT EXISTS metadata (key TEXT PRIMARY KEY, value TEXT NOT NULL);`)
	if err != nil {
		return err
	}
	var initialized string
	err = tx.QueryRow("SELECT value FROM metadata WHERE key = 'initialized'").Scan(&initialized)
	if errors.Is(err, sql.ErrNoRows) {
		items := starterBookmarks()
		if legacyPath != "" {
			data, readErr := os.ReadFile(legacyPath)
			if readErr == nil {
				if err := json.Unmarshal(data, &items); err != nil {
					return fmt.Errorf("import existing bookmarks: %w", err)
				}
			} else if !errors.Is(readErr, os.ErrNotExist) {
				return readErr
			}
		}
		for _, b := range items {
			if _, err := tx.Exec("INSERT INTO bookmarks(id,title,url,description,category,clicks) VALUES(?,?,?,?,?,?)", b.ID, b.Title, b.URL, b.Description, b.Category, b.Clicks); err != nil {
				return err
			}
		}
		if _, err := tx.Exec("INSERT INTO metadata(key,value) VALUES('initialized','1')"); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	return tx.Commit()
}

// Credentials are created once. Restarts use the existing SQLite admin account.
func (s *bookmarkStore) bootstrapAdmin() error {
	var count int
	if err := s.db.QueryRow("SELECT count(*) FROM admins").Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		return nil
	}
	username := os.Getenv("ADMIN_USERNAME")
	if username == "" {
		username = "admin"
	}
	password := os.Getenv("ADMIN_PASSWORD")
	generated := password == ""
	if generated {
		var secret [24]byte
		if _, err := rand.Read(secret[:]); err != nil {
			return err
		}
		password = hex.EncodeToString(secret[:])
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return fmt.Errorf("hash admin password (maximum 72 bytes): %w", err)
	}
	if _, err := s.db.Exec("INSERT INTO admins(id,username,password_hash) VALUES(1,?,?)", username, hash); err != nil {
		return err
	}
	if generated {
		log.Printf("Created persistent admin %q; save this initial password: %s", username, password)
	}
	return nil
}

func (s *bookmarkStore) authenticate(username, password string) bool {
	var storedUsername string
	var hash []byte
	if err := s.db.QueryRow("SELECT username,password_hash FROM admins WHERE id=1").Scan(&storedUsername, &hash); err != nil {
		return false
	}
	err := bcrypt.CompareHashAndPassword(hash, []byte(password))
	return err == nil && username == storedUsername
}

func (s *bookmarkStore) list() ([]Bookmark, error) {
	rows, err := s.db.Query(`SELECT b.id,b.title,b.url,b.description,b.category,b.clicks,
 COALESCE(length(m.icon),0),COALESCE(length(m.preview),0),m.bookmark_id IS NULL
 FROM bookmarks b LEFT JOIN bookmark_media m ON m.bookmark_id=b.id AND m.source_url=b.url ORDER BY b.rowid`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []Bookmark{}
	for rows.Next() {
		var b Bookmark
		var iconSize, previewSize int
		if err := rows.Scan(&b.ID, &b.Title, &b.URL, &b.Description, &b.Category, &b.Clicks, &iconSize, &previewSize, &b.MediaPending); err != nil {
			return nil, err
		}
		if iconSize > 0 {
			b.IconURL = "/api/bookmarks/" + url.PathEscape(b.ID) + "/icon"
		}
		if previewSize > 0 {
			b.PreviewURL = "/api/bookmarks/" + url.PathEscape(b.ID) + "/preview"
		}
		items = append(items, b)
	}
	return items, rows.Err()
}
func (s *bookmarkStore) create(b Bookmark) error {
	_, err := s.db.Exec("INSERT INTO bookmarks(id,title,url,description,category,clicks) VALUES(?,?,?,?,?,0)", b.ID, b.Title, b.URL, b.Description, b.Category)
	return err
}
func changed(result sql.Result, err error) (bool, error) {
	if err != nil {
		return false, err
	}
	count, err := result.RowsAffected()
	return count > 0, err
}
func (s *bookmarkStore) update(id string, b Bookmark) (bool, error) {
	return changed(s.db.Exec("UPDATE bookmarks SET title=?,url=?,description=?,category=? WHERE id=?", b.Title, b.URL, b.Description, b.Category, id))
}
func (s *bookmarkStore) delete(id string) (bool, error) {
	return changed(s.db.Exec("DELETE FROM bookmarks WHERE id=?", id))
}
func (s *bookmarkStore) recordClick(id string) error {
	ok, err := changed(s.db.Exec("UPDATE bookmarks SET clicks=clicks+1 WHERE id=?", id))
	if err != nil {
		return err
	}
	if !ok {
		return os.ErrNotExist
	}
	return nil
}

type sessionManager struct{ db *sql.DB }

func tokenHash(token string) string {
	hash := sha256.Sum256([]byte(token))
	return hex.EncodeToString(hash[:])
}
func (s *sessionManager) create() (string, error) {
	var secret [32]byte
	if _, err := rand.Read(secret[:]); err != nil {
		return "", err
	}
	token := hex.EncodeToString(secret[:])
	if _, err := s.db.Exec("DELETE FROM sessions WHERE expires_at <= ?", time.Now().Unix()); err != nil {
		return "", err
	}
	_, err := s.db.Exec("INSERT INTO sessions(token_hash,admin_id,expires_at) VALUES(?,1,?)", tokenHash(token), time.Now().Add(12*time.Hour).Unix())
	return token, err
}
func (s *sessionManager) valid(token string) bool {
	if len(token) != 64 {
		return false
	}
	var exists int
	err := s.db.QueryRow("SELECT 1 FROM sessions WHERE token_hash=? AND expires_at>?", tokenHash(token), time.Now().Unix()).Scan(&exists)
	return err == nil
}
func (s *sessionManager) remove(token string) error {
	_, err := s.db.Exec("DELETE FROM sessions WHERE token_hash=?", tokenHash(token))
	return err
}
