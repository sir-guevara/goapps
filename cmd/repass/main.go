package main

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"flag"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"

	"golang.org/x/crypto/bcrypt"
	_ "modernc.org/sqlite"
)

func resetPassword(db *sql.DB) (string, string, error) {
	var secret [24]byte
	if _, err := rand.Read(secret[:]); err != nil {
		return "", "", err
	}
	password := hex.EncodeToString(secret[:])
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", "", err
	}
	tx, err := db.Begin()
	if err != nil {
		return "", "", err
	}
	defer tx.Rollback()
	var username string
	if err := tx.QueryRow("SELECT username FROM admins WHERE id=1").Scan(&username); err != nil {
		return "", "", fmt.Errorf("read existing admin: %w", err)
	}
	if _, err := tx.Exec("UPDATE admins SET password_hash=? WHERE id=1", hash); err != nil {
		return "", "", err
	}
	if _, err := tx.Exec("DELETE FROM sessions WHERE admin_id=1"); err != nil {
		return "", "", err
	}
	if err := tx.Commit(); err != nil {
		return "", "", err
	}
	return username, password, nil
}

func main() {
	databasePath := flag.String("db", filepath.Join("data", "commonplace.db"), "path to the running app's existing SQLite database")
	flag.Parse()
	path, err := filepath.Abs(*databasePath)
	if err != nil {
		log.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		log.Fatalf("cannot access database %q: %v; run from the app's working directory or use -db /path/to/data/commonplace.db", path, err)
	}
	if !info.Mode().IsRegular() {
		log.Fatalf("database %q is not a regular file", path)
	}
	dsn := (&url.URL{Scheme: "file", Path: path}).String() + "?mode=rw&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()
	if err := db.Ping(); err != nil {
		log.Fatalf("cannot open database %q: %v; check that your user can write to both the database and its parent directory", path, err)
	}
	username, password, err := resetPassword(db)
	if err != nil {
		log.Fatalf("reset admin password: %v", err)
	}
	fmt.Printf("Username: %s\nPassword: %s\nExisting admin sessions have been signed out. Save the new password.\n", username, password)
}
