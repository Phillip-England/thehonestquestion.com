package main

import (
	"embed"
	"errors"
	"html/template"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

//go:embed templates/*.html public
var siteFiles embed.FS

type article struct {
	Slug      string
	Tags      string
	TagList   []string
	Image     string
	Title     string
	Summary   string
	ReadTime  string
	Sections  []section
	Body      template.HTML
	Markdown  string
	Published bool
}

type section struct {
	Heading string
	Body    []string
}

var articles = []article{
	{
		Slug: "when-faith-feels-fragile", Tags: "#faith #doubt", Title: "When faith feels fragile", ReadTime: "4 min read",
		Summary: "A gentle place to start when everything you once felt certain about suddenly feels less certain.",
		Sections: []section{
			{Heading: "You can begin where you are", Body: []string{"Some days faith feels less like a steady foundation and more like a question you are afraid to say out loud. If that is where you are, you do not need to rush toward a confident answer today.", "Uncertainty can be frightening, especially when belief has been woven into your family, friendships, and sense of self. It makes sense that this matters so much."}},
			{Heading: "Give the question a little room", Body: []string{"Try writing down the question as plainly as you can. Then notice what is underneath it: fear, grief, curiosity, anger, or perhaps a little of each. Naming those feelings can make the question less lonely.", "You are allowed to take one small step at a time. Read slowly. Talk with someone who can listen without trying to win. Rest when you need to."}},
			{Heading: "You are still welcome here", Body: []string{"This space does not require a polished testimony or a tidy conclusion. Your honest questions are welcome. There is room for you to breathe while you work through them."}},
		},
	},
	{
		Slug: "can-i-ask-hard-questions", Tags: "#questions #faith", Title: "Can I ask the hard questions?", ReadTime: "3 min read",
		Summary: "On the fear that asking might mean you are doing faith wrong, and why honest curiosity needs room.",
		Sections: []section{
			{Heading: "A question is not a verdict", Body: []string{"A hard question does not tell the whole story of your faith. It tells you that something matters enough to think about carefully.", "Many Christians have wrestled with doubt, disappointment, and unanswered prayer. You do not have to pretend that the difficult parts are easy."}},
			{Heading: "Find a safe conversation", Body: []string{"Consider sharing one question with a person who can stay curious with you. A good listener will not punish you for asking or demand that you settle everything in a single conversation.", "If a conversation leaves you feeling overwhelmed, it is okay to pause and come back later. Your pace matters."}},
			{Heading: "Start small", Body: []string{"You do not need to solve every question at once. Pick the one that feels closest to the surface, write it down, and give yourself permission to explore it honestly."}},
		},
	},
	{
		Slug: "a-breathing-practice", Tags: "#anxiety #practice", Title: "A small practice for anxious moments", ReadTime: "2 min read",
		Summary: "A simple way to slow down when a question or conversation leaves your body feeling on edge.",
		Sections: []section{
			{Heading: "Pause and notice", Body: []string{"Place your feet on the floor if you can. Notice where your body meets the chair or ground. Let your shoulders soften a little.", "Breathe in gently, then let the exhale be a little longer than the inhale. Do this a few times without trying to force a particular feeling."}},
			{Heading: "Name what is here", Body: []string{"You might quietly say, ‘I am feeling afraid,’ or ‘I have a question I cannot answer yet.’ There is no need to argue with the feeling in this moment.", "When you are ready, ask yourself what you need next: a glass of water, a walk, a trusted person, or simply a break from thinking."}},
		},
	},
}

type pageData struct {
	Title       string
	Articles    []article
	Article     article
	Tags        []tagFilter
	Query       string
	SelectedTag string
	Filtering   bool
	Sent        bool
	Error       string
}

type tagFilter struct {
	Name     string
	URL      string
	Selected bool
}

type server struct {
	home              *template.Template
	article           *template.Template
	dataDir           string
	store             *postStore
	adminUsername     string
	adminPassword     string
	sessionSecret     []byte
	trustedProxyCIDRs []*net.IPNet
}

var emailPattern = regexp.MustCompile(`^[^\s@]+@[^\s@]+\.[^\s@]+$`)

func newServer(dataDir string) (*server, error) {
	home, err := template.ParseFS(siteFiles, "templates/home.html")
	if err != nil {
		return nil, err
	}
	articlePage, err := template.ParseFS(siteFiles, "templates/article.html")
	if err != nil {
		return nil, err
	}
	store, err := openPostStore(filepath.Join(dataDir, "main.sqlite"))
	if err != nil {
		return nil, err
	}
	return &server{home: home, article: articlePage, dataDir: dataDir, store: store}, nil
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	public, _ := fs.Sub(siteFiles, "public")
	mux.Handle("GET /public/", http.StripPrefix("/public/", http.FileServer(http.FS(public))))
	mux.HandleFunc("GET /uploads/{name}", s.serveImage)
	mux.HandleFunc("GET /", s.homePage)
	mux.HandleFunc("GET /articles/{slug}", s.articlePage)
	mux.HandleFunc("POST /questions", s.submitQuestion)
	s.adminRoutes(mux)
	return mux
}

func (s *server) homePage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	posts, err := s.store.list(true)
	if err != nil {
		http.Error(w, "Articles are temporarily unavailable", http.StatusInternalServerError)
		return
	}
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	if len([]rune(query)) > 100 {
		query = string([]rune(query)[:100])
	}
	selectedTag := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("tag")))
	allTags := map[string]bool{}
	for _, post := range posts {
		for _, tag := range post.TagList {
			allTags[tag] = true
		}
	}
	filtered := make([]article, 0, len(posts))
	for _, post := range posts {
		if selectedTag != "" && !hasTag(post, selectedTag) {
			continue
		}
		if query != "" && !strings.Contains(strings.ToLower(post.Title+" "+post.Summary+" "+post.Markdown+" "+post.Tags), strings.ToLower(query)) {
			continue
		}
		filtered = append(filtered, post)
	}
	tagNames := make([]string, 0, len(allTags))
	for tag := range allTags {
		tagNames = append(tagNames, tag)
	}
	sort.Strings(tagNames)
	tagFilters := make([]tagFilter, 0, len(tagNames))
	for _, tag := range tagNames {
		values := url.Values{"tag": {tag}}
		if query != "" {
			values.Set("q", query)
		}
		tagFilters = append(tagFilters, tagFilter{Name: tag, URL: "/?" + values.Encode() + "#articles", Selected: selectedTag == tag})
	}
	if err := s.home.Execute(w, pageData{Title: "The Honest Question — A place to pause, wonder, and be heard", Articles: filtered, Tags: tagFilters, Query: query, SelectedTag: selectedTag, Filtering: query != "" || selectedTag != "", Sent: r.URL.Query().Get("sent") == "1", Error: r.URL.Query().Get("error")}); err != nil {
		log.Printf("render home: %v", err)
	}
}

