package feed

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(time.Millisecond)
	return c.t
}

type harness struct {
	t     *testing.T
	store *LocalStore
	srv   *Server
	ts    *httptest.Server
}

func newHarness(t *testing.T, limits Limits) *harness {
	t.Helper()
	ts := httptest.NewUnstartedServer(nil)
	base := "http://" + ts.Listener.Addr().String()
	store := NewMemStore(base)
	c := &clock{t: time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)}
	srv := New(Config{Store: store, Local: store.Handler(), Limits: limits, Now: c.now, XFFHops: 1, IPSecret: []byte("test"),
		Logger: slog.New(slog.DiscardHandler)})
	ts.Config.Handler = srv
	ts.Start()
	t.Cleanup(ts.Close)
	return &harness{t: t, store: store, srv: srv, ts: ts}
}

var bigLimits = Limits{Uploads: 1000, Posts: 1000, Reports: 1000}

type resp struct {
	code   int
	header http.Header
	body   []byte
}

func (h *harness) do(method, path, ip string, body any, headers ...string) resp {
	h.t.Helper()
	var r io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		r = strings.NewReader(b)
	default:
		data, _ := json.Marshal(b)
		r = bytes.NewReader(data)
	}
	req, _ := http.NewRequest(method, h.ts.URL+path, r)
	if ip != "" {
		req.Header.Set("X-Forwarded-For", ip)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	res, err := h.ts.Client().Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer res.Body.Close()
	data, _ := io.ReadAll(res.Body)
	return resp{res.StatusCode, res.Header, data}
}

func (h *harness) upload(kind, ct string, data []byte) string {
	h.t.Helper()
	r := h.do("POST", "/v1/uploads", "10.0.0.1", map[string]any{"kind": kind, "content_type": ct, "bytes": len(data)})
	if r.code != 200 {
		h.t.Fatalf("upload: %d %s", r.code, r.body)
	}
	var u uploadResponse
	json.Unmarshal(r.body, &u)
	req, _ := http.NewRequest("PUT", u.UploadURL, bytes.NewReader(data))
	req.Header.Set("Content-Type", ct)
	res, err := h.ts.Client().Do(req)
	if err != nil || res.StatusCode != 200 {
		h.t.Fatalf("PUT: %v %v", err, res.Status)
	}
	res.Body.Close()
	return u.Object
}

func (h *harness) post(handle, token, video string) resp {
	h.t.Helper()
	var hdr []string
	if token != "" {
		hdr = []string{"Authorization", "Bearer " + token}
	}
	return h.do("POST", "/v1/posts", "10.0.0.1", map[string]any{
		"handle": handle, "caption": "hi", "video_object": video, "duration_s": 3.5, "width": 720, "height": 1280,
	}, hdr...)
}

func decodeCreate(t *testing.T, r resp) createPostResponse {
	t.Helper()
	var c createPostResponse
	if err := json.Unmarshal(r.body, &c); err != nil {
		t.Fatalf("decode %s: %v", r.body, err)
	}
	return c
}

func TestHealthz(t *testing.T) {
	h := newHarness(t, bigLimits)
	r := h.do("GET", "/healthz", "", nil)
	if r.code != 200 || string(r.body) != "ok" {
		t.Fatalf("got %d %q", r.code, r.body)
	}
}

