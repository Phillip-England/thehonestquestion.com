package main

import (
	"bytes"
	"context"
	"database/sql"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDatabaseUpload(t *testing.T) {
	dir := t.TempDir()
	s, err := newServer(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.store.close()
	localDir := t.TempDir()
	local, err := openPostStore(filepath.Join(localDir, "main.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = local.save(article{Title: "Local published article", Tags: "#faith", Summary: "Local summary", Markdown: "Local article body", Published: true}, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = local.db.Exec(`DELETE FROM posts WHERE slug <> 'local-published-article'`); err != nil {
		t.Fatal(err)
	}
	local.close()
	s.sessionSecret = []byte("test-session-secret-at-least-32-characters")
	token, _ := randomToken()
	if _, err = s.store.db.Exec(`INSERT INTO sessions VALUES (?,?,?)`, tokenHash(token), "csrf", time.Now().Add(time.Hour).Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	upload := func(data []byte, csrf string, auth bool) *httptest.ResponseRecorder {
		var body bytes.Buffer
		writer := multipart.NewWriter(&body)
		writer.WriteField("csrf", csrf)
		writer.WriteField("confirm", "1")
		part, _ := writer.CreateFormFile("database", "main.sqlite")
		part.Write(data)
		writer.Close()
		r := httptest.NewRequest("POST", "/admin/database", &body)
		r.Header.Set("Content-Type", writer.FormDataContentType())
		if auth {
			r.AddCookie(&http.Cookie{Name: "admin_session", Value: s.signedSession(token)})
		}
		w := httptest.NewRecorder()
		s.routes().ServeHTTP(w, r)
		return w
	}
	data, err := os.ReadFile(filepath.Join(localDir, "main.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if w := upload(data, "csrf", false); w.Code != 303 {
		t.Fatalf("unauthorized: %d", w.Code)
	}
	if w := upload(data, "wrong", true); w.Code != 403 {
		t.Fatalf("csrf: %d", w.Code)
	}
	if w := upload([]byte("not SQLite"), "csrf", true); w.Code != 400 {
		t.Fatalf("invalid file: %d", w.Code)
	}
	if w := upload(data, "csrf", true); w.Code != 303 || w.Header().Get("Location") != "/admin/database?replaced=1" {
		t.Fatalf("upload: %d %s", w.Code, w.Body.String())
	}
	w := httptest.NewRecorder()
	s.routes().ServeHTTP(w, httptest.NewRequest("GET", "/articles/local-published-article", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Local article body") {
		t.Fatal("uploaded article is not live")
	}
	if _, err = s.store.get("when-faith-feels-fragile"); err != sql.ErrNoRows {
		t.Fatal("old post remained")
	}
	backups, _ := filepath.Glob(filepath.Join(dir, "backups", "*.sqlite"))
	if len(backups) != 1 {
		t.Fatal("missing backup")
	}
	backup, err := sql.Open("sqlite", backups[0])
	if err != nil {
		t.Fatal(err)
	}
	defer backup.Close()
	var count int
	if err = backup.QueryRow(`SELECT count(*) FROM posts WHERE slug='when-faith-feels-fragile'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("backup missing old content: %v", err)
	}
	if w := upload(data, "csrf", true); w.Code != 303 {
		t.Fatal("session not retained")
	}
}

func TestDatabaseRejectsSchemaChanges(t *testing.T) {
	for _, ddl := range []string{`ALTER TABLE posts ADD COLUMN surprise TEXT`, `CREATE TRIGGER unwanted AFTER INSERT ON posts BEGIN DELETE FROM posts; END`, `DROP INDEX contact_messages_created_at`, `ALTER TABLE posts RENAME COLUMN markdown TO body`} {
		t.Run(ddl, func(t *testing.T) {
			dir := t.TempDir()
			live, err := openPostStore(filepath.Join(dir, "main.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			defer live.close()
			path := filepath.Join(t.TempDir(), "incoming.sqlite")
			incoming, err := openPostStore(path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = incoming.db.Exec(ddl); err != nil {
				t.Fatal(err)
			}
			incoming.close()
			if err = live.replaceDatabase(context.Background(), path, dir); err != errDatabaseSchema {
				t.Fatalf("schema accepted: %v", err)
			}
			if _, err = live.get("when-faith-feels-fragile"); err != nil {
				t.Fatal("live database changed")
			}
		})
	}
}
