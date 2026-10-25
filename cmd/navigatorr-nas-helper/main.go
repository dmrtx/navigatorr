// Command navigatorr-nas-helper serves read-only facts about the media library
// from the machine that stores it. See docs/nas-helper.md.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jakenesler/navigatorr/internal/nashelper"
)

type rootList []string

func (r *rootList) String() string { return strings.Join(*r, ",") }
func (r *rootList) Set(v string) error {
	*r = append(*r, v)
	return nil
}

func main() {
	var roots rootList
	listen := flag.String("listen", envOr("NAS_HELPER_LISTEN", ""), "address to listen on, e.g. 192.0.2.10:8480 (required; bind the LAN address only)")
	tokenFile := flag.String("token-file", envOr("NAS_HELPER_TOKEN_FILE", ""), "file holding the bearer token (required)")
	maxHashes := flag.Int("max-hashes", 2, "hashes reading at once")
	flag.Var(&roots, "root", "directory to serve; repeat for several (or set NAS_HELPER_ROOTS, comma separated)")
	flag.Parse()
	if len(roots) == 0 {
		for _, r := range strings.Split(os.Getenv("NAS_HELPER_ROOTS"), ",") {
			if r = strings.TrimSpace(r); r != "" {
				roots = append(roots, r)
			}
		}
	}
	if err := run(*listen, *tokenFile, roots, *maxHashes); err != nil {
		log.Fatal(err)
	}
}

func run(listen, tokenFile string, roots []string, maxHashes int) error {
	if listen == "" || tokenFile == "" || len(roots) == 0 {
		return errors.New("--listen, --token-file and at least one --root are required")
	}
	raw, err := os.ReadFile(tokenFile)
	if err != nil {
		return fmt.Errorf("read token file: %w", err)
	}
	srv, err := nashelper.NewServer(nashelper.Config{
		Roots:     roots,
		Token:     strings.TrimSpace(string(raw)),
		MaxHashes: maxHashes,
		Logf:      log.Printf,
	})
	if err != nil {
		return err
	}
	defer srv.Close()
	httpSrv := &http.Server{
		Addr:              listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	errc := make(chan error, 1)
	go func() { errc <- httpSrv.ListenAndServe() }()
	log.Printf("navigatorr-nas-helper %s instance %s serving %d root(s) on %s", nashelper.Version, srv.InstanceID(), len(roots), listen)
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return httpSrv.Shutdown(shutdown)
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
