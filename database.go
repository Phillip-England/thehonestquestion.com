package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const databaseUploadLimit = 100 << 20

var errDatabaseSchema = errors.New("The uploaded database does not match the current schema. Use a database from the same version of this site.")

func (s *server) databasePage(w http.ResponseWriter, r *http.Request) {
	csrf, ok := s.requireAdmin(w, r, false)
	if !ok {
		return
	}
	s.renderAdmin(w, "database", adminPageData{Title: "Database", CSRF: csrf, Success: r.URL.Query().Get("replaced") == "1"})
}

func (s *server) uploadDatabase(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r, false); !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, databaseUploadLimit+(1<<20))
	err := r.ParseMultipartForm(1 << 20)
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll()
	}
	if err != nil {
		http.Error(w, "Could not read upload. Maximum database size is 100 MB.", http.StatusBadRequest)
		return
	}
	csrf, ok := s.requireAdmin(w, r, true)
	if !ok {
		return
	}
	fail := func(message string) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Frame-Options", "DENY")
		w.WriteHeader(http.StatusBadRequest)
		s.renderAdmin(w, "database", adminPageData{Title: "Database", CSRF: csrf, Error: message})
	}
	if r.PostFormValue("confirm") != "1" {
		fail("Confirm that you want to replace the current database contents.")
		return
	}
	file, _, err := r.FormFile("database")
	if err != nil {
		fail("Choose a SQLite database to upload.")
		return
	}
	defer file.Close()
	temp, err := os.CreateTemp(s.dataDir, "database-upload-*.sqlite")
	if err != nil {
		http.Error(w, "Upload unavailable", 500)
		return
	}
	defer os.Remove(temp.Name())
	n, err := io.Copy(temp, io.LimitReader(file, databaseUploadLimit+1))
	closeErr := temp.Close()
	if err != nil || closeErr != nil || n > databaseUploadLimit {
		fail("Could not read database, or database exceeds 100 MB.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	if err := s.store.replaceDatabase(ctx, temp.Name(), s.dataDir); err != nil {
		log.Printf("replace database: %v", err)
		if errors.Is(err, errDatabaseSchema) {
			fail(errDatabaseSchema.Error())
		} else {
			fail("The database could not be validated or replaced. The current database has been kept.")
		}
		return
	}
	http.Redirect(w, r, "/admin/database?replaced=1", http.StatusSeeOther)
}

// Comparing every schema object also rejects uploaded triggers and views. Ignore
// whitespace formatting, but require identical tables, constraints, and indexes.
func databaseSchema(ctx context.Context, conn *sql.Conn, name string) (map[string]string, error) {
	rows, err := conn.QueryContext(ctx, `SELECT type,name,sql FROM `+name+`.sqlite_schema WHERE name NOT LIKE 'sqlite_%' ORDER BY type,name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := map[string]string{}
	for rows.Next() {
		var kind, name, ddl string
		if err := rows.Scan(&kind, &name, &ddl); err != nil {
			return nil, err
		}
		result[kind+":"+name] = strings.Join(strings.Fields(ddl), " ")
	}
	return result, rows.Err()
}

func (s *postStore) replaceDatabase(ctx context.Context, path, dir string) error {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	uri := (&url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro"}).String()
	if _, err = conn.ExecContext(ctx, `ATTACH DATABASE ? AS uploaded`, uri); err != nil {
		return err
	}
	defer conn.ExecContext(context.Background(), `DETACH DATABASE uploaded`)
	var integrity string
	if err = conn.QueryRowContext(ctx, `PRAGMA uploaded.integrity_check`).Scan(&integrity); err != nil {
		return err
	}
	if integrity != "ok" {
		return fmt.Errorf("database integrity: %s", integrity)
	}
	current, err := databaseSchema(ctx, conn, "main")
	if err != nil {
		return err
	}
	incoming, err := databaseSchema(ctx, conn, "uploaded")
	if err != nil {
		return err
	}
	if len(current) != len(incoming) {
		return errDatabaseSchema
	}
	for key, value := range current {
		if incoming[key] != value {
			return errDatabaseSchema
		}
	}
	backupDir := filepath.Join(dir, "backups")
	if err = os.MkdirAll(backupDir, 0700); err != nil {
		return err
	}
	backup, err := os.CreateTemp(backupDir, "before-replacement-*.sqlite")
	if err != nil {
		return err
	}
	backupPath := backup.Name()
	if err = backup.Close(); err != nil {
		return err
	}
	// VACUUM INTO accepts an existing empty file and captures committed WAL data.
	if _, err = conn.ExecContext(ctx, `VACUUM main INTO ?`, backupPath); err != nil {
		os.Remove(backupPath)
		return err
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Retain production sessions and login limits; never import local credentials.
	for _, table := range []string{"posts", "contact_messages", "contact_submissions", "meta"} {
		if _, err = tx.ExecContext(ctx, `DELETE FROM main.`+table); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO main.`+table+` SELECT * FROM uploaded.`+table); err != nil {
			return err
		}
	}
	return tx.Commit()
}
