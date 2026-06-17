// Command cgr-sync mirrors Chainguard (cgr.dev) images into a private OCI
// registry. It is a one-shot CLI: read config, sync, report, exit — designed to
// run as a CI/CD step. Exit code is non-zero if any image failed to sync.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/chriscarty/cgr-sync/internal/config"
	imgsync "github.com/chriscarty/cgr-sync/internal/sync"
)

// version is overridden at build time: -ldflags "-X main.version=<v>".
var version = "dev"

func main() {
	cfgPath := flag.String("config", "cgr-sync.yaml", "path to the config file")
	dryRun := flag.Bool("dry-run", false, "plan the work and print it, but copy nothing")
	cont := flag.Bool("continue-on-error", false, "keep going after a failure instead of exiting on the first")
	noSigs := flag.Bool("no-signatures", false, "do not mirror cosign signatures/attestations")
	showVer := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVer {
		fmt.Println("cgr-sync", version)
		return
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	res, err := imgsync.Run(ctx, cfg, imgsync.Options{
		DryRun:           *dryRun,
		MirrorSignatures: !*noSigs,
		ContinueOnError:  *cont,
		Logf:             func(f string, a ...any) { fmt.Printf(f+"\n", a...) },
	})

	fmt.Printf("\nsummary: copied=%d skipped=%d signatures=%d failed=%d\n",
		res.Copied, res.Skipped, res.Signatures, res.Failed)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sync: %v\n", err)
		os.Exit(1)
	}
}
