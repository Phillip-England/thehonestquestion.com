package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestArticleURLsAndComments(t *testing.T) {
	s, err := newServer(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.store.close()
	post := article{Slug: "chosen-url", Title: "An article", Tags: "#faith", Summary: "Summary", Markdown: "Body", Published: true}
	slug, err := s.store.save(post, "")
	if err != nil || slug != "chosen-url" {
		t.Fatalf("save chosen URL: %s %v", slug, err)
	}
	if _, err := s.store.save(article{Slug: slug, Title: "Another", Tags: "#faith", Summary: "Summary", Markdown: "Body"}, ""); err != errSlugTaken {
		t.Fatalf("duplicate URL: %v", err)
	}
	if _, err := s.store.save(article{Slug: "Bad URL", Title: "Another"}, ""); err == nil {
		t.Fatal("invalid URL accepted")
	}
	form := url.Values{"comment": {"A careful first thought"}}
	submit := func(token, ip string, trap bool) {
		values := url.Values{}
		for key, v := range form {
			values[key] = v
		}
		values.Set("comment_token", token)
		if trap {
			values.Set("homepage", "spam.example")
		}
		r := httptest.NewRequest(http.MethodPost, "/articles/"+slug+"/comments", strings.NewReader(values.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.RemoteAddr = ip + ":1234"
		w := httptest.NewRecorder()
		s.routes().ServeHTTP(w, r)
		if w.Code != http.StatusSeeOther {
			t.Fatalf("comment response: %d %s", w.Code, w.Body.String())
		}
	}
	count := func(want int) {
		var got int
		if err := s.store.db.QueryRow(`SELECT count(*) FROM comments`).Scan(&got); err != nil || got != want {
			t.Fatalf("comments: %d, want %d, err %v", got, want, err)
		}
	}
	fresh, _ := s.newCommentToken(slug)
	submit(fresh, "192.0.2.1", false)
	count(0)
	old := time.Now().Add(-6 * time.Second).Unix()
	issued := strconv.FormatInt(old, 10)
	ready := issued + "." + s.commentSignature(slug, issued)
	submit(ready, "192.0.2.1", true)
	count(0)
	submit(ready, "192.0.2.1", false)
	submit(ready, "192.0.2.1", false)
	count(1)
	post.Slug = "renamed-url"
	if got, err := s.store.save(post, slug); err != nil || got != post.Slug {
		t.Fatalf("rename: %s %v", got, err)
	}
	if oldPage := httptest.NewRecorder(); true {
		s.routes().ServeHTTP(oldPage, httptest.NewRequest(http.MethodGet, "/articles/"+slug, nil))
		if oldPage.Code != http.StatusNotFound {
			t.Fatalf("old URL: %d", oldPage.Code)
		}
	}
	page := httptest.NewRecorder()
	s.routes().ServeHTTP(page, httptest.NewRequest(http.MethodGet, "/articles/renamed-url", nil))
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), "A careful first thought") {
		t.Fatal("comment did not follow renamed URL")
	}
}

func TestEgateQuestionRequest(t *testing.T) {
	var sent bool
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/email" || r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer eg_test" || r.Header.Get("X-Egate-Client-IP") != "192.0.2.10" {
			t.Errorf("unexpected egate request")
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body["from"] != "noreply@example.com" || body["to"] != "owner@example.com" || body["reply_to"] != "visitor@example.com" || !strings.Contains(body["text_body"], "A private question") {
			t.Errorf("unexpected body: %+v", body)
		}
		sent = true
		w.WriteHeader(http.StatusOK)
	}))
	defer gateway.Close()
	g := emailGateway{Host: gateway.URL, APIKey: "eg_test", Sender: "noreply@example.com", Recipient: "owner@example.com"}
	if err := g.sendQuestion(t.Context(), question{Name: "Visitor", Email: "visitor@example.com", Message: "A private question"}, "192.0.2.10"); err != nil || !sent {
		t.Fatalf("egate send: %v, sent %v", err, sent)
	}
}
