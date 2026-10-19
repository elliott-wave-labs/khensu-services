package feed

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	MaxVideoBytes  = 150 << 20
	MaxPosterBytes = 5 << 20
	MaxJSONBody    = 16 << 10
	MaxCaption     = 2200
	MaxReason      = 500
	UploadTTL      = 15 * time.Minute
	HideReports    = 3

	DefaultFeedLimit = 20
	MaxFeedLimit     = 50
	maxFeedScan      = 500
	feedCacheTTL     = 5 * time.Second
)

var uploadTypes = map[string]struct{ kind, ext string }{
	"video/mp4":       {"video", "mp4"},
	"video/webm":      {"video", "webm"},
	"video/quicktime": {"video", "mov"},
	"image/jpeg":      {"poster", "jpg"},
	"image/png":       {"poster", "png"},
	"image/webp":      {"poster", "webp"},
}

var (
	reUploadObject = regexp.MustCompile(`^u/[0-9a-f]{32}\.(mp4|webm|mov|jpg|png|webp)$`)
	rePostID       = regexp.MustCompile(`^[0-9]{19}-[0-9a-f]{16}$`)
	reCursor       = regexp.MustCompile(`^posts/[0-9]{19}-[0-9a-f]{16}\.json$`)
)

// Post is the public shape of a post.
type Post struct {
	ID        string  `json:"id"`
	Handle    string  `json:"handle"`
	Caption   string  `json:"caption"`
	VideoURL  string  `json:"video_url"`
	PosterURL string  `json:"poster_url"`
	DurationS float64 `json:"duration_s"`
	Width     int     `json:"width"`
	Height    int     `json:"height"`
	CreatedAt string  `json:"created_at"`
}

type report struct {
	IPHash string    `json:"ip_hash"`
	Reason string    `json:"reason,omitempty"`
	At     time.Time `json:"at"`
}

type storedPost struct {
	ID           string    `json:"id"`
	Handle       string    `json:"handle"`
	HandleKey    string    `json:"handle_key"`
	Caption      string    `json:"caption"`
	VideoObject  string    `json:"video_object"`
	PosterObject string    `json:"poster_object,omitempty"`
	DurationS    float64   `json:"duration_s"`
	Width        int       `json:"width"`
	Height       int       `json:"height"`
	CreatedAt    time.Time `json:"created_at"`
	Hidden       bool      `json:"hidden"`
	Reports      []report  `json:"reports,omitempty"`
}

type handleRecord struct {
	Handle      string    `json:"handle"`
	Key         string    `json:"key"`
	TokenSHA256 string    `json:"token_sha256"`
	CreatedAt   time.Time `json:"created_at"`
}

type Config struct {
	Store Store
	// IPSecret derives the daily salt for IP hashes. It must be the same on
	// every instance and must not live in the public bucket.
	IPSecret []byte
	// XFFHops > 0 takes the client IP from the Nth X-Forwarded-For entry
	// counted from the right (1 on Cloud Run). 0 uses the TCP peer.
	XFFHops int
	Limits  Limits
	// Local, when set, is mounted at /local/ (dir and mem stores).
	Local  http.Handler
	Now    func() time.Time
	Logger *slog.Logger
}

type Server struct {
	store   Store
	secret  []byte
	hops    int
	limiter *limiter
	now     func() time.Time
	log     *slog.Logger
	handler http.Handler

	cacheMu sync.Mutex
	cache   map[string]cachedFeed
}

type cachedFeed struct {
	body []byte
	at   time.Time
}

func New(cfg Config) *Server {
	s := &Server{
		store:  cfg.Store,
		secret: cfg.IPSecret,
		hops:   cfg.XFFHops,
		now:    cfg.Now,
		log:    cfg.Logger,
		cache:  map[string]cachedFeed{},
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	if len(s.secret) == 0 {
		s.secret = make([]byte, 32)
		rand.Read(s.secret)
	}
	limits := cfg.Limits
	if limits == (Limits{}) {
		limits = DefaultLimits
	}
	s.limiter = newLimiter(limits, s.now)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		io.WriteString(w, "ok")
	})
	mux.HandleFunc("GET /v1/feed", s.handleFeed)
	mux.HandleFunc("POST /v1/uploads", s.handleUpload)
	mux.HandleFunc("POST /v1/posts", s.handleCreatePost)
	mux.HandleFunc("POST /v1/posts/{id}/report", s.handleReport)
	if cfg.Local != nil {
		mux.Handle("/local/", cfg.Local)
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "not found")
	})
	s.handler = s.logged(cors(mux))
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.handler.ServeHTTP(w, r) }