func TestClaimAndTokenFlow(t *testing.T) {
	h := newHarness(t, bigLimits)
	video := h.upload("video", "video/mp4", []byte("fake mp4"))

	first := h.post("Elliott", "", video)
	if first.code != 201 {
		t.Fatalf("first post: %d %s", first.code, first.body)
	}
	c := decodeCreate(t, first)
	if len(c.Token) != 43 {
		t.Fatalf("token %q: want 43 base64url chars", c.Token)
	}
	if c.Post.Handle != "Elliott" || c.Post.VideoURL != h.store.PublicURL(video) || c.Post.PosterURL != "" {
		t.Fatalf("post: %+v", c.Post)
	}
	if _, err := time.Parse(time.RFC3339, c.Post.CreatedAt); err != nil {
		t.Fatal(err)
	}

	rec, _, err := h.store.Get(context.Background(), "handles/elliott.json")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(rec, []byte(c.Token)) || !bytes.Contains(rec, []byte(tokenHash(c.Token))) {
		t.Fatalf("handle record must hold only the token hash: %s", rec)
	}

	tests := []struct {
		name   string
		handle string
		token  string
		code   int
	}{
		{"no token", "elliott", "", 403},
		{"wrong token", "ELLIOTT", "nope", 403},
		{"other handle's shape", "ｅｌｌｉｏｔｔ", "", 403},
		{"right token, case-folded", "ELLIOTT", c.Token, 201},
		{"right token, fullwidth", "ｅｌｌｉｏｔｔ", c.Token, 201},
		{"bad handle", "no spaces", c.Token, 400},
	}
	for _, tt := range tests {
		r := h.post(tt.handle, tt.token, video)
		if r.code != tt.code {
			t.Errorf("%s: got %d %s, want %d", tt.name, r.code, r.body, tt.code)
			continue
		}
		if r.code == 201 {
			got := decodeCreate(t, r)
			if got.Token != "" || got.Post.Handle != "Elliott" {
				t.Errorf("%s: later post must keep the claimed handle and not return a token: %+v", tt.name, got)
			}
		}
	}

	other := h.post("someone", "", video)
	if other.code != 201 || decodeCreate(t, other).Token == "" {
		t.Fatalf("second handle claim: %d %s", other.code, other.body)
	}
}

func TestPostValidation(t *testing.T) {
	h := newHarness(t, bigLimits)
	video := h.upload("video", "video/webm", []byte("webm"))
	poster := h.upload("poster", "image/jpeg", []byte("jpg"))
	h.store.Put(context.Background(), "u/"+strings.Repeat("0", 32)+".mp4", nil, "video/mp4", PutAny)
	tests := []struct {
		name string
		body map[string]any
		code int
	}{
		{"ok with poster", map[string]any{"handle": "a", "video_object": video, "poster_object": poster}, 201},
		{"missing video", map[string]any{"handle": "b", "video_object": "u/" + strings.Repeat("a", 32) + ".mp4"}, 400},
		{"empty video", map[string]any{"handle": "b", "video_object": "u/" + strings.Repeat("0", 32) + ".mp4"}, 400},
		{"not an upload", map[string]any{"handle": "b", "video_object": "handles/a.json"}, 400},
		{"poster as video", map[string]any{"handle": "b", "video_object": poster}, 400},
		{"video as poster", map[string]any{"handle": "b", "video_object": video, "poster_object": video}, 400},
		{"long caption", map[string]any{"handle": "b", "video_object": video, "caption": strings.Repeat("é", 2201)}, 400},
		{"max caption", map[string]any{"handle": "d", "video_object": video, "caption": strings.Repeat("é", 2200)}, 201},
		{"negative width", map[string]any{"handle": "c", "video_object": video, "width": -1}, 400},
	}
	for _, tt := range tests {
		r := h.do("POST", "/v1/posts", "10.0.0.2", tt.body)
		if r.code != tt.code {
			t.Errorf("%s: got %d %s, want %d", tt.name, r.code, r.body, tt.code)
		}
	}
	if _, _, err := h.store.Get(context.Background(), "handles/b.json"); err != ErrNotFound {
		t.Errorf("a rejected post must not claim its handle (err=%v)", err)
	}
}

