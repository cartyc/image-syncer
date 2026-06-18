// Command cgr-sync mirrors Chainguard (cgr.dev) images into a private OCI
// registry. Two modes:
//
//	cgr-sync sync  [flags]   one-shot mirror (run as a CI step / cron) [default]
//	cgr-sync serve [flags]   webhook listener — mirror on Chainguard push events
//
// The two are complementary: `serve` reacts to new images in near-real-time;
// a scheduled `sync` reconciles anything an event was missed for.
package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/cartyc/image-syncer/internal/config"
	"github.com/cartyc/image-syncer/internal/events"
	imgsync "github.com/cartyc/image-syncer/internal/sync"
	"github.com/cartyc/image-syncer/internal/verify"
)

// version is overridden at build time: -ldflags "-X main.version=<v>".
var version = "dev"

func main() {
	args := os.Args[1:]
	switch {
	case len(args) > 0 && args[0] == "serve":
		serveCmd(args[1:])
	case len(args) > 0 && (args[0] == "-version" || args[0] == "--version"):
		fmt.Println("cgr-sync", version)
	case len(args) > 0 && args[0] == "sync":
		syncCmd(args[1:])
	default:
		syncCmd(args) // default subcommand
	}
}

func logf(f string, a ...any) { fmt.Printf(f+"\n", a...) }

func fatal(code int, f string, a ...any) {
	fmt.Fprintf(os.Stderr, f+"\n", a...)
	os.Exit(code)
}

// verifierFor returns a cosign verifier if any repository enables verification,
// else nil. Exits with a clear error if verification is required but cosign is
// unavailable.
func verifierFor(cfg *config.Config) imgsync.Verifier {
	for _, r := range cfg.Repositories {
		if r.Verify.Enabled {
			v, err := verify.NewCosign()
			if err != nil {
				fatal(2, "%v", err)
			}
			return v
		}
	}
	return nil
}

// syncCmd is the one-shot mirror (CI / cron).
func syncCmd(args []string) {
	fs := flag.NewFlagSet("sync", flag.ExitOnError)
	cfgPath := fs.String("config", "cgr-sync.yaml", "path to the config file")
	dryRun := fs.Bool("dry-run", false, "plan the work and print it, but copy nothing")
	cont := fs.Bool("continue-on-error", false, "keep going after a failure instead of exiting on the first")
	noSigs := fs.Bool("no-signatures", false, "do not mirror cosign signatures/attestations")
	_ = fs.Parse(args)

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fatal(2, "config: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	res, err := imgsync.Run(ctx, cfg, imgsync.Options{
		DryRun:           *dryRun,
		MirrorSignatures: !*noSigs,
		ContinueOnError:  *cont,
		Verify:           verifierFor(cfg),
		Logf:             logf,
	})
	fmt.Printf("\nsummary: copied=%d skipped=%d signatures=%d failed=%d\n",
		res.Copied, res.Skipped, res.Signatures, res.Failed)
	if err != nil {
		fatal(1, "sync: %v", err)
	}
}

// serveCmd runs the webhook listener that mirrors on Chainguard push events.
func serveCmd(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	cfgPath := fs.String("config", "cgr-sync.yaml", "path to the config file")
	addr := fs.String("addr", ":8080", "listen address")
	path := fs.String("path", "/events", "webhook path")
	audience := fs.String("audience", "", "expected token audience = your public webhook URL (required unless -insecure-skip-verify)")
	identity := fs.String("identity", "", "expected event subject, e.g. webhook:<UIDP> (optional)")
	insecure := fs.Bool("insecure-skip-verify", false, "DANGEROUS: skip event token validation (local testing only)")
	noSigs := fs.Bool("no-signatures", false, "do not mirror cosign signatures/attestations")
	_ = fs.Parse(args)

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fatal(2, "config: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var validator events.Validator
	if *insecure {
		fmt.Fprintln(os.Stderr, "WARNING: event token validation disabled (-insecure-skip-verify); do not expose publicly")
		validator = events.InsecureNoValidator{}
	} else {
		if *audience == "" {
			fatal(2, "serve: -audience (your public webhook URL) is required unless -insecure-skip-verify")
		}
		v, err := events.NewOIDCValidator(ctx, *audience, *identity)
		if err != nil {
			fatal(1, "serve: %v", err)
		}
		validator = v
	}

	opts := imgsync.Options{MirrorSignatures: !*noSigs, Verify: verifierFor(cfg), Logf: logf}
	h := &events.Handler{
		Validate: validator,
		Logf:     logf,
		OnPush: func(ctx context.Context, sourceRepo, tag, digest string) error {
			res, matched, err := imgsync.SyncImage(ctx, cfg, opts, sourceRepo, tag)
			if !matched {
				logf("ignoring %s:%s (not in config)", sourceRepo, tag)
				return nil
			}
			if err == nil {
				logf("synced %s:%s — copied=%d skipped=%d signatures=%d", sourceRepo, tag, res.Copied, res.Skipped, res.Signatures)
			}
			return err
		},
	}

	mux := http.NewServeMux()
	mux.Handle(*path, h)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })

	srv := &http.Server{Addr: *addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()

	logf("cgr-sync %s listening on %s%s — pair with a scheduled `sync` for reconciliation", version, *addr, *path)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		fatal(1, "serve: %v", err)
	}
}