// AllowedOrigin reports whether a browser origin may call the API.
func AllowedOrigin(origin string) bool {
	switch origin {
	case "https://khensu.nu", "http://127.0.0.1:4321", "http://localhost:4321":
		return true
	}
	u, err := url.Parse(origin)
	if err != nil || u.Scheme != "https" || u.Path != "" || u.RawQuery != "" || u.User != nil || u.Port() != "" {
		return false
	}
	sub, ok := strings.CutSuffix(u.Host, ".khensu.nu")
	if !ok || sub == "" {
		return false
	}
	for _, label := range strings.Split(sub, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	return true
}

func cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Add("Vary", "Origin")
		h.Set("X-Content-Type-Options", "nosniff")
		origin := r.Header.Get("Origin")
		allowed := origin != "" && AllowedOrigin(origin)
		if allowed {
			h.Set("Access-Control-Allow-Origin", origin)
		}
		if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
			if !allowed {
				writeError(w, http.StatusForbidden, "origin not allowed")
				return
			}
			h.Set("Access-Control-Allow-Methods", "GET, HEAD, POST, PUT, OPTIONS")
			h.Set("Access-Control-Allow-Headers", "content-type, authorization")
			h.Set("Access-Control-Max-Age", "600")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

// logged logs method, path and status only: never the client IP.
func (s *Server) logged(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(sw, r)
		s.log.Info("request", "method", r.Method, "path", r.URL.Path, "status", sw.status, "ms", time.Since(start).Milliseconds())
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// decode reads a JSON body of at most MaxJSONBody bytes. It writes the error
// response itself and returns false on failure.
func decode(w http.ResponseWriter, r *http.Request, v any, allowEmpty bool) bool {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxJSONBody))
	var maxErr *http.MaxBytesError
	if errors.As(err, &maxErr) {
		writeError(w, http.StatusRequestEntityTooLarge, "body too large")
		return false
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "could not read body")
		return false
	}
	if allowEmpty && len(strings.TrimSpace(string(body))) == 0 {
		return true
	}
	if err := json.Unmarshal(body, v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return false
	}
	return true
}

func (s *Server) clientIP(r *http.Request) string {
	raw := ""
	if s.hops > 0 {
		var parts []string
		for _, v := range r.Header.Values("X-Forwarded-For") {
			for _, p := range strings.Split(v, ",") {
				if p = strings.TrimSpace(p); p != "" {
					parts = append(parts, p)
				}
			}
		}
		if len(parts) >= s.hops {
			raw = parts[len(parts)-s.hops]
		}
	}
	if raw == "" {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			host = r.RemoteAddr
		}
		raw = host
	}
	ip := net.ParseIP(raw)
	if ip == nil {
		return raw
	}
	if v4 := ip.To4(); v4 != nil {
		return v4.String()
	}
	// One IPv6 /64 is one subscriber.
	return ip.Mask(net.CIDRMask(64, 128)).String()
}

// ipHash hashes the client IP with a salt that changes every UTC day.
func (s *Server) ipHash(r *http.Request) string {
	day := hmac.New(sha256.New, s.secret)
	io.WriteString(day, s.now().UTC().Format("2006-01-02"))
	m := hmac.New(sha256.New, day.Sum(nil))
	io.WriteString(m, s.clientIP(r))
	return hex.EncodeToString(m.Sum(nil)[:16])
}

func (s *Server) rateLimited(w http.ResponseWriter, class, ipHash string) bool {
	ok, wait := s.limiter.allow(class, ipHash)
	if ok {
		return false
	}
	w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(wait.Seconds()))))
	writeError(w, http.StatusTooManyRequests, "too many "+class+", try again later")
	return true
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func (s *Server) newPostID() string {
	return strconv.FormatInt(math.MaxInt64-s.now().UnixNano(), 10) + "-" + randHex(8)
}

func postObject(id string) string { return "posts/" + id + ".json" }