func (s *server) articlePage(w http.ResponseWriter, r *http.Request) {
	if a, err := s.store.get(r.PathValue("slug")); err == nil && a.Published {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		tagLinks := make([]tagFilter, 0, len(a.TagList))
		for _, tag := range a.TagList {
			tagLinks = append(tagLinks, tagFilter{Name: tag, URL: "/?" + url.Values{"tag": {tag}}.Encode() + "#articles"})
		}
		if err := s.article.Execute(w, pageData{Title: a.Title + " — The Honest Question", Article: a, Tags: tagLinks}); err != nil {
			log.Printf("render article: %v", err)
		}
		return
	}
	http.NotFound(w, r)
}

func (s *server) submitQuestion(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, "/?error=too-long#ask", http.StatusSeeOther)
		return
	}
	if !sameOrigin(r) {
		http.Redirect(w, r, "/?error=unavailable#ask", http.StatusSeeOther)
		return
	}
	if r.PostFormValue("website") != "" {
		// Quietly discard submissions from bots that fill the hidden field.
		http.Redirect(w, r, "/?sent=1#ask", http.StatusSeeOther)
		return
	}
	q := question{
		CreatedAt: time.Now().UTC(),
		Name:      strings.TrimSpace(r.PostFormValue("name")),
		Email:     strings.TrimSpace(r.PostFormValue("email")),
		Message:   strings.TrimSpace(r.PostFormValue("message")),
	}
	if len([]rune(q.Message)) < 10 || len([]rune(q.Message)) > 5000 || len(q.Name) > 120 || len(q.Email) > 254 || (q.Email != "" && !emailPattern.MatchString(q.Email)) {
		http.Redirect(w, r, "/?error=invalid#ask", http.StatusSeeOther)
		return
	}
	if err := s.store.saveContact(q, s.clientIP(r)); err != nil {
		if !errors.Is(err, errContactLimit) {
			log.Printf("save contact message: %v", err)
		}
		http.Redirect(w, r, "/?error=unavailable#ask", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/?sent=1#ask", http.StatusSeeOther)
}

func main() {
	settings, err := loadSettings("config/.env")
	if err != nil {
		log.Fatal(err)
	}
	s, err := newServer("data")
	if err != nil {
		log.Fatal(err)
	}
	defer s.store.close()
	s.adminUsername, s.adminPassword, s.sessionSecret = settings.AdminUsername, settings.AdminPassword, []byte(settings.SessionSecret)
	s.trustedProxyCIDRs = settings.TrustedProxyCIDRs
	addr := "0.0.0.0:" + settings.Port
	log.Printf("The Honest Question listening on %s", addr)
	if err := http.ListenAndServe(addr, s.routes()); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}
