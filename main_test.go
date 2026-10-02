package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"image"
	_ "image/jpeg"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestPagesAndAssets(t *testing.T) {
	s, err := newServer(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		path, want string
		status     int
	}{
		{"/", "The Honest<br><em>Question.</em>", http.StatusOK},
		{"/articles/when-faith-feels-fragile", "When faith feels fragile", http.StatusOK},
		{"/public/css/site.css", ".hero", http.StatusOK},
		{"/articles/missing", "404 page not found", http.StatusNotFound},
	} {
		r := httptest.NewRequest(http.MethodGet, tc.path, nil)
		w := httptest.NewRecorder()
		s.routes().ServeHTTP(w, r)
		if w.Code != tc.status || !strings.Contains(w.Body.String(), tc.want) {
			t.Errorf("GET %s: status %d, expected %d and %q", tc.path, w.Code, tc.status, tc.want)
		}
	}
}

func TestArticleSharingMetadata(t *testing.T) {
	s, err := newServer(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.store.close()
	post := article{Title: `A "question" & answer`, Summary: `Room for <questions> & hope`, Markdown: "Hello", Image: strings.Repeat("a", 43) + ".jpg", Published: true}
	slug, err := s.store.save(post, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, withImage := range []bool{true, false} {
		if !withImage {
			post.Image = ""
			if _, err := s.store.save(post, slug); err != nil {
				t.Fatal(err)
			}
		}
		w := httptest.NewRecorder()
		s.routes().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/articles/"+slug+"?tracking=1", nil))
		body := w.Body.String()
		for _, want := range []string{
			`property="og:type" content="article"`,
			`property="og:url" content="https://thehonestquestion.com/articles/` + slug + `"`,
			`rel="canonical" href="https://thehonestquestion.com/articles/` + slug + `"`,
			`property="og:title" content="A &#34;question&#34; &amp; answer"`,
			`property="og:description" content="Room for &lt;questions&gt; &amp; hope"`,
		} {
			if w.Code != http.StatusOK || !strings.Contains(body, want) {
				t.Errorf("missing sharing metadata %q: status %d", want, w.Code)
			}
		}
		if withImage {
			for _, want := range []string{
				`name="twitter:card" content="summary_large_image"`,
				`property="og:image" content="https://thehonestquestion.com/uploads/` + post.Image + `"`,
				`name="twitter:image" content="https://thehonestquestion.com/uploads/` + post.Image + `"`,
				`property="og:image:width" content="1200"`,
				`property="og:image:height" content="675"`,
			} {
				if !strings.Contains(body, want) {
					t.Errorf("missing image metadata %q", want)
				}
			}
		} else if strings.Contains(body, `property="og:image"`) || strings.Contains(body, `name="twitter:image"`) || !strings.Contains(body, `name="twitter:card" content="summary"`) {
			t.Error("article without an image should use a summary card without image metadata")
		}
	}
}

func TestQuestionSubmission(t *testing.T) {
	dir := t.TempDir()
	s, err := newServer(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.store.close()
	form := url.Values{"name": {"Sam"}, "email": {"sam@example.com"}, "message": {"I have a question about faith"}}
	submit := func(values url.Values, remote string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/questions", strings.NewReader(values.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("X-Forwarded-For", "203.0.113.9") // Spoofed headers cannot bypass the limit.
		r.RemoteAddr = remote
		w := httptest.NewRecorder()
		s.routes().ServeHTTP(w, r)
		return w
	}
	if w := submit(form, "192.0.2.1:1000"); w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/?sent=1#ask" {
		t.Fatalf("unexpected response: %d %q", w.Code, w.Header().Get("Location"))
	}
	var name, email, message string
	var createdAt int64
	if err := s.store.db.QueryRow(`SELECT name,email,message,created_at FROM contact_messages`).Scan(&name, &email, &message, &createdAt); err != nil {
		t.Fatal(err)
	}
	if name != "Sam" || email != "sam@example.com" || message != "I have a question about faith" || createdAt == 0 {
		t.Fatalf("unexpected saved contact: %q %q %q %d", name, email, message, createdAt)
	}
	if _, err := os.Stat(filepath.Join(dir, "questions.jsonl")); !os.IsNotExist(err) {
		t.Fatal("new submission created a JSONL file")
	}
	bad := url.Values{"message": {"short"}}
	if w := submit(bad, "192.0.2.1:1000"); w.Header().Get("Location") != "/?error=invalid#ask" {
		t.Fatalf("invalid question was accepted: %d", w.Code)
	}
	trapped := url.Values{"website": {"https://spam.example"}, "message": {"I have a question about faith"}}
	if w := submit(trapped, "192.0.2.1:1000"); w.Header().Get("Location") != "/?sent=1#ask" {
		t.Fatalf("honeypot response: %d", w.Code)
	}
	var count int
	if err := s.store.db.QueryRow(`SELECT COUNT(*) FROM contact_messages`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("honeypot or invalid form was stored: count=%d err=%v", count, err)
	}
	for i := 0; i < 2; i++ {
		if w := submit(form, "192.0.2.1:2000"); w.Header().Get("Location") != "/?sent=1#ask" {
			t.Fatalf("allowed submission %d: %d", i, w.Code)
		}
	}
	if w := submit(form, "192.0.2.1:3000"); w.Header().Get("Location") != "/?error=unavailable#ask" {
		t.Fatalf("fourth submission was accepted: %d %q", w.Code, w.Header().Get("Location"))
	}
	if w := submit(form, "192.0.2.2:1000"); w.Header().Get("Location") != "/?sent=1#ask" {
		t.Fatalf("different IP was blocked: %d", w.Code)
	}
	if err := s.store.db.QueryRow(`SELECT COUNT(*) FROM contact_messages`).Scan(&count); err != nil || count != 4 {
		t.Fatalf("wrong contact count: %d %v", count, err)
	}
	if _, err := s.store.db.Exec(`UPDATE contact_submissions SET attempted_at=? WHERE ip=?`, time.Now().Add(-contactWindow-time.Minute).Unix(), "192.0.2.1"); err != nil {
		t.Fatal(err)
	}
	if w := submit(form, "192.0.2.1:4000"); w.Header().Get("Location") != "/?sent=1#ask" {
		t.Fatalf("expired submissions still blocked: %d", w.Code)
	}
	if err := s.store.db.QueryRow(`SELECT COUNT(*) FROM contact_submissions WHERE ip=?`, "192.0.2.1").Scan(&count); err != nil || count != 1 {
		t.Fatalf("old counters were not pruned: %d %v", count, err)
	}
}

func TestContactInboxAndLegacyMigration(t *testing.T) {
	dir := t.TempDir()
	legacy := question{CreatedAt: time.Now().UTC(), Name: "Sam", Email: "sam@example.com", Message: "Can I ask <script>alert(1)</script> about faith?"}
	data, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "questions.jsonl")
	if err := os.WriteFile(path, append(data, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := newServer(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("legacy JSONL file remained after import")
	}
	unauthenticated := httptest.NewRecorder()
	s.routes().ServeHTTP(unauthenticated, httptest.NewRequest(http.MethodGet, "/admin/messages", nil))
	if unauthenticated.Header().Get("Location") != "/admin/login" {
		t.Fatal("contact inbox was public")
	}
	s.sessionSecret = []byte("test-session-secret-at-least-32-characters")
	token, err := randomToken()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.db.Exec(`INSERT INTO sessions (token_hash, csrf_token, expires_at) VALUES (?,?,?)`, tokenHash(token), "test-csrf", time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/admin/messages", nil)
	req.AddCookie(&http.Cookie{Name: "admin_session", Value: s.signedSession(token)})
	inbox := httptest.NewRecorder()
	s.routes().ServeHTTP(inbox, req)
	if inbox.Code != http.StatusOK || !strings.Contains(inbox.Body.String(), "sam@example.com") || !strings.Contains(inbox.Body.String(), "Can I ask") || strings.Contains(inbox.Body.String(), "<script>") || inbox.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("private inbox did not safely render imported message: %d", inbox.Code)
	}
	s.store.close()
	s, err = newServer(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.store.close()
	var count int
	if err := s.store.db.QueryRow(`SELECT COUNT(*) FROM contact_messages`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("legacy message imported more than once: %d %v", count, err)
	}
}

func TestContactRetentionAndCap(t *testing.T) {
	s, err := newServer(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.store.close()
	now := time.Now().UTC()
	tx, err := s.store.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`INSERT INTO contact_messages (created_at,name,email,message) VALUES (?,?,?,?)`, now.Add(-contactRetention-time.Hour).Unix(), "old", "", "old note"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < contactMaxMessages+1; i++ {
		if _, err := tx.Exec(`INSERT INTO contact_messages (created_at,name,email,message) VALUES (?,?,?,?)`, now.Unix(), "current", "", "note"); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := s.store.pruneContacts(now); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := s.store.db.QueryRow(`SELECT COUNT(*) FROM contact_messages`).Scan(&count); err != nil || count != contactMaxMessages {
		t.Fatalf("contact storage was not bounded: %d %v", count, err)
	}
	if err := s.store.db.QueryRow(`SELECT COUNT(*) FROM contact_messages WHERE name='old'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("expired contact was retained: %d %v", count, err)
	}
}

func TestConcurrentContactLimit(t *testing.T) {
	s, err := newServer(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.store.close()
	q := question{CreatedAt: time.Now().UTC(), Message: "A thoughtful question about faith"}
	results := make(chan error, 12)
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- s.store.saveContact(q, "192.0.2.25")
		}()
	}
	wg.Wait()
	close(results)
	accepted := 0
	for err := range results {
		if err == nil {
			accepted++
		} else if !errors.Is(err, errContactLimit) {
			t.Fatalf("unexpected concurrent submission error: %v", err)
		}
	}
	if accepted != contactLimit {
		t.Fatalf("accepted %d concurrent submissions, expected %d", accepted, contactLimit)
	}
	var count int
	if err := s.store.db.QueryRow(`SELECT COUNT(*) FROM contact_messages`).Scan(&count); err != nil || count != contactLimit {
		t.Fatalf("stored %d concurrent messages: %v", count, err)
	}
}

func TestAdminPostWorkflow(t *testing.T) {
	s, err := newServer(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.store.close()
	s.adminUsername, s.adminPassword = "writer", "test-password"
	s.sessionSecret = []byte("test-session-secret-at-least-32-characters")
	handler := s.routes()
	get := httptest.NewRecorder()
	handler.ServeHTTP(get, httptest.NewRequest(http.MethodGet, "/admin/login", nil))
	if get.Code != http.StatusOK {
		t.Fatalf("login page: %d", get.Code)
	}
	var loginCookie *http.Cookie
	for _, c := range get.Result().Cookies() {
		if c.Name == "admin_login_csrf" {
			loginCookie = c
		}
	}
	if loginCookie == nil {
		t.Fatal("missing login CSRF cookie")
	}
	loginForm := url.Values{"username": {"writer"}, "password": {"test-password"}, "csrf": {loginCookie.Value}}
	loginReq := httptest.NewRequest(http.MethodPost, "/admin/login", strings.NewReader(loginForm.Encode()))
	loginReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	loginReq.AddCookie(loginCookie)
	login := httptest.NewRecorder()
	handler.ServeHTTP(login, loginReq)
	if login.Code != http.StatusSeeOther {
		t.Fatalf("login: %d %s", login.Code, login.Body.String())
	}
	var sessionCookie *http.Cookie
	for _, c := range login.Result().Cookies() {
		if c.Name == "admin_session" {
			sessionCookie = c
		}
	}
	if sessionCookie == nil {
		t.Fatal("missing session cookie")
	}
	csrf, ok := s.session(func() *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/admin", nil)
		r.AddCookie(sessionCookie)
		return r
	}())
	if !ok {
		t.Fatal("session not stored")
	}
	form := url.Values{"csrf": {csrf}, "title": {"A new reflection"}, "tags": {"#faith, #reflection"}, "summary": {"A short summary"}, "markdown": {"## Welcome\n\nA **bold** thought.\n\n<script>alert(1)</script>"}}
	saveReq := httptest.NewRequest(http.MethodPost, "/admin/posts", strings.NewReader(form.Encode()))
	saveReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	saveReq.AddCookie(sessionCookie)
	saved := httptest.NewRecorder()
	handler.ServeHTTP(saved, saveReq)
	if saved.Code != http.StatusSeeOther {
		t.Fatalf("save draft: %d %s", saved.Code, saved.Body.String())
	}
	public := httptest.NewRecorder()
	handler.ServeHTTP(public, httptest.NewRequest(http.MethodGet, "/articles/a-new-reflection", nil))
	if public.Code != http.StatusNotFound {
		t.Fatalf("draft is public: %d", public.Code)
	}
	form.Set("published", "1")
	publishReq := httptest.NewRequest(http.MethodPost, "/admin/posts/a-new-reflection", strings.NewReader(form.Encode()))
	publishReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	publishReq.AddCookie(sessionCookie)
	published := httptest.NewRecorder()
	handler.ServeHTTP(published, publishReq)
	if published.Code != http.StatusSeeOther {
		t.Fatalf("publish: %d %s", published.Code, published.Body.String())
	}
	adminReq := httptest.NewRequest(http.MethodGet, "/admin/posts/a-new-reflection", nil)
	adminReq.AddCookie(sessionCookie)
	adminPage := httptest.NewRecorder()
	handler.ServeHTTP(adminPage, adminReq)
	if adminPage.Code != http.StatusOK || !strings.Contains(adminPage.Body.String(), `form="delete-post-form"`) || !strings.Contains(adminPage.Body.String(), `name="tags"`) {
		t.Fatalf("post editor missing controls: %d", adminPage.Code)
	}
	public = httptest.NewRecorder()
	handler.ServeHTTP(public, httptest.NewRequest(http.MethodGet, "/articles/a-new-reflection", nil))
	if public.Code != http.StatusOK || !strings.Contains(public.Body.String(), "<strong>bold</strong>") || strings.Contains(public.Body.String(), "<script>") || !strings.Contains(public.Body.String(), `href="/?tag=%23reflection#articles"`) {
		t.Fatalf("unexpected public article: %d %s", public.Code, public.Body.String())
	}
	for _, tc := range []struct {
		path, want string
		absent     bool
	}{
		{"/?tag=%23reflection", "A new reflection", false},
		{"/?tag=%23reflection&q=bold", "A new reflection", false},
		{"/?tag=%23reflection&q=missingword", "A new reflection", true},
		{"/?tag=%23questions", "A new reflection", true},
	} {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, tc.path, nil))
		found := strings.Contains(w.Body.String(), tc.want)
		if w.Code != http.StatusOK || found == tc.absent {
			t.Fatalf("filter %s: status %d, found=%v", tc.path, w.Code, found)
		}
	}
	filteredPage := httptest.NewRecorder()
	handler.ServeHTTP(filteredPage, httptest.NewRequest(http.MethodGet, "/?tag=%23reflection&q=bold", nil))
	if !strings.Contains(filteredPage.Body.String(), `name="tag" value="#reflection"`) {
		t.Fatal("search form did not preserve the selected hashtag")
	}
	if _, err := exec.LookPath("ffmpeg"); err == nil {
		logo, err := siteFiles.ReadFile("public/img/the-honest-question.png")
		if err != nil {
			t.Fatal(err)
		}
		upload := func(includeImage, remove bool) *httptest.ResponseRecorder {
			var body bytes.Buffer
			writer := multipart.NewWriter(&body)
			for key, values := range form {
				for _, value := range values {
					if err := writer.WriteField(key, value); err != nil {
						t.Fatal(err)
					}
				}
			}
			if remove {
				if err := writer.WriteField("remove_image", "1"); err != nil {
					t.Fatal(err)
				}
			}
			if includeImage {
				part, err := writer.CreateFormFile("image", "the-honest-question.png")
				if err != nil {
					t.Fatal(err)
				}
				if _, err := part.Write(logo); err != nil {
					t.Fatal(err)
				}
			}
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest(http.MethodPost, "/admin/posts/a-new-reflection", &body)
			req.Header.Set("Content-Type", writer.FormDataContentType())
			req.AddCookie(sessionCookie)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, req)
			return response
		}
		if response := upload(true, false); response.Code != http.StatusSeeOther {
			t.Fatalf("upload: %d %s", response.Code, response.Body.String())
		}
		first, err := s.store.get("a-new-reflection")
		if err != nil || first.Image == "" {
			t.Fatalf("thumbnail not saved: %v", err)
		}
		for _, path := range []string{"/", "/articles/a-new-reflection"} {
			page := httptest.NewRecorder()
			handler.ServeHTTP(page, httptest.NewRequest(http.MethodGet, path, nil))
			if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), "/uploads/"+first.Image) {
				t.Fatalf("thumbnail missing from %s", path)
			}
		}
		imageFile, err := os.Open(filepath.Join(s.dataDir, "uploads", first.Image))
		if err != nil {
			t.Fatal(err)
		}
		config, format, err := image.DecodeConfig(imageFile)
		imageFile.Close()
		if err != nil || format != "jpeg" || config.Width != thumbnailWidth || config.Height != thumbnailHeight {
			t.Fatalf("unexpected thumbnail: %dx%d %s %v", config.Width, config.Height, format, err)
		}
		imageResponse := httptest.NewRecorder()
		handler.ServeHTTP(imageResponse, httptest.NewRequest(http.MethodGet, "/uploads/"+first.Image, nil))
		if imageResponse.Code != http.StatusOK || imageResponse.Header().Get("Content-Type") != "image/jpeg" {
			t.Fatalf("thumbnail route: %d", imageResponse.Code)
		}
		if response := upload(true, false); response.Code != http.StatusSeeOther {
			t.Fatalf("replacement: %d %s", response.Code, response.Body.String())
		}
		second, err := s.store.get("a-new-reflection")
		if err != nil || second.Image == first.Image {
			t.Fatal("image was not replaced")
		}
		if _, err := os.Stat(filepath.Join(s.dataDir, "uploads", first.Image)); !os.IsNotExist(err) {
			t.Fatal("replaced thumbnail was not removed")
		}
		if response := upload(false, true); response.Code != http.StatusSeeOther {
			t.Fatalf("remove image: %d %s", response.Code, response.Body.String())
		}
		withoutImage, err := s.store.get("a-new-reflection")
		if err != nil || withoutImage.Image != "" {
			t.Fatal("image was not removed from post")
		}
		if _, err := os.Stat(filepath.Join(s.dataDir, "uploads", second.Image)); !os.IsNotExist(err) {
			t.Fatal("removed thumbnail still exists")
		}
	}
	form.Set("csrf", "bad")
	badReq := httptest.NewRequest(http.MethodPost, "/admin/posts/a-new-reflection", strings.NewReader(form.Encode()))
	badReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	badReq.AddCookie(sessionCookie)
	bad := httptest.NewRecorder()
	handler.ServeHTTP(bad, badReq)
	if bad.Code != http.StatusForbidden {
		t.Fatalf("missing CSRF protection: %d", bad.Code)
	}
	deleteReq := httptest.NewRequest(http.MethodPost, "/admin/posts/a-new-reflection/delete", strings.NewReader(form.Encode()))
	deleteReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	deleteReq.AddCookie(sessionCookie)
	badDelete := httptest.NewRecorder()
	handler.ServeHTTP(badDelete, deleteReq)
	if badDelete.Code != http.StatusForbidden {
		t.Fatalf("delete without CSRF: %d", badDelete.Code)
	}
	form.Set("csrf", csrf)
	deleteReq = httptest.NewRequest(http.MethodPost, "/admin/posts/a-new-reflection/delete", strings.NewReader(form.Encode()))
	deleteReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	deleteReq.AddCookie(sessionCookie)
	deleted := httptest.NewRecorder()
	handler.ServeHTTP(deleted, deleteReq)
	if deleted.Code != http.StatusSeeOther {
		t.Fatalf("delete: %d %s", deleted.Code, deleted.Body.String())
	}
	public = httptest.NewRecorder()
	handler.ServeHTTP(public, httptest.NewRequest(http.MethodGet, "/articles/a-new-reflection", nil))
	if public.Code != http.StatusNotFound {
		t.Fatalf("deleted post still public: %d", public.Code)
	}
}

func TestLegacyCategoryMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "main.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE posts (slug TEXT PRIMARY KEY, title TEXT NOT NULL, category TEXT NOT NULL, summary TEXT NOT NULL, markdown TEXT NOT NULL, read_time TEXT NOT NULL, published INTEGER NOT NULL, created_at TEXT NOT NULL, updated_at TEXT NOT NULL); CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL); INSERT INTO meta VALUES ('seeded','1'); INSERT INTO posts VALUES ('legacy','Legacy','QUESTIONS & FAITH','Summary','Body','1 min read',1,'2026-01-01','2026-01-01');`)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	store, err := openPostStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.close()
	post, err := store.get("legacy")
	if err != nil {
		t.Fatal(err)
	}
	if post.Tags != "#questions_faith" {
		t.Fatalf("unexpected migrated tags: %q", post.Tags)
	}
	if post.Image != "" {
		t.Fatalf("legacy post should not have an image: %q", post.Image)
	}
}

func TestPrivateDatabaseAndRequiredSettings(t *testing.T) {
	dir := t.TempDir()
	store, err := openPostStore(filepath.Join(dir, "main.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	store.close()
	info, err := os.Stat(filepath.Join(dir, "main.sqlite"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("database permissions: %v, %v", info, err)
	}
	store, err = openPostStore(filepath.Join(dir, "main.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`INSERT INTO sessions (token_hash, csrf_token, expires_at) VALUES (?,?,?)`, "expired-token", "csrf", time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	store.close()
	store, err = openPostStore(filepath.Join(dir, "main.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	var expired int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM sessions WHERE token_hash='expired-token'`).Scan(&expired); err != nil || expired != 0 {
		t.Fatalf("expired session was retained: %d %v", expired, err)
	}
	store.close()
	path := filepath.Join(dir, ".env")
	for _, key := range []string{"ADMIN_USERNAME", "ADMIN_PASSWORD", "SESSION_SECRET"} {
		t.Setenv(key, "")
	}
	if err := os.WriteFile(path, []byte("ADMIN_USERNAME=writer\nADMIN_PASSWORD=strong-password\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadSettings(path); err == nil {
		t.Fatal("missing session secret was accepted")
	}
	t.Setenv("ADMIN_USERNAME", "writer")
	t.Setenv("ADMIN_PASSWORD", "strong-password")
	t.Setenv("SESSION_SECRET", "a-secret-longer-than-thirty-two-characters")
	if _, err := loadSettings(path); err != nil {
		t.Fatalf("valid settings rejected: %v", err)
	}
	t.Setenv("TRUSTED_PROXY_CIDRS", "invalid")
	if _, err := loadSettings(path); err == nil {
		t.Fatal("invalid trusted proxy range was accepted")
	}
}

func TestTrustedProxyClientIP(t *testing.T) {
	_, network, err := net.ParseCIDR("172.18.0.0/16")
	if err != nil {
		t.Fatal(err)
	}
	s := &server{trustedProxyCIDRs: []*net.IPNet{network}}
	req := httptest.NewRequest(http.MethodPost, "/questions", nil)
	req.RemoteAddr = "172.18.0.2:1234"
	req.Header.Set("X-Forwarded-For", "203.0.113.9, 172.18.0.3")
	if got := s.clientIP(req); got != "203.0.113.9" {
		t.Fatalf("trusted chain: %q", got)
	}
	req.Header.Set("X-Forwarded-For", "203.0.113.9, 198.51.100.2")
	if got := s.clientIP(req); got != "198.51.100.2" {
		t.Fatalf("spoofed first address was trusted: %q", got)
	}
	req.RemoteAddr = "192.0.2.1:1234"
	if got := s.clientIP(req); got != "192.0.2.1" {
		t.Fatalf("untrusted peer spoofed address: %q", got)
	}
}

func TestLoginRateLimitAndSignedSession(t *testing.T) {
	s, err := newServer(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.store.close()
	s.adminUsername = "writer"
	s.adminPassword = "correct-password"
	s.sessionSecret = []byte("a-secret-longer-than-thirty-two-characters")
	handler := s.routes()
	page := httptest.NewRecorder()
	handler.ServeHTTP(page, httptest.NewRequest(http.MethodGet, "/admin/login", nil))
	var csrf *http.Cookie
	for _, cookie := range page.Result().Cookies() {
		if cookie.Name == "admin_login_csrf" {
			csrf = cookie
		}
	}
	if csrf == nil {
		t.Fatal("login CSRF cookie missing")
	}
	attempt := func(password, remote string) *httptest.ResponseRecorder {
		form := url.Values{"username": {"writer"}, "password": {password}, "csrf": {csrf.Value}}
		req := httptest.NewRequest(http.MethodPost, "/admin/login", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("X-Forwarded-For", "203.0.113.200") // An untrusted header cannot evade the limit.
		req.RemoteAddr = remote
		req.AddCookie(csrf)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		return response
	}
	for i := 0; i < loginFailureLimit; i++ {
		if response := attempt("wrong-password", "192.0.2.1:1000"); response.Code != http.StatusSeeOther {
			t.Fatalf("failure %d: %d", i, response.Code)
		}
	}
	if response := attempt("correct-password", "192.0.2.1:2000"); response.Code != http.StatusTooManyRequests {
		t.Fatalf("locked IP: %d", response.Code)
	}
	if _, err := s.store.db.Exec(`UPDATE login_failures SET attempted_at=? WHERE ip=?`, time.Now().Add(-loginWindow-time.Minute).Unix(), "192.0.2.1"); err != nil {
		t.Fatal(err)
	}
	if response := attempt("correct-password", "192.0.2.1:3000"); response.Code != http.StatusSeeOther {
		t.Fatalf("expired failures still block login: %d", response.Code)
	}
	response := attempt("correct-password", "192.0.2.2:1000")
	if response.Code != http.StatusSeeOther {
		t.Fatalf("other IP: %d", response.Code)
	}
	var session *http.Cookie
	for _, cookie := range response.Result().Cookies() {
		if cookie.Name == "admin_session" {
			session = cookie
		}
	}
	if session == nil || session.MaxAge != int(sessionLifetime.Seconds()) {
		t.Fatal("signed 12 hour session cookie missing")
	}
	req := httptest.NewRequest(http.MethodGet, "/admin", nil)
	req.AddCookie(session)
	if _, ok := s.session(req); !ok {
		t.Fatal("valid signed session rejected")
	}
	session.Value = "A" + session.Value[1:]
	req = httptest.NewRequest(http.MethodGet, "/admin", nil)
	req.AddCookie(session)
	if _, ok := s.session(req); ok {
		t.Fatal("tampered session accepted")
	}
}