func TestUploadLimits(t *testing.T) {
	h := newHarness(t, bigLimits)
	tests := []struct {
		kind, ct string
		bytes    int64
		code     int
	}{
		{"video", "video/mp4", 1, 200},
		{"video", "video/webm", MaxVideoBytes, 200},
		{"video", "video/quicktime", MaxVideoBytes + 1, 413},
		{"poster", "image/png", MaxPosterBytes, 200},
		{"poster", "image/webp", MaxPosterBytes + 1, 413},
		{"poster", "image/jpeg", 10, 200},
		{"video", "image/jpeg", 10, 415},
		{"poster", "video/mp4", 10, 415},
		{"video", "video/x-matroska", 10, 415},
		{"video", "application/octet-stream", 10, 415},
		{"audio", "video/mp4", 10, 400},
		{"video", "video/mp4", 0, 400},
	}
	for _, tt := range tests {
		r := h.do("POST", "/v1/uploads", "10.0.0.3", map[string]any{"kind": tt.kind, "content_type": tt.ct, "bytes": tt.bytes})
		if r.code != tt.code {
			t.Errorf("%s %s %d: got %d %s, want %d", tt.kind, tt.ct, tt.bytes, r.code, r.body, tt.code)
			continue
		}
		if r.code == 200 {
			var u uploadResponse
			json.Unmarshal(r.body, &u)
			if !reUploadObject.MatchString(u.Object) || u.PublicURL != h.store.PublicURL(u.Object) || u.UploadURL == "" {
				t.Errorf("bad upload response %+v", u)
			}
		}
	}

	big := h.do("POST", "/v1/uploads", "10.0.0.3", `{"kind":"video","pad":"`+strings.Repeat("x", MaxJSONBody)+`"}`)
	if big.code != 413 {
		t.Errorf("16 KB body limit: got %d", big.code)
	}
	bad := h.do("POST", "/v1/uploads", "10.0.0.3", `{nope`)
	if bad.code != 400 {
		t.Errorf("bad JSON: got %d", bad.code)
	}
}

func TestSignedPutIsContentTypeBound(t *testing.T) {
	h := newHarness(t, bigLimits)
	r := h.do("POST", "/v1/uploads", "", map[string]any{"kind": "poster", "content_type": "image/png", "bytes": 3})
	var u uploadResponse
	json.Unmarshal(r.body, &u)
	put := func(ct string, body []byte, rawURL string) int {
		req, _ := http.NewRequest("PUT", rawURL, bytes.NewReader(body))
		req.Header.Set("Content-Type", ct)
		res, err := h.ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}
	if c := put("image/jpeg", []byte("png"), u.UploadURL); c != 403 {
		t.Errorf("wrong content type: got %d", c)
	}
	tampered, _ := url.Parse(u.UploadURL)
	q := tampered.Query()
	q.Set("max", "999999999")
	tampered.RawQuery = q.Encode()
	if c := put("image/png", []byte("png"), tampered.String()); c != 403 {
		t.Errorf("tampered max: got %d", c)
	}
	if c := put("image/png", bytes.Repeat([]byte("x"), MaxPosterBytes+1), u.UploadURL); c != 413 {
		t.Errorf("oversize PUT: got %d", c)
	}
	if c := put("image/png", []byte("png"), u.UploadURL); c != 200 {
		t.Errorf("good PUT: got %d", c)
	}
	get := h.do("GET", "/local/"+u.Object, "", nil)
	if get.code != 200 || string(get.body) != "png" || get.header.Get("Content-Type") != "image/png" {
		t.Errorf("GET public url: %d %q %q", get.code, get.body, get.header.Get("Content-Type"))
	}
}

