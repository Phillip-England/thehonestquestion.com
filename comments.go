package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type comment struct {
	Body      string
	CreatedAt string
}

const commentMaxLength = 500

type adminComment struct {
	ID        int64
	PostSlug  string
	PostTitle string
	Body      string
	CreatedAt string
}

const adminCommentPageSize = 50

func (s *postStore) listAdminComments(page int) ([]adminComment, bool, error) {
	rows, err := s.db.Query(`SELECT c.id, c.post_slug, p.title, c.body, c.created_at
		FROM comments c JOIN posts p ON p.slug = c.post_slug
		ORDER BY c.created_at DESC, c.id DESC LIMIT ? OFFSET ?`, adminCommentPageSize+1, (page-1)*adminCommentPageSize)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	comments := []adminComment{}
	for rows.Next() {
		var c adminComment
		var stamp int64
		if err := rows.Scan(&c.ID, &c.PostSlug, &c.PostTitle, &c.Body, &stamp); err != nil {
			return nil, false, err
		}
		c.CreatedAt = time.Unix(stamp, 0).UTC().Format("Jan 2, 2006 at 3:04 PM UTC")
		comments = append(comments, c)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	hasNext := len(comments) > adminCommentPageSize
	if hasNext {
		comments = comments[:adminCommentPageSize]
	}
	return comments, hasNext, nil
}

func (s *postStore) deleteComment(id int64) error {
	_, err := s.db.Exec(`DELETE FROM comments WHERE id=?`, id)
	return err
}

var errCommentExists = errors.New("comment already exists for this article and IP")

func (s *postStore) listComments(slug string) ([]comment, error) {
	rows, err := s.db.Query(`SELECT body, created_at FROM comments WHERE post_slug=? ORDER BY created_at, id`, slug)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	comments := []comment{}
	for rows.Next() {
		var c comment
		var stamp int64
		if err := rows.Scan(&c.Body, &stamp); err != nil {
			return nil, err
		}
		c.CreatedAt = time.Unix(stamp, 0).UTC().Format("Jan 2, 2006")
		comments = append(comments, c)
	}
	return comments, rows.Err()
}

func (s *postStore) hasComment(slug, ip string) (bool, error) {
	var count int
	err := s.db.QueryRow(`SELECT count(*) FROM comments WHERE post_slug=? AND ip=?`, slug, ip).Scan(&count)
	return count > 0, err
}

func (s *postStore) saveComment(slug, ip, body string, now time.Time) error {
	_, err := s.db.Exec(`INSERT INTO comments (post_slug, ip, body, created_at) VALUES (?,?,?,?)`, slug, ip, body, now.Unix())
	if err != nil {
		if exists, checkErr := s.hasComment(slug, ip); checkErr == nil && exists {
			return errCommentExists
		}
	}
	return err
}

func (s *server) commentSignature(slug, stamp string) string {
	mac := hmac.New(sha256.New, s.commentSecret)
	mac.Write([]byte(slug + "\x00" + stamp))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (s *server) newCommentToken(slug string) (string, error) {
	stamp := strconv.FormatInt(time.Now().Unix(), 10)
	return stamp + "." + s.commentSignature(slug, stamp), nil
}

func (s *server) commentTokenReady(slug, token string, now time.Time) bool {
	stamp, signature, ok := strings.Cut(token, ".")
	if !ok {
		return false
	}
	issued, err := strconv.ParseInt(stamp, 10, 64)
	if err != nil || !hmac.Equal([]byte(signature), []byte(s.commentSignature(slug, stamp))) {
		return false
	}
	age := now.Sub(time.Unix(issued, 0))
	return age >= 5*time.Second && age <= time.Hour
}

func (s *server) submitComment(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	a, err := s.store.get(slug)
	if err != nil || !a.Published {
		http.NotFound(w, r)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Comment is too long", http.StatusBadRequest)
		return
	}
	if !sameOrigin(r) {
		http.Error(w, "Invalid origin", http.StatusForbidden)
		return
	}
	redirect := func(state string) {
		http.Redirect(w, r, fmt.Sprintf("/articles/%s?comment=%s#comments", slug, state), http.StatusSeeOther)
	}
	for _, name := range []string{"website", "homepage", "company", "phone", "address", "subject", "contact_email", "feedback"} {
		if r.PostFormValue(name) != "" {
			redirect("sent")
			return
		}
	}
	if !s.commentTokenReady(slug, r.PostFormValue("comment_token"), time.Now()) {
		redirect("sent")
		return
	}
	body := strings.TrimSpace(r.PostFormValue("comment"))
	if len([]rune(body)) < 10 || len([]rune(body)) > commentMaxLength {
		redirect("error")
		return
	}
	if err := s.store.saveComment(slug, s.clientIP(r), body, time.Now().UTC()); err != nil {
		if !errors.Is(err, errCommentExists) {
			http.Error(w, "Comments are temporarily unavailable", 500)
			return
		}
	}
	redirect("sent")
}
