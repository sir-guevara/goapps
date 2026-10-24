package main

import (
	"database/sql"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

func TestResetPassword(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	_, err = db.Exec(`CREATE TABLE admins(id INTEGER PRIMARY KEY,username TEXT,password_hash BLOB);
CREATE TABLE sessions(admin_id INTEGER);
CREATE TABLE bookmarks(title TEXT);
INSERT INTO admins VALUES(1,'owner','old hash');
INSERT INTO sessions VALUES(1);
INSERT INTO bookmarks VALUES('saved site');`)
	if err != nil {
		t.Fatal(err)
	}
	username, password, err := resetPassword(db)
	if err != nil {
		t.Fatal(err)
	}
	var hash []byte
	if err := db.QueryRow("SELECT password_hash FROM admins WHERE id=1").Scan(&hash); err != nil {
		t.Fatal(err)
	}
	if username != "owner" || len(password) != 48 || bcrypt.CompareHashAndPassword(hash, []byte(password)) != nil {
		t.Fatal("generated credentials do not match saved admin")
	}
	var sessions, bookmarks int
	if err := db.QueryRow("SELECT count(*) FROM sessions").Scan(&sessions); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT count(*) FROM bookmarks").Scan(&bookmarks); err != nil {
		t.Fatal(err)
	}
	if sessions != 0 || bookmarks != 1 {
		t.Fatal("reset must revoke sessions and preserve bookmarks")
	}
}
