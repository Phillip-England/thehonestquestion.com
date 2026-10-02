package main

import (
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"html/template"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	_ "modernc.org/sqlite"
)

type postStore struct{ db *sql.DB }

var markdown = goldmark.New(goldmark.WithExtensions(extension.GFM))
var slugCharacters = regexp.MustCompile(`[^a-z0-9]+`)

func openPostStore(path string) (*postStore, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	// SQLite contains private visitor data and authentication metadata.
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := file.Chmod(0600); err != nil {
		file.Close()
		return nil, err
	}
	if err := file.Close(); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err = db.Exec(`PRAGMA secure_delete=ON`); err != nil {
		db.Close()
		return nil, err
	}
	if _, err = db.Exec(`CREATE TABLE IF NOT EXISTS posts (
  slug TEXT PRIMARY KEY, title TEXT NOT NULL, tags TEXT NOT NULL,
  summary TEXT NOT NULL, markdown TEXT NOT NULL, read_time TEXT NOT NULL, image TEXT NOT NULL DEFAULT '', published INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL, updated_at TEXT NOT NULL
 ); CREATE TABLE IF NOT EXISTS sessions (
  token_hash TEXT PRIMARY KEY, csrf_token TEXT NOT NULL, expires_at TEXT NOT NULL
 ); CREATE TABLE IF NOT EXISTS login_failures (
  ip TEXT NOT NULL, attempted_at INTEGER NOT NULL
 ); CREATE INDEX IF NOT EXISTS login_failures_ip_attempted_at ON login_failures(ip, attempted_at
 ); CREATE TABLE IF NOT EXISTS contact_messages (
  id INTEGER PRIMARY KEY, created_at INTEGER NOT NULL,
  name TEXT NOT NULL, email TEXT NOT NULL, message TEXT NOT NULL
 ); CREATE INDEX IF NOT EXISTS contact_messages_created_at ON contact_messages(created_at DESC, id DESC
 ); CREATE TABLE IF NOT EXISTS contact_submissions (
  ip TEXT NOT NULL, attempted_at INTEGER NOT NULL
 ); CREATE INDEX IF NOT EXISTS contact_submissions_ip_attempted_at ON contact_submissions(ip, attempted_at
 ); CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);`); err != nil {
		db.Close()
		return nil, err
	}
	s := &postStore{db: db}
	if _, err := db.Exec(`DELETE FROM sessions WHERE expires_at < ?`, time.Now().UTC().Add(-time.Second).Format(time.RFC3339Nano)); err != nil {
		db.Close()
		return nil, err
	}
	if err := s.migrateCategory(); err != nil {
		db.Close()
		return nil, err
	}
	if err := s.migrateImage(); err != nil {
		db.Close()
		return nil, err
	}
	if err := s.seed(); err != nil {
		db.Close()
		return nil, err
	}
	if err := s.normalizeLegacyTags(); err != nil {
		db.Close()
		return nil, err
	}
	if err := s.migrateLegacyQuestions(filepath.Join(filepath.Dir(path), "questions.jsonl")); err != nil {
		db.Close()
		return nil, err
	}
	if err := s.pruneContacts(time.Now().UTC()); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}
func (s *postStore) close() error { return s.db.Close() }

