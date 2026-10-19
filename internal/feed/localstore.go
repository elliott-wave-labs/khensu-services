package feed

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"hash/fnv"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// LocalStore keeps objects in memory (dir == "") or under a directory, for
// tests and local runs. Its Handler serves PUT /local/<object> for signed
// uploads and GET /local/<object> as the public URL, so a site on localhost
// can run the whole upload flow.
type LocalStore struct {
	mu   sync.Mutex
	dir  string
	mem  map[string][]byte
	base string
	key  []byte
	now  func() time.Time
}

// NewMemStore keeps objects in memory. base is the server's origin, such as
// http://127.0.0.1:8787.
func NewMemStore(base string) *LocalStore {
	return newLocalStore("", base)
}

func NewDirStore(dir, base string) (*LocalStore, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return newLocalStore(dir, base), nil
}

func newLocalStore(dir, base string) *LocalStore {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		panic(err)
	}
	return &LocalStore{dir: dir, mem: map[string][]byte{}, base: strings.TrimRight(base, "/"), key: key, now: time.Now}
}

func checkName(name string) error {
	if name == "" || strings.ContainsAny(name, "\\\x00") || strings.HasPrefix(name, "/") {
		return errors.New("feed: bad object name")
	}
	for _, seg := range strings.Split(name, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return errors.New("feed: bad object name")
		}
	}
	return nil
}

func (l *LocalStore) path(name string) string { return filepath.Join(l.dir, filepath.FromSlash(name)) }

func (l *LocalStore) read(name string) ([]byte, error) {
	if err := checkName(name); err != nil {
		return nil, ErrNotFound
	}
	if l.dir == "" {
		d, ok := l.mem[name]
		if !ok {
			return nil, ErrNotFound
		}
		return d, nil
	}
	d, err := os.ReadFile(l.path(name))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotFound
	}
	return d, err
}

func (l *LocalStore) write(name string, data []byte) error {
	if err := checkName(name); err != nil {
		return err
	}
	if l.dir == "" {
		l.mem[name] = bytes.Clone(data)
		return nil
	}
	p := l.path(name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

func generation(data []byte) int64 {
	h := fnv.New64a()
	h.Write(data)
	return int64(h.Sum64()>>1) | 1
}

func (l *LocalStore) Get(ctx context.Context, name string) ([]byte, int64, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	d, err := l.read(name)
	if err != nil {
		return nil, 0, err
	}
	return d, generation(d), nil
}

func (l *LocalStore) Put(ctx context.Context, name string, data []byte, contentType string, ifGen int64) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if ifGen != PutAny {
		cur, err := l.read(name)
		switch {
		case errors.Is(err, ErrNotFound):
			if ifGen != 0 {
				return ErrPrecondition
			}
		case err != nil:
			return err
		case ifGen == 0 || generation(cur) != ifGen:
			return ErrPrecondition
		}
	}
	return l.write(name, data)
}

func (l *LocalStore) Delete(ctx context.Context, name string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, err := l.read(name); err != nil {
		return err
	}
	if l.dir == "" {
		delete(l.mem, name)
		return nil
	}
	return os.Remove(l.path(name))
}

func (l *LocalStore) Stat(ctx context.Context, name string) (ObjectInfo, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	d, err := l.read(name)
	if err != nil {
		return ObjectInfo{}, err
	}
	return ObjectInfo{Name: name, Size: int64(len(d)), ContentType: contentTypeFor(name)}, nil
}

func (l *LocalStore) List(ctx context.Context, prefix, startAfter string, limit int) ([]string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	var all []string
	if l.dir == "" {
		for n := range l.mem {
			all = append(all, n)
		}
	} else {
		err := filepath.WalkDir(l.dir, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || strings.HasSuffix(p, ".tmp") {
				return err
			}
			rel, err := filepath.Rel(l.dir, p)
			if err != nil {
				return err
			}
			all = append(all, filepath.ToSlash(rel))
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	sort.Strings(all)
	var out []string
	for _, n := range all {
		if len(out) >= limit {
			break
		}
		if strings.HasPrefix(n, prefix) && n > startAfter {
			out = append(out, n)
		}
	}
	return out, nil
}

func (l *LocalStore) sign(name, contentType, exp, max string) string {
	m := hmac.New(sha256.New, l.key)
	io.WriteString(m, name+"\n"+contentType+"\n"+exp+"\n"+max)
	return hex.EncodeToString(m.Sum(nil))
}

func (l *LocalStore) SignedPutURL(ctx context.Context, name, contentType string, maxBytes int64, ttl time.Duration) (string, error) {
	if err := checkName(name); err != nil {
		return "", err
	}
	exp := strconv.FormatInt(l.now().Add(ttl).Unix(), 10)
	max := strconv.FormatInt(maxBytes, 10)
	return l.base + "/local/" + escapeObject(name) + "?exp=" + exp + "&max=" + max + "&sig=" + l.sign(name, contentType, exp, max), nil
}

func (l *LocalStore) PublicURL(name string) string {
	return l.base + "/local/" + escapeObject(name)
}

func contentTypeFor(name string) string {
	switch path.Ext(name) {
	case ".mp4":
		return "video/mp4"
	case ".webm":
		return "video/webm"
	case ".mov":
		return "video/quicktime"
	case ".jpg":
		return "image/jpeg"
	case ".png":
		return "image/png"
	case ".webp":
		return "image/webp"
	case ".json":
		return "application/json"
	}
	if t := mime.TypeByExtension(path.Ext(name)); t != "" {
		return t
	}
	return "application/octet-stream"
}

// Handler serves /local/<object>.
func (l *LocalStore) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/local/")
		if checkName(name) != nil {
			http.NotFound(w, r)
			return
		}
		switch r.Method {
		case http.MethodGet, http.MethodHead:
			l.mu.Lock()
			d, err := l.read(name)
			l.mu.Unlock()
			if err != nil {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", contentTypeFor(name))
			http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(d))
		case http.MethodPut:
			q := r.URL.Query()
			exp, max, sig := q.Get("exp"), q.Get("max"), q.Get("sig")
			want := l.sign(name, r.Header.Get("Content-Type"), exp, max)
			if !hmac.Equal([]byte(sig), []byte(want)) {
				http.Error(w, "signature does not match", http.StatusForbidden)
				return
			}
			e, err1 := strconv.ParseInt(exp, 10, 64)
			m, err2 := strconv.ParseInt(max, 10, 64)
			if err1 != nil || err2 != nil || l.now().Unix() > e {
				http.Error(w, "url expired", http.StatusForbidden)
				return
			}
			data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, m))
			if err != nil {
				http.Error(w, "too large", http.StatusRequestEntityTooLarge)
				return
			}
			if err := l.Put(r.Context(), name, data, r.Header.Get("Content-Type"), PutAny); err != nil {
				http.Error(w, "write failed", http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusOK)
		default:
			w.Header().Set("Allow", "GET, HEAD, PUT")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
}
