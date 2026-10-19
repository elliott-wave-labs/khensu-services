package feed

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"cloud.google.com/go/storage"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"
)

// GCSStore keeps objects in a Google Cloud Storage bucket. Signed URLs use the
// service account's private key when the credentials carry one, otherwise the
// IAM Credentials signBlob API (the service account needs
// roles/iam.serviceAccountTokenCreator on itself).
type GCSStore struct {
	client *storage.Client
	bucket *storage.BucketHandle
	name   string
}

func NewGCSStore(ctx context.Context, bucket string) (*GCSStore, error) {
	c, err := storage.NewClient(ctx)
	if err != nil {
		return nil, err
	}
	return &GCSStore{client: c, bucket: c.Bucket(bucket), name: bucket}, nil
}

func (g *GCSStore) Close() error { return g.client.Close() }

func (g *GCSStore) Get(ctx context.Context, name string) ([]byte, int64, error) {
	r, err := g.bucket.Object(name).NewReader(ctx)
	if errors.Is(err, storage.ErrObjectNotExist) {
		return nil, 0, ErrNotFound
	}
	if err != nil {
		return nil, 0, err
	}
	defer r.Close()
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, 0, err
	}
	return data, r.Attrs.Generation, nil
}

func (g *GCSStore) Put(ctx context.Context, name string, data []byte, contentType string, ifGen int64) error {
	obj := g.bucket.Object(name)
	switch {
	case ifGen == 0:
		obj = obj.If(storage.Conditions{DoesNotExist: true})
	case ifGen > 0:
		obj = obj.If(storage.Conditions{GenerationMatch: ifGen})
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	w := obj.NewWriter(ctx)
	w.ContentType = contentType
	w.CacheControl = "no-cache"
	if _, err := w.Write(data); err != nil {
		return mapGCSErr(err)
	}
	return mapGCSErr(w.Close())
}

func mapGCSErr(err error) error {
	var gerr *googleapi.Error
	if errors.As(err, &gerr) && gerr.Code == http.StatusPreconditionFailed {
		return ErrPrecondition
	}
	return err
}

func (g *GCSStore) Delete(ctx context.Context, name string) error {
	err := g.bucket.Object(name).Delete(ctx)
	if errors.Is(err, storage.ErrObjectNotExist) {
		return ErrNotFound
	}
	return err
}

func (g *GCSStore) Stat(ctx context.Context, name string) (ObjectInfo, error) {
	a, err := g.bucket.Object(name).Attrs(ctx)
	if errors.Is(err, storage.ErrObjectNotExist) {
		return ObjectInfo{}, ErrNotFound
	}
	if err != nil {
		return ObjectInfo{}, err
	}
	return ObjectInfo{Name: a.Name, Size: a.Size, ContentType: a.ContentType}, nil
}

func (g *GCSStore) List(ctx context.Context, prefix, startAfter string, limit int) ([]string, error) {
	q := &storage.Query{Prefix: prefix, StartOffset: startAfter}
	if err := q.SetAttrSelection([]string{"Name"}); err != nil {
		return nil, err
	}
	it := g.bucket.Objects(ctx, q)
	var names []string
	for len(names) < limit {
		a, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return nil, err
		}
		if a.Name == startAfter {
			continue
		}
		names = append(names, a.Name)
	}
	return names, nil
}

func (g *GCSStore) SignedPutURL(ctx context.Context, name, contentType string, maxBytes int64, ttl time.Duration) (string, error) {
	return g.bucket.SignedURL(name, &storage.SignedURLOptions{
		Scheme:      storage.SigningSchemeV4,
		Method:      http.MethodPut,
		ContentType: contentType,
		Expires:     time.Now().Add(ttl),
	})
}

func (g *GCSStore) PublicURL(name string) string {
	return "https://storage.googleapis.com/" + g.name + "/" + escapeObject(name)
}

func escapeObject(name string) string {
	parts := strings.Split(name, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}