func (s *Server) public(p *storedPost) Post {
	out := Post{
		ID:        p.ID,
		Handle:    p.Handle,
		Caption:   p.Caption,
		VideoURL:  s.store.PublicURL(p.VideoObject),
		DurationS: p.DurationS,
		Width:     p.Width,
		Height:    p.Height,
		CreatedAt: p.CreatedAt.UTC().Format(time.RFC3339),
	}
	if p.PosterObject != "" {
		out.PosterURL = s.store.PublicURL(p.PosterObject)
	}
	return out
}

// GET /v1/feed?limit=20&before=<cursor>

type feedResponse struct {
	Posts []Post `json:"posts"`
	Next  string `json:"next"`
}

func (s *Server) handleFeed(w http.ResponseWriter, r *http.Request) {
	limit := DefaultFeedLimit
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			writeError(w, http.StatusBadRequest, "limit must be a positive integer")
			return
		}
		limit = min(n, MaxFeedLimit)
	}
	before := r.URL.Query().Get("before")
	if before != "" && !reCursor.MatchString(before) {
		writeError(w, http.StatusBadRequest, "invalid cursor")
		return
	}
	key := strconv.Itoa(limit) + "|" + before
	s.cacheMu.Lock()
	c, hit := s.cache[key]
	s.cacheMu.Unlock()
	if !hit || s.now().Sub(c.at) > feedCacheTTL {
		resp, err := s.buildFeed(r.Context(), limit, before)
		if err != nil {
			s.log.Error("feed", "err", err)
			writeError(w, http.StatusInternalServerError, "feed unavailable")
			return
		}
		body, _ := json.Marshal(resp)
		c = cachedFeed{body: body, at: s.now()}
		s.cacheMu.Lock()
		if len(s.cache) > 1000 {
			clear(s.cache)
		}
		s.cache[key] = c
		s.cacheMu.Unlock()
	}
	w.Header().Set("Cache-Control", "public, max-age=5")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Write(c.body)
	w.Write([]byte("\n"))
}

func (s *Server) invalidateFeed() {
	s.cacheMu.Lock()
	clear(s.cache)
	s.cacheMu.Unlock()
}

func (s *Server) buildFeed(ctx context.Context, limit int, before string) (feedResponse, error) {
	resp := feedResponse{Posts: []Post{}}
	cursor := before
	scanned := 0
	for {
		batch := limit - len(resp.Posts) + 1
		names, err := s.store.List(ctx, "posts/", cursor, batch)
		if err != nil {
			return resp, err
		}
		posts := s.loadPosts(ctx, names)
		for i, name := range names {
			scanned++
			cursor = name
			if p := posts[i]; p != nil && !p.Hidden {
				if len(resp.Posts) == limit {
					// A further visible post exists; page ends at the previous one.
					return resp, nil
				}
				resp.Posts = append(resp.Posts, s.public(p))
				resp.Next = name
			}
			if scanned >= maxFeedScan {
				resp.Next = name
				return resp, nil
			}
		}
		if len(names) < batch {
			resp.Next = ""
			return resp, nil
		}
	}
}

func (s *Server) loadPosts(ctx context.Context, names []string) []*storedPost {
	out := make([]*storedPost, len(names))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 16)
	for i, name := range names {
		if !reCursor.MatchString(name) {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			data, _, err := s.store.Get(ctx, name)
			if err != nil {
				return
			}
			var p storedPost
			if json.Unmarshal(data, &p) == nil && p.ID != "" {
				out[i] = &p
			}
		}()
	}
	wg.Wait()
	return out
}

// POST /v1/uploads

type uploadRequest struct {
	Kind        string `json:"kind"`
	ContentType string `json:"content_type"`
	Bytes       int64  `json:"bytes"`
}

type uploadResponse struct {
	Object    string `json:"object"`
	UploadURL string `json:"upload_url"`
	PublicURL string `json:"public_url"`
}

func maxBytesFor(kind string) int64 {
	if kind == "video" {
		return MaxVideoBytes
	}
	return MaxPosterBytes
}

