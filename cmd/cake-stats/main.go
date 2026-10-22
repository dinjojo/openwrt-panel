package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/galpt/cake-stats/pkg/log"
	"github.com/galpt/cake-stats/pkg/orbi"
	"github.com/galpt/cake-stats/pkg/server"
	"github.com/rs/zerolog"
)

// Version is overridden at build-time via -ldflags "-X main.Version=x.y.z".
var Version = "1.0.0"

func main() {
	host := flag.String("host", "0.0.0.0", "bind address for web interface")
	port := flag.Int("port", 11112, "TCP port for web interface")
	interval := flag.Duration("interval", 100*time.Millisecond, "poll interval for tc")
	histCap := flag.Int("history", 300, "samples to retain per interface")
	vmURL := flag.String("vm", "http://127.0.0.1:8428", "VictoriaMetrics base URL for history and health checks (empty disables)")
	orbiURL := flag.String("orbi-url", os.Getenv("ORBI_URL"), "Orbi AP base URL, e.g. http://192.168.1.100 (empty disables Orbi monitoring)")
	orbiUser := flag.String("orbi-user", os.Getenv("ORBI_USER"), "Orbi admin user")
	orbiPass := flag.String("orbi-pass", os.Getenv("ORBI_PASS"), "Orbi admin password")
	showVer := flag.Bool("version", false, "print version and exit")

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "cake-stats %s\n\n", Version)
		fmt.Fprintf(os.Stderr, "Usage: %s [options]\n\nOptions:\n", os.Args[0])
		flag.PrintDefaults()
	}
	flag.Parse()

	if *showVer {
		fmt.Printf("cake-stats %s\n", Version)
		os.Exit(0)
	}

	addr := fmt.Sprintf("%s:%d", *host, *port)
	log.Logger = log.Logger.Level(zerolog.InfoLevel).With().Str("version", Version).Logger()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv := server.New(addr, *interval, *histCap, *vmURL, Version, orbi.New(*orbiURL, *orbiUser, *orbiPass, nil))
	if err := srv.Run(ctx, addr); err != nil {
		log.Logger.Fatal().Err(err).Msg("fatal")
	}
	log.Logger.Info().Msg("shutdown complete")
}
