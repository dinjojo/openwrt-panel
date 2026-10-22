package main

import (
	"context"
	"flag"
	"fmt"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/galpt/cake-stats/pkg/alerts"
	"github.com/galpt/cake-stats/pkg/log"
	"github.com/galpt/cake-stats/pkg/orbi"
	"github.com/galpt/cake-stats/pkg/orbissn"
	"github.com/galpt/cake-stats/pkg/radio"
	"github.com/galpt/cake-stats/pkg/server"
	"github.com/galpt/cake-stats/pkg/usage"
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
	// Shell access to the Orbi for per-client radio stats. The host is the
	// -orbi-url host. Never logged; keep the values in a root-only env file.
	orbiSSHUser := flag.String("orbi-ssh-user", os.Getenv("ORBI_SSH_USER"), "Orbi SSH user for per-client Wi-Fi stats (default root when -orbi-ssh-pass is set)")
	orbiSSHPass := flag.String("orbi-ssh-pass", os.Getenv("ORBI_SSH_PASS"), "Orbi SSH password (enables the SSH shell poller; host taken from -orbi-url)")
	orbiTelnetPass := flag.String("orbi-telnet-pass", os.Getenv("ORBI_TELNET_PASS"), "Orbi telnet password (enables telnet shell polling, fallback after SSH; user = -orbi-ssh-user or root)")
	radioIface := flag.String("radio-iface", os.Getenv("RADIO_IFACE"), "local monitor-mode Wi-Fi interface for channel utilisation and neighbour scans, e.g. wlan1 (empty disables)")
	// The ntfy token is never logged; keep it in a root-only env file.
	alertURL := flag.String("alert-ntfy-url", os.Getenv("ALERT_NTFY_URL"), "ntfy topic URL for push alerts on warn/crit events, e.g. https://ntfy.sh/mytopic (empty disables)")
	alertToken := flag.String("alert-ntfy-token", os.Getenv("ALERT_NTFY_TOKEN"), "ntfy access token (sent as Authorization: Bearer)")
	dataDir := flag.String("data-dir", envOr("CAKE_DATA_DIR", "/opt/monitoring"), "directory for persisted per-client usage files (usage-YYYY-MM-DD.jsonl)")
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
	orbiHost := ""
	if u, err := url.Parse(*orbiURL); err == nil && *orbiURL != "" {
		orbiHost = u.Hostname()
	}
	srv.SetShellPoller(orbissn.New(orbissn.Config{
		Host:       orbiHost,
		SSHUser:    *orbiSSHUser,
		SSHPass:    *orbiSSHPass,
		TelnetPass: *orbiTelnetPass,
	}))
	srv.SetRadioCollector(radio.New(*radioIface))
	notifier, err := alerts.New(*alertURL, *alertToken)
	if err != nil {
		log.Logger.Warn().Err(err).Msg("push alerts disabled")
	}
	srv.SetAlerts(notifier)
	if *dataDir != "" {
		srv.SetUsageStore(usage.New(*dataDir))
	}
	if err := srv.Run(ctx, addr); err != nil {
		log.Logger.Fatal().Err(err).Msg("fatal")
	}
	log.Logger.Info().Msg("shutdown complete")
}

// envOr returns the environment variable or a default when it is unset.
func envOr(key, def string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return def
}
