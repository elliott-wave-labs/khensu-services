package feed

import (
	"context"
	"errors"
	"time"
)

var (
	ErrNotFound     = errors.New("feed: object not found")
	ErrPrecondition = errors.New("feed: precondition failed")
)

// PutAny makes Put unconditional. Generation 0 means the object must not exist.
const PutAny int64 = -1

type ObjectInfo struct {
	Name        string
	Size        int64
	ContentType string
}

// Store is a flat object store. Everything written to it is publicly readable
// in production (the bucket grants allUsers objectViewer), so never put
// secrets or raw IPs in it.
type Store interface {
	// Get returns the object and its generation.
	Get(ctx context.Context, name string) ([]byte, int64, error)
	// Put writes the object if its generation matches ifGen (0: must not
	// exist, PutAny: unconditional). A mismatch returns ErrPrecondition.
	Put(ctx context.Context, name string, data []byte, contentType string, ifGen int64) error
	Delete(ctx context.Context, name string) error
	Stat(ctx context.Context, name string) (ObjectInfo, error)
	// List returns up to limit object names under prefix, in lexical order,
	// strictly after startAfter.
	List(ctx context.Context, prefix, startAfter string, limit int) ([]string, error)
	// SignedPutURL returns a URL that accepts one PUT of the object with
	// exactly this Content-Type until the URL expires.
	SignedPutURL(ctx context.Context, name, contentType string, maxBytes int64, ttl time.Duration) (string, error)
	PublicURL(name string) string
}