func (s *postStore) migrateCategory() error {
	rows, err := s.db.Query(`PRAGMA table_info(posts)`)
	if err != nil {
		return err
	}
	hasCategory, hasTags := false, false
	for rows.Next() {
		var id, notNull, primaryKey int
		var name, kind string
		var defaultValue sql.NullString
		if err := rows.Scan(&id, &name, &kind, &notNull, &defaultValue, &primaryKey); err != nil {
			rows.Close()
			return err
		}
		if name == "category" {
			hasCategory = true
		}
		if name == "tags" {
			hasTags = true
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	if hasCategory && !hasTags {
		return s.renameCategoryColumn()
	}
	return nil
}

func (s *postStore) renameCategoryColumn() error {
	_, err := s.db.Exec(`ALTER TABLE posts RENAME COLUMN category TO tags`)
	return err
}

func (s *postStore) migrateImage() error {
	rows, err := s.db.Query(`PRAGMA table_info(posts)`)
	if err != nil {
		return err
	}
	hasImage := false
	for rows.Next() {
		var id, notNull, primaryKey int
		var name, kind string
		var defaultValue sql.NullString
		if err := rows.Scan(&id, &name, &kind, &notNull, &defaultValue, &primaryKey); err != nil {
			rows.Close()
			return err
		}
		if name == "image" {
			hasImage = true
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	if hasImage {
		return nil
	}
	_, err = s.db.Exec(`ALTER TABLE posts ADD COLUMN image TEXT NOT NULL DEFAULT ''`)
	return err
}

func (s *postStore) normalizeLegacyTags() error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var value string
	err = tx.QueryRow(`SELECT value FROM meta WHERE key='tags_v1'`).Scan(&value)
	if err == nil {
		return tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	rows, err := tx.Query(`SELECT slug,tags FROM posts`)
	if err != nil {
		return err
	}
	type item struct{ slug, tags string }
	items := []item{}
	for rows.Next() {
		var x item
		if err := rows.Scan(&x.slug, &x.tags); err != nil {
			rows.Close()
			return err
		}
		items = append(items, x)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, x := range items {
		tags, ok := normalizeTags(x.tags)
		if !ok {
			legacy := strings.Trim(slugCharacters.ReplaceAllString(strings.ToLower(x.tags), "_"), "_")
			if legacy == "" {
				legacy = "reflections"
			}
			tags = "#" + legacy
		}
		if _, err := tx.Exec(`UPDATE posts SET tags=? WHERE slug=?`, tags, x.slug); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`INSERT INTO meta VALUES ('tags_v1','1')`); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *postStore) seed() error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var value string
	err = tx.QueryRow(`SELECT value FROM meta WHERE key='seeded'`).Scan(&value)
	if err == nil {
		return tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	now := time.Now().UTC()
	for i, a := range articles {
		var body strings.Builder
		for _, sec := range a.Sections {
			body.WriteString("## " + sec.Heading + "\n\n")
			for _, p := range sec.Body {
				body.WriteString(p + "\n\n")
			}
		}
		stamp := now.Add(time.Duration(-i) * time.Minute).Format(time.RFC3339Nano)
		_, err = tx.Exec(`INSERT OR IGNORE INTO posts (slug,title,tags,summary,markdown,read_time,published,created_at,updated_at) VALUES (?,?,?,?,?,?,?,?,?)`, a.Slug, a.Title, a.Tags, a.Summary, body.String(), a.ReadTime, 1, stamp, stamp)
		if err != nil {
			return err
		}
	}
	if _, err = tx.Exec(`INSERT INTO meta VALUES ('seeded','1')`); err != nil {
		return err
	}
	return tx.Commit()
}
func renderMarkdown(input string) (template.HTML, error) {
	var out bytes.Buffer
	if err := markdown.Convert([]byte(input), &out); err != nil {
		return "", err
	}
	return template.HTML(out.String()), nil
}
func (s *postStore) list(publishedOnly bool) ([]article, error) {
	query := `SELECT slug,title,tags,summary,markdown,read_time,image,published FROM posts`
	if publishedOnly {
		query += ` WHERE published=1`
	}
	query += ` ORDER BY created_at DESC`
	rows, err := s.db.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	posts := []article{}
	for rows.Next() {
		var a article
		var published int
		if err := rows.Scan(&a.Slug, &a.Title, &a.Tags, &a.Summary, &a.Markdown, &a.ReadTime, &a.Image, &published); err != nil {
			return nil, err
		}
		a.Published = published == 1
		a.TagList = strings.Fields(a.Tags)
		a.Body, err = renderMarkdown(a.Markdown)
		if err != nil {
			return nil, err
		}
		posts = append(posts, a)
	}
	return posts, rows.Err()
}
func (s *postStore) get(slug string) (article, error) {
	var a article
	var published int
	err := s.db.QueryRow(`SELECT slug,title,tags,summary,markdown,read_time,image,published FROM posts WHERE slug=?`, slug).Scan(&a.Slug, &a.Title, &a.Tags, &a.Summary, &a.Markdown, &a.ReadTime, &a.Image, &published)
	if err != nil {
		return a, err
	}
	a.Published = published == 1
	a.TagList = strings.Fields(a.Tags)
	a.Body, err = renderMarkdown(a.Markdown)
	return a, err
}
func (s *postStore) save(a article, existingSlug string) (string, error) {
	if existingSlug != "" {
		_, err := s.db.Exec(`UPDATE posts SET title=?,tags=?,summary=?,markdown=?,read_time=?,image=?,published=?,updated_at=? WHERE slug=?`, a.Title, a.Tags, a.Summary, a.Markdown, readTime(a.Markdown), a.Image, a.Published, time.Now().UTC().Format(time.RFC3339Nano), existingSlug)
		return existingSlug, err
	}
	base := slugify(a.Title)
	stamp := time.Now().UTC().Format(time.RFC3339Nano)
	for n := 1; n <= 1000; n++ {
		slug := base
		if n > 1 {
			slug = fmt.Sprintf("%s-%d", base, n)
		}
		_, err := s.db.Exec(`INSERT INTO posts (slug,title,tags,summary,markdown,read_time,image,published,created_at,updated_at) VALUES (?,?,?,?,?,?,?,?,?,?)`, slug, a.Title, a.Tags, a.Summary, a.Markdown, readTime(a.Markdown), a.Image, a.Published, stamp, stamp)
		if err == nil {
			return slug, nil
		}
		var count int
		if queryErr := s.db.QueryRow(`SELECT COUNT(*) FROM posts WHERE slug=?`, slug).Scan(&count); queryErr != nil || count == 0 {
			return "", err
		}
	}
	return "", errors.New("could not find a free slug")
}

func (s *postStore) delete(slug string) (bool, error) {
	result, err := s.db.Exec(`DELETE FROM posts WHERE slug=?`, slug)
	if err != nil {
		return false, err
	}
	count, err := result.RowsAffected()
	return count > 0, err
}

func normalizeTags(input string) (string, bool) {
	parts := strings.Fields(strings.ReplaceAll(input, ",", " "))
	if len(parts) == 0 || len(parts) > 10 {
		return "", false
	}
	seen := map[string]bool{}
	tags := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.ToLower(part)
		runes := []rune(part)
		if len(runes) < 2 || len(runes) > 31 || runes[0] != '#' {
			return "", false
		}
		for _, r := range runes[1:] {
			if !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' {
				return "", false
			}
		}
		if !seen[part] {
			seen[part] = true
			tags = append(tags, part)
		}
	}
	return strings.Join(tags, " "), true
}

func hasTag(a article, tag string) bool {
	for _, candidate := range a.TagList {
		if candidate == tag {
			return true
		}
	}
	return false
}
func slugify(title string) string {
	slug := strings.Trim(slugCharacters.ReplaceAllString(strings.ToLower(title), "-"), "-")
	if len(slug) > 70 {
		slug = strings.TrimRight(slug[:70], "-")
	}
	if slug == "" {
		slug = "post"
	}
	return slug
}
func readTime(body string) string {
	minutes := (len(strings.Fields(body)) + 199) / 200
	if minutes < 1 {
		minutes = 1
	}
	return fmt.Sprintf("%d min read", minutes)
}