func TestReportHiding(t *testing.T) {
	h := newHarness(t, bigLimits)
	video := h.upload("video", "video/mp4", []byte("v"))
	c := decodeCreate(t, h.post("reported", "", video))
	id := c.Post.ID
	path := "/v1/posts/" + id + "/report"

	feedCount := func() int {
		var f feedResponse
		json.Unmarshal(h.do("GET", "/v1/feed", "", nil).body, &f)
		return len(f.Posts)
	}
	if feedCount() != 1 {
		t.Fatal("post should be in the feed")
	}

	steps := []struct {
		ip      string
		body    any
		visible int
	}{
		{"1.1.1.1", map[string]string{"reason": "spam"}, 1},
		{"1.1.1.1", map[string]string{"reason": "again"}, 1}, // same IP counts once
		{"2.2.2.2", "", 1}, // empty body allowed
		{"2001:db8::1", map[string]string{"reason": "x"}, 0}, // third distinct IP hides
		{"4.4.4.4", map[string]string{"reason": "x"}, 0},
	}
	for i, s := range steps {
		r := h.do("POST", path, s.ip, s.body)
		if r.code != 204 {
			t.Fatalf("step %d: got %d %s", i, r.code, r.body)
		}
		if got := feedCount(); got != s.visible {
			t.Fatalf("step %d: %d visible, want %d", i, got, s.visible)
		}
	}
	data, _, _ := h.store.Get(context.Background(), postObject(id))
	var p storedPost
	json.Unmarshal(data, &p)
	if !p.Hidden || len(p.Reports) != 4 {
		t.Fatalf("stored post: hidden=%v reports=%d", p.Hidden, len(p.Reports))
	}
	for _, rep := range p.Reports {
		if strings.Contains(rep.IPHash, ".") || strings.Contains(rep.IPHash, ":") {
			t.Fatalf("raw IP stored: %q", rep.IPHash)
		}
	}
	if bytes.Contains(data, []byte("1.1.1.1")) {
		t.Fatal("raw IP in post JSON")
	}

	if r := h.do("POST", "/v1/posts/0000000000000000000-0000000000000000/report", "", nil); r.code != 404 {
		t.Errorf("missing post: %d", r.code)
	}
	if r := h.do("POST", "/v1/posts/..%2Fhandles%2Fx/report", "", nil); r.code != 404 {
		t.Errorf("bad id: %d", r.code)
	}
}

func TestIPv6SameSubnetIsOneReporter(t *testing.T) {
	h := newHarness(t, bigLimits)
	video := h.upload("video", "video/mp4", []byte("v"))
	id := decodeCreate(t, h.post("v6", "", video)).Post.ID
	for i := 1; i <= 5; i++ {
		h.do("POST", "/v1/posts/"+id+"/report", fmt.Sprintf("2001:db8:1:2::%x", i), nil)
	}
	data, _, _ := h.store.Get(context.Background(), postObject(id))
	var p storedPost
	json.Unmarshal(data, &p)
	if p.Hidden || len(p.Reports) != 1 {
		t.Fatalf("one /64 should count once: hidden=%v reports=%d", p.Hidden, len(p.Reports))
	}
}

func TestCursorPaging(t *testing.T) {
	h := newHarness(t, bigLimits)
	video := h.upload("video", "video/mp4", []byte("v"))
	var ids []string
	var token string
	for i := 0; i < 7; i++ {
		c := decodeCreate(t, h.post("pager", token, video))
		if token == "" {
			token = c.Token
		}
		ids = append(ids, c.Post.ID)
	}
	// Hide the 3rd-newest post.
	hidden := ids[4]
	for _, ip := range []string{"1.0.0.1", "1.0.0.2", "1.0.0.3"} {
		h.do("POST", "/v1/posts/"+hidden+"/report", ip, nil)
	}
	var want []string
	for i := len(ids) - 1; i >= 0; i-- {
		if ids[i] != hidden {
			want = append(want, ids[i])
		}
	}

	for _, limit := range []int{1, 2, 3, 6, 20} {
		var got []string
		cursor := ""
		for pages := 0; pages < 20; pages++ {
			r := h.do("GET", fmt.Sprintf("/v1/feed?limit=%d&before=%s", limit, url.QueryEscape(cursor)), "", nil)
			if r.code != 200 || r.header.Get("Cache-Control") != "public, max-age=5" {
				t.Fatalf("feed: %d %q", r.code, r.header.Get("Cache-Control"))
			}
			var f feedResponse
			json.Unmarshal(r.body, &f)
			if len(f.Posts) > limit {
				t.Fatalf("limit %d: page has %d posts", limit, len(f.Posts))
			}
			for _, p := range f.Posts {
				got = append(got, p.ID)
			}
			if f.Next == "" {
				break
			}
			cursor = f.Next
		}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("limit %d:\n got %v\nwant %v", limit, got, want)
		}
	}

	tests := map[string]int{
		"/v1/feed?limit=0":            400,
		"/v1/feed?limit=abc":          400,
		"/v1/feed?limit=999":          200,
		"/v1/feed?before=handles%2Fx": 400,
		"/v1/feed?before=posts%2F..":  400,
	}
	for path, code := range tests {
		if r := h.do("GET", path, "", nil); r.code != code {
			t.Errorf("%s: got %d, want %d", path, r.code, code)
		}
	}

	var empty map[string]json.RawMessage
	json.Unmarshal(newHarness(t, bigLimits).do("GET", "/v1/feed", "", nil).body, &empty)
	if string(empty["posts"]) != "[]" || string(empty["next"]) != `""` {
		t.Errorf("empty feed: %s %s", empty["posts"], empty["next"])
	}
}