func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	if s.rateLimited(w, "uploads", s.ipHash(r)) {
		return
	}
	var req uploadRequest
	if !decode(w, r, &req, false) {
		return
	}
	if req.Kind != "video" && req.Kind != "poster" {
		writeError(w, http.StatusBadRequest, `kind must be "video" or "poster"`)
		return
	}
	t, ok := uploadTypes[req.ContentType]
	if !ok || t.kind != req.Kind {
		writeError(w, http.StatusUnsupportedMediaType, "unsupported content_type for "+req.Kind)
		return
	}
	if req.Bytes <= 0 {
		writeError(w, http.StatusBadRequest, "bytes must be positive")
		return
	}
	max := maxBytesFor(req.Kind)
	if req.Bytes > max {
		writeError(w, http.StatusRequestEntityTooLarge, req.Kind+" too large")
		return
	}
	obj := "u/" + randHex(16) + "." + t.ext
	u, err := s.store.SignedPutURL(r.Context(), obj, req.ContentType, max, UploadTTL)
	if err != nil {
		s.log.Error("sign upload", "err", err)
		writeError(w, http.StatusInternalServerError, "could not sign upload")
		return
	}
	writeJSON(w, http.StatusOK, uploadResponse{Object: obj, UploadURL: u, PublicURL: s.store.PublicURL(obj)})
}

// POST /v1/posts

type createPostRequest struct {
	Handle       string  `json:"handle"`
	Caption      string  `json:"caption"`
	VideoObject  string  `json:"video_object"`
	PosterObject string  `json:"poster_object"`
	DurationS    float64 `json:"duration_s"`
	Width        int     `json:"width"`
	Height       int     `json:"height"`
}

type createPostResponse struct {
	Post  Post   `json:"post"`
	Token string `json:"token,omitempty"`
}

func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if len(h) > 7 && strings.EqualFold(h[:7], "bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return ""
}

func tokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// checkObject verifies an uploaded object exists, is of the expected kind
// and is within its size limit. It returns a client-facing error message.
func (s *Server) checkObject(ctx context.Context, name, kind string) (string, error) {
	if !reUploadObject.MatchString(name) {
		return kind + "_object is not an upload object", nil
	}
	t := uploadTypes[contentTypeFor(name)]
	if t.kind != kind {
		return kind + "_object has the wrong type", nil
	}
	info, err := s.store.Stat(ctx, name)
	if errors.Is(err, ErrNotFound) {
		return kind + "_object not found; upload it first", nil
	}
	if err != nil {
		return "", err
	}
	if info.Size <= 0 || info.Size > maxBytesFor(kind) {
		return kind + "_object is empty or too large", nil
	}
	if ct, _, _ := strings.Cut(info.ContentType, ";"); ct != "" && uploadTypes[ct].kind != kind {
		return kind + "_object has the wrong content type", nil
	}
	return "", nil
}

