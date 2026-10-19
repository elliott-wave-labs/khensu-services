// Command feed serves the public khensu.nu video feed.
//
// Environment:
//
//	PORT              listen port (default 8080)
//	FEED_STORE        gcs (default), mem, or dir:/path
//	FEED_BUCKET       GCS bucket (default khensu-feed)
//	FEED_PUBLIC_BASE  origin used for /local/ URLs in mem and dir mode
//	                  (default http://127.0.0.1:$PORT)
//	FEED_IP_SECRET    secret for the daily IP-hash salt (required for gcs)
//	FEED_XFF_HOPS     X-Forwarded-For entry to trust, counted from the right
//	                  (default 1 for gcs, 0 otherwise)
package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/elliott-wave-labs/khensu-services/internal/feed"
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(log); err != nil {
		log.Error("feed", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	port := env("PORT", "8080")
	mode := env("FEED_STORE", "gcs")
	base := env("FEED_PUBLIC_BASE", "http://127.0.0.1:"+port)
	cfg := feed.Config{Logger: log, IPSecret: []byte(os.Getenv("FEED_IP_SECRET"))}

	hops := 0
	switch {
	case mode == "gcs":
		g, err := feed.NewGCSStore(ctx, env("FEED_BUCKET", "khensu-feed"))
		if err != nil {
			return err
		}
		defer g.Close()
		cfg.Store = g
		hops = 1
		if len(cfg.IPSecret) == 0 {
			return errors.New("FEED_IP_SECRET is required with FEED_STORE=gcs")
		}
	case mode == "mem":
		s := feed.NewMemStore(base)
		cfg.Store, cfg.Local = s, s.Handler()
	case strings.HasPrefix(mode, "dir:"):
		s, err := feed.NewDirStore(strings.TrimPrefix(mode, "dir:"), base)
		if err != nil {
			return err
		}
		cfg.Store, cfg.Local = s, s.Handler()
	default:
		return errors.New("FEED_STORE must be gcs, mem or dir:/path")
	}
	if v := os.Getenv("FEED_XFF_HOPS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return errors.New("FEED_XFF_HOPS must be a non-negative integer")
		}
		hops = n
	}
	cfg.XFFHops = hops

	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           feed.New(cfg),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	ln, err := net.Listen("tcp", srv.Addr)
	if err != nil {
		return err
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	log.Info("listening", "port", port, "store", mode)
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return srv.Shutdown(shutdown)
}