func TestCORS(t *testing.T) {
	h := newHarness(t, bigLimits)
	tests := []struct {
		method, origin string
		preflight      bool
		code           int
		allow          string
	}{
		{"OPTIONS", "https://khensu.nu", true, 204, "https://khensu.nu"},
		{"OPTIONS", "https://waves.khensu.nu", true, 204, "https://waves.khensu.nu"},
		{"OPTIONS", "http://localhost:4321", true, 204, "http://localhost:4321"},
		{"OPTIONS", "http://127.0.0.1:4321", true, 204, "http://127.0.0.1:4321"},
		{"OPTIONS", "https://evil.example", true, 403, ""},
		{"GET", "https://moments.khensu.nu", false, 200, "https://moments.khensu.nu"},
		{"GET", "https://evil.example", false, 200, ""},
	}
	for _, tt := range tests {
		hdr := []string{"Origin", tt.origin}
		if tt.preflight {
			hdr = append(hdr, "Access-Control-Request-Method", "POST", "Access-Control-Request-Headers", "content-type, authorization")
		}
		r := h.do(tt.method, "/v1/feed", "", nil, hdr...)
		if r.code != tt.code || r.header.Get("Access-Control-Allow-Origin") != tt.allow {
			t.Errorf("%s %s: got %d %q", tt.method, tt.origin, r.code, r.header.Get("Access-Control-Allow-Origin"))
		}
		if tt.preflight && tt.code == 204 {
			if got := r.header.Get("Access-Control-Allow-Headers"); got != "content-type, authorization" {
				t.Errorf("Allow-Headers %q", got)
			}
			if !strings.Contains(r.header.Get("Access-Control-Allow-Methods"), "POST") {
				t.Errorf("Allow-Methods %q", r.header.Get("Access-Control-Allow-Methods"))
			}
		}
		if !strings.Contains(strings.Join(r.header.Values("Vary"), ","), "Origin") {
			t.Errorf("%s %s: missing Vary: Origin", tt.method, tt.origin)
		}
	}
}

func TestRateLimits(t *testing.T) {
	h := newHarness(t, Limits{Uploads: 2, Posts: 1, Reports: 1})
	body := map[string]any{"kind": "video", "content_type": "video/mp4", "bytes": 1}
	codes := []int{}
	for i := 0; i < 3; i++ {
		codes = append(codes, h.do("POST", "/v1/uploads", "9.9.9.9", body).code)
	}
	if fmt.Sprint(codes) != "[200 200 429]" {
		t.Fatalf("uploads: %v", codes)
	}
	if r := h.do("POST", "/v1/uploads", "9.9.9.9", body); r.header.Get("Retry-After") == "" {
		t.Error("missing Retry-After")
	}
	if r := h.do("POST", "/v1/uploads", "8.8.8.8", body); r.code != 200 {
		t.Errorf("other IP limited: %d", r.code)
	}
}