func (s *Server) handleCreatePost(w http.ResponseWriter, r *http.Request) {
	if s.rateLimited(w, "posts", s.ipHash(r)) {
		return
	}
	var req createPostRequest
	if !decode(w, r, &req, false) {
		return
	}
	display, key, err := NormalizeHandle(req.Handle)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !utf8.ValidString(req.Caption) || utf8.RuneCountInString(req.Caption) > MaxCaption {
		writeError(w, http.StatusBadRequest, "caption must be valid UTF-8 of at most 2200 characters")
		return
	}
	if req.DurationS < 0 || req.DurationS > 3600 || math.IsNaN(req.DurationS) ||
		req.Width < 0 || req.Width > 16384 || req.Height < 0 || req.Height > 16384 {
		writeError(w, http.StatusBadRequest, "duration_s, width or height out of range")
		return
	}
	ctx := r.Context()
	if msg, err := s.checkObject(ctx, req.VideoObject, "video"); err != nil || msg != "" {
		s.objectError(w, msg, err)
		return
	}
	if req.PosterObject != "" {
		if msg, err := s.checkObject(ctx, req.PosterObject, "poster"); err != nil || msg != "" {
			s.objectError(w, msg, err)
			return
		}
	}

	handleObj := "handles/" + key + ".json"
	var token string
	data, _, err := s.store.Get(ctx, handleObj)
	switch {
	case err == nil:
		var rec handleRecord
		if err := json.Unmarshal(data, &rec); err != nil {
			s.log.Error("handle record", "err", err)
			writeError(w, http.StatusInternalServerError, "handle record unreadable")
			return
		}
		got := tokenHash(bearer(r))
		if bearer(r) == "" || subtle.ConstantTimeCompare([]byte(got), []byte(rec.TokenSHA256)) != 1 {
			writeError(w, http.StatusForbidden, "handle is claimed; send its token as Authorization: Bearer")
			return
		}
		display = rec.Handle
	case errors.Is(err, ErrNotFound):
		raw := make([]byte, 32)
		rand.Read(raw)
		token = base64.RawURLEncoding.EncodeToString(raw)
		rec, _ := json.Marshal(handleRecord{Handle: display, Key: key, TokenSHA256: tokenHash(token), CreatedAt: s.now().UTC()})
		err := s.store.Put(ctx, handleObj, rec, "application/json", 0)
		if errors.Is(err, ErrPrecondition) {
			writeError(w, http.StatusForbidden, "handle was just claimed by someone else")
			return
		}
		if err != nil {
			s.log.Error("claim handle", "err", err)
			writeError(w, http.StatusInternalServerError, "could not claim handle")
			return
		}
	default:
		s.log.Error("read handle", "err", err)
		writeError(w, http.StatusInternalServerError, "could not read handle")
		return
	}

	p := storedPost{
		ID:           s.newPostID(),
		Handle:       display,
		HandleKey:    key,
		Caption:      req.Caption,
		VideoObject:  req.VideoObject,
		PosterObject: req.PosterObject,
		DurationS:    req.DurationS,
		Width:        req.Width,
		Height:       req.Height,
		CreatedAt:    s.now().UTC().Truncate(time.Second),
	}
	body, _ := json.Marshal(p)
	if err := s.store.Put(ctx, postObject(p.ID), body, "application/json", 0); err != nil {
		s.log.Error("write post", "err", err)
		if token != "" {
			s.store.Delete(context.WithoutCancel(ctx), handleObj)
		}
		writeError(w, http.StatusInternalServerError, "could not save post")
		return
	}
	s.invalidateFeed()
	writeJSON(w, http.StatusCreated, createPostResponse{Post: s.public(&p), Token: token})
}

func (s *Server) objectError(w http.ResponseWriter, msg string, err error) {
	if err != nil {
		s.log.Error("stat object", "err", err)
		writeError(w, http.StatusInternalServerError, "could not check upload")
		return
	}
	writeError(w, http.StatusBadRequest, msg)
}

// POST /v1/posts/{id}/report

type reportRequest struct {
	Reason string `json:"reason"`
}

func (s *Server) handleReport(w http.ResponseWriter, r *http.Request) {
	ipHash := s.ipHash(r)
	if s.rateLimited(w, "reports", ipHash) {
		return
	}
	id := r.PathValue("id")
	if !rePostID.MatchString(id) {
		writeError(w, http.StatusNotFound, "no such post")
		return
	}
	var req reportRequest
	if !decode(w, r, &req, true) {
		return
	}
	reason := strings.ToValidUTF8(req.Reason, "")
	if utf8.RuneCountInString(reason) > MaxReason {
		reason = string([]rune(reason)[:MaxReason])
	}
	ctx := r.Context()
	for attempt := 0; attempt < 8; attempt++ {
		data, gen, err := s.store.Get(ctx, postObject(id))
		if errors.Is(err, ErrNotFound) {
			writeError(w, http.StatusNotFound, "no such post")
			return
		}
		if err != nil {
			s.log.Error("read post", "err", err)
			writeError(w, http.StatusInternalServerError, "could not read post")
			return
		}
		var p storedPost
		if err := json.Unmarshal(data, &p); err != nil {
			writeError(w, http.StatusInternalServerError, "post unreadable")
			return
		}
		for _, rep := range p.Reports {
			if rep.IPHash == ipHash {
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
		p.Reports = append(p.Reports, report{IPHash: ipHash, Reason: reason, At: s.now().UTC().Truncate(time.Second)})
		if len(p.Reports) >= HideReports {
			p.Hidden = true
		}
		body, _ := json.Marshal(p)
		err = s.store.Put(ctx, postObject(id), body, "application/json", gen)
		if errors.Is(err, ErrPrecondition) {
			continue
		}
		if err != nil {
			s.log.Error("write report", "err", err)
			writeError(w, http.StatusInternalServerError, "could not save report")
			return
		}
		if p.Hidden {
			s.invalidateFeed()
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeError(w, http.StatusServiceUnavailable, "post is busy, try again")
}
