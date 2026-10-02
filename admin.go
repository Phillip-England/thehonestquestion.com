package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"html/template"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const loginWindow = 24 * time.Hour
const loginFailureLimit = 5
const sessionLifetime = 12 * time.Hour

type adminPageData struct {
	Success   bool
	Title     string
	Posts     []article
	Post      article
	Editing   bool
	Error     string
	CSRF      string
	LoginCSRF string
	Messages  []contactMessage
	Page      int
	PrevPage  int
	NextPage  int
	HasPrev   bool
	HasNext   bool
}

func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
func tokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
func constantEqual(a, b string) bool {
	aa := sha256.Sum256([]byte(a))
	bb := sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(aa[:], bb[:]) == 1
}
func (s *server) signedSession(token string) string {
	mac := hmac.New(sha256.New, s.sessionSecret)
	mac.Write([]byte(token))
	return token + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
func (s *server) sessionToken(value string) (string, bool) {
	token, signature, ok := strings.Cut(value, ".")
	if !ok || len(token) != 43 || len(signature) != 43 || len(s.sessionSecret) < 32 {
		return "", false
	}
	expected := s.signedSession(token)
	return token, constantEqual(value, expected)
}
func (s *server) trustedProxy(ip net.IP) bool {
	for _, network := range s.trustedProxyCIDRs {
		if network.Contains(ip) {
			return true
		}
	}
	return false
}
func (s *server) clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	peer := net.ParseIP(host)
	if peer == nil || !s.trustedProxy(peer) || r.Header.Get("X-Forwarded-For") == "" {
		return host
	}
	// Work from the nearest proxy outward. The first untrusted address is the client.
	forwarded := strings.Split(r.Header.Get("X-Forwarded-For"), ",")
	for i := len(forwarded) - 1; i >= 0; i-- {
		ip := net.ParseIP(strings.TrimSpace(forwarded[i]))
		if ip == nil {
			return host
		}
		if !s.trustedProxy(ip) {
			return ip.String()
		}
	}
	return host
}
func secureRequest(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}
func setCookie(w http.ResponseWriter, r *http.Request, name, value string, age int) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: value, Path: "/admin", HttpOnly: true, Secure: secureRequest(r), SameSite: http.SameSiteStrictMode, MaxAge: age})
}
func (s *server) adminRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /admin/login", s.loginPage)
	mux.HandleFunc("POST /admin/login", s.login)
	mux.HandleFunc("POST /admin/logout", s.logout)
	mux.HandleFunc("GET /admin", s.adminIndex)
	mux.HandleFunc("GET /admin/messages", s.adminMessages)
	mux.HandleFunc("GET /admin/database", s.databasePage)
	mux.HandleFunc("POST /admin/database", s.uploadDatabase)
	mux.HandleFunc("GET /admin/posts/new", s.newPostPage)
	mux.HandleFunc("GET /admin/posts/{slug}", s.editPostPage)
	mux.HandleFunc("POST /admin/posts", s.createPost)
	mux.HandleFunc("POST /admin/posts/{slug}", s.updatePost)
	mux.HandleFunc("POST /admin/posts/{slug}/delete", s.deletePost)
	mux.HandleFunc("POST /admin/preview", s.previewPost)
}
func (s *server) renderAdmin(w http.ResponseWriter, name string, data adminPageData) {
	t, err := template.ParseFS(siteFiles, "templates/admin.html")
	if err != nil {
		http.Error(w, "Template unavailable", 500)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Frame-Options", "DENY")
	if err := t.ExecuteTemplate(w, name, data); err != nil {
		http.Error(w, "Could not render page", 500)
	}
}
func (s *server) loginPage(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.session(r); ok {
		http.Redirect(w, r, "/admin", http.StatusSeeOther)
		return
	}
	token, err := randomToken()
	if err != nil {
		http.Error(w, "Unavailable", 500)
		return
	}
	setCookie(w, r, "admin_login_csrf", token, 600)
	s.renderAdmin(w, "login", adminPageData{Title: "Admin login", LoginCSRF: token, Error: r.URL.Query().Get("error")})
}
func (s *server) login(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid form", 400)
		return
	}
	cookie, err := r.Cookie("admin_login_csrf")
	if err != nil || cookie.Value == "" || !constantEqual(cookie.Value, r.PostFormValue("csrf")) {
		http.Error(w, "Invalid form token", 403)
		return
	}
	if !sameOrigin(r) {
		http.Error(w, "Invalid origin", 403)
		return
	}
	// The single SQLite connection serializes the limit check and insert.
	tx, err := s.store.db.Begin()
	if err != nil {
		http.Error(w, "Unavailable", 500)
		return
	}
	defer tx.Rollback()
	now := time.Now().UTC()
	cutoff := now.Add(-loginWindow).Unix()
	if _, err := tx.Exec(`DELETE FROM login_failures WHERE attempted_at <= ?`, cutoff); err != nil {
		http.Error(w, "Unavailable", 500)
		return
	}
	if _, err := tx.Exec(`DELETE FROM sessions WHERE expires_at < ?`, now.Add(-time.Second).Format(time.RFC3339Nano)); err != nil {
		http.Error(w, "Unavailable", 500)
		return
	}
	ip := s.clientIP(r)
	var failures int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM login_failures WHERE ip=? AND attempted_at > ?`, ip, cutoff).Scan(&failures); err != nil {
		http.Error(w, "Unavailable", 500)
		return
	}
	if failures >= loginFailureLimit {
		if err := tx.Commit(); err != nil {
			http.Error(w, "Unavailable", 500)
			return
		}
		w.Header().Set("Retry-After", "3600")
		http.Error(w, "Too many sign-in attempts. Please try again later.", http.StatusTooManyRequests)
		return
	}
	userOK := constantEqual(r.PostFormValue("username"), s.adminUsername)
	passwordOK := constantEqual(r.PostFormValue("password"), s.adminPassword)
	if !userOK || !passwordOK {
		if _, err := tx.Exec(`INSERT INTO login_failures (ip, attempted_at) VALUES (?, ?)`, ip, now.Unix()); err != nil {
			http.Error(w, "Unavailable", 500)
			return
		}
		if err := tx.Commit(); err != nil {
			http.Error(w, "Unavailable", 500)
			return
		}
		http.Redirect(w, r, "/admin/login?error=1", http.StatusSeeOther)
		return
	}
	token, err := randomToken()
	if err != nil {
		http.Error(w, "Unavailable", 500)
		return
	}
	csrf, err := randomToken()
	if err != nil {
		http.Error(w, "Unavailable", 500)
		return
	}
	expires := now.Add(sessionLifetime)
	if _, err := tx.Exec(`INSERT INTO sessions VALUES (?,?,?)`, tokenHash(token), csrf, expires.Format(time.RFC3339Nano)); err != nil {
		http.Error(w, "Unavailable", 500)
		return
	}
	if _, err := tx.Exec(`DELETE FROM login_failures WHERE ip=?`, ip); err != nil {
		http.Error(w, "Unavailable", 500)
		return
	}
	if err := tx.Commit(); err != nil {
		http.Error(w, "Unavailable", 500)
		return
	}
	setCookie(w, r, "admin_session", s.signedSession(token), int(sessionLifetime.Seconds()))
	setCookie(w, r, "admin_login_csrf", "", -1)
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}
func (s *server) session(r *http.Request) (string, bool) {
	cookie, err := r.Cookie("admin_session")
	if err != nil || len(cookie.Value) > 100 {
		return "", false
	}
	token, ok := s.sessionToken(cookie.Value)
	if !ok {
		return "", false
	}
	var csrf, expires string
	err = s.store.db.QueryRow(`SELECT csrf_token,expires_at FROM sessions WHERE token_hash=?`, tokenHash(token)).Scan(&csrf, &expires)
	if err != nil {
		return "", false
	}
	exp, err := time.Parse(time.RFC3339Nano, expires)
	return csrf, err == nil && time.Now().Before(exp)
}
func (s *server) requireAdmin(w http.ResponseWriter, r *http.Request, mutation bool) (string, bool) {
	csrf, ok := s.session(r)
	if !ok {
		if mutation {
			http.Error(w, "Please sign in", 401)
		} else {
			http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
		}
		return "", false
	}
	if mutation {
		if !sameOrigin(r) || !constantEqual(csrf, r.PostFormValue("csrf")) {
			http.Error(w, "Invalid form token", 403)
			return "", false
		}
	}
	return csrf, true
}
func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		origin = r.Header.Get("Referer")
	}
	if origin == "" {
		return true
	} // Non-browser clients still need a valid session and CSRF token.
	parsed, err := url.Parse(origin)
	return err == nil && strings.EqualFold(parsed.Host, r.Host) && (parsed.Scheme == "http" || parsed.Scheme == "https")
}
func (s *server) logout(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid form", 400)
		return
	}
	if _, ok := s.requireAdmin(w, r, true); !ok {
		return
	}
	if cookie, err := r.Cookie("admin_session"); err == nil {
		if token, ok := s.sessionToken(cookie.Value); ok {
			s.store.db.Exec(`DELETE FROM sessions WHERE token_hash=?`, tokenHash(token))
		}
	}
	setCookie(w, r, "admin_session", "", -1)
	http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
}
func (s *server) adminIndex(w http.ResponseWriter, r *http.Request) {
	csrf, ok := s.requireAdmin(w, r, false)
	if !ok {
		return
	}
	posts, err := s.store.list(false)
	if err != nil {
		http.Error(w, "Unavailable", 500)
		return
	}
	s.renderAdmin(w, "index", adminPageData{Title: "Posts", Posts: posts, CSRF: csrf})
}

func (s *server) adminMessages(w http.ResponseWriter, r *http.Request) {
	csrf, ok := s.requireAdmin(w, r, false)
	if !ok {
		return
	}
	page, err := strconv.Atoi(r.URL.Query().Get("page"))
	if err != nil || page < 1 {
		page = 1
	}
	if page > contactMaxMessages/contactPageSize {
		page = contactMaxMessages / contactPageSize
	}
	messages, hasNext, err := s.store.listContacts(page)
	if err != nil {
		log.Printf("list contact messages: %v", err)
		http.Error(w, "Unavailable", http.StatusInternalServerError)
		return
	}
	s.renderAdmin(w, "messages", adminPageData{Title: "Messages", CSRF: csrf, Messages: messages, Page: page, PrevPage: page - 1, NextPage: page + 1, HasPrev: page > 1, HasNext: hasNext})
}
func (s *server) newPostPage(w http.ResponseWriter, r *http.Request) {
	csrf, ok := s.requireAdmin(w, r, false)
	if !ok {
		return
	}
	s.renderAdmin(w, "editor", adminPageData{Title: "New post", Post: article{Tags: "#faith"}, CSRF: csrf})
}
func (s *server) editPostPage(w http.ResponseWriter, r *http.Request) {
	csrf, ok := s.requireAdmin(w, r, false)
	if !ok {
		return
	}
	a, err := s.store.get(r.PathValue("slug"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	s.renderAdmin(w, "editor", adminPageData{Title: "Edit post", Post: a, Editing: true, CSRF: csrf})
}
func parsePostForm(r *http.Request) article {
	return article{Title: strings.TrimSpace(r.PostFormValue("title")), Tags: strings.TrimSpace(r.PostFormValue("tags")), Summary: strings.TrimSpace(r.PostFormValue("summary")), Markdown: strings.TrimSpace(r.PostFormValue("markdown")), Published: r.PostFormValue("published") == "1"}
}
func validPost(a article) bool {
	return len([]rune(a.Title)) >= 3 && len([]rune(a.Title)) <= 160 && len([]rune(a.Tags)) <= 320 && a.Tags != "" && len([]rune(a.Summary)) <= 400 && a.Summary != "" && len([]rune(a.Markdown)) <= 50000 && a.Markdown != ""
}
func (s *server) savePost(w http.ResponseWriter, r *http.Request, existing string) {
	r.Body = http.MaxBytesReader(w, r.Body, 25<<20)
	var parseErr error
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		parseErr = r.ParseMultipartForm(8 << 20)
		if r.MultipartForm != nil {
			defer r.MultipartForm.RemoveAll()
		}
	} else {
		parseErr = r.ParseForm()
	}
	if parseErr != nil {
		http.Error(w, "Post is too large", 400)
		return
	}
	csrf, ok := s.requireAdmin(w, r, true)
	if !ok {
		return
	}
	var previous article
	if existing != "" {
		var err error
		previous, err = s.store.get(existing)
		if err != nil {
			http.NotFound(w, r)
			return
		}
	}
	a := parsePostForm(r)
	a.Slug = existing
	a.Image = previous.Image
	tags, validTags := normalizeTags(a.Tags)
	if !validPost(a) || !validTags {
		s.renderAdmin(w, "editor", adminPageData{Title: "Edit post", Post: a, Editing: existing != "", CSRF: csrf, Error: "Add a title, summary, Markdown body, and up to 10 hashtags such as #faith #questions."})
		return
	}
	a.Tags = tags
	var file io.ReadCloser
	var uploadErr error
	var originalName string
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		uploaded, fileHeader, err := r.FormFile("image")
		file, uploadErr = uploaded, err
		if fileHeader != nil {
			originalName = fileHeader.Filename
		}
	}
	if uploadErr != nil && !errors.Is(uploadErr, http.ErrMissingFile) {
		http.Error(w, "Could not read uploaded image", http.StatusBadRequest)
		return
	}
	newImage := ""
	if file != nil {
		newImage, uploadErr = s.saveImage(file, originalName)
		file.Close()
		if uploadErr != nil {
			s.renderAdmin(w, "editor", adminPageData{Title: "Edit post", Post: a, Editing: existing != "", CSRF: csrf, Error: "Could not process that image. Try a JPG, PNG, WebP, GIF, or HEIC image. The maximum size is 20 MB."})
			return
		}
		a.Image = newImage
	} else if r.PostFormValue("remove_image") == "1" {
		a.Image = ""
	}
	slug, err := s.store.save(a, existing)
	if err != nil {
		if newImage != "" {
			if removeErr := s.removeImage(newImage); removeErr != nil {
				log.Printf("remove unsaved image: %v", removeErr)
			}
		}
		http.Error(w, "Could not save post", 500)
		return
	}
	if previous.Image != "" && previous.Image != a.Image {
		if err := s.removeImage(previous.Image); err != nil {
			log.Printf("remove replaced image: %v", err)
		}
	}
	http.Redirect(w, r, "/admin/posts/"+slug+"?saved=1", http.StatusSeeOther)
}
func (s *server) createPost(w http.ResponseWriter, r *http.Request) { s.savePost(w, r, "") }
func (s *server) updatePost(w http.ResponseWriter, r *http.Request) {
	s.savePost(w, r, r.PathValue("slug"))
}
func (s *server) deletePost(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid form", http.StatusBadRequest)
		return
	}
	if _, ok := s.requireAdmin(w, r, true); !ok {
		return
	}
	previous, err := s.store.get(r.PathValue("slug"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	deleted, err := s.store.delete(r.PathValue("slug"))
	if err != nil {
		http.Error(w, "Could not delete post", http.StatusInternalServerError)
		return
	}
	if !deleted {
		http.NotFound(w, r)
		return
	}
	if err := s.removeImage(previous.Image); err != nil {
		log.Printf("remove deleted post image: %v", err)
	}
	http.Redirect(w, r, "/admin?deleted=1", http.StatusSeeOther)
}
func (s *server) previewPost(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 100<<10)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Post is too large", 400)
		return
	}
	if _, ok := s.requireAdmin(w, r, true); !ok {
		return
	}
	body := r.PostFormValue("markdown")
	if len(body) > 100<<10 {
		http.Error(w, "Post is too large", 400)
		return
	}
	rendered, err := renderMarkdown(body)
	if err != nil {
		http.Error(w, "Could not render Markdown", 500)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	io.WriteString(w, string(rendered))
}
