package server

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/galpt/cake-stats/pkg/alerts"
	"github.com/galpt/cake-stats/pkg/events"
	"github.com/galpt/cake-stats/pkg/orbi"
	"github.com/galpt/cake-stats/pkg/usage"
)

func TestBuildEventMetrics(t *testing.T) {
	loc := time.FixedZone("ICT", 7*3600)
	at := func(day, h, m int) int64 { return time.Date(2026, 10, day, h, m, 0, 0, loc).Unix() }
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, loc)
	// Newest first, as the store returns them.
	evs := []events.Event{
		{At: at(22, 11, 0), Kind: "satellite", Sev: "warn", Msg: "RBS50 went offline"}, // open: 60 min to now
		{At: at(22, 9, 0), Kind: "probe", Sev: "crit", Msg: "Speed check worse", Count: 2},
		{At: at(22, 8, 0), Kind: "probe", Sev: "ok", Msg: "fine"},
		{At: at(21, 23, 30), Kind: "monitor", Sev: "info", Msg: "Monitor restarted"}, // closes the WAN outage
		{At: at(21, 23, 0), Kind: "wan", Sev: "crit", Msg: "WAN session dropped"},
		{At: at(21, 6, 30), Kind: "wifi", Sev: "info", Msg: "RBS50 is back online"}, // legacy kind
		{At: at(21, 6, 0), Kind: "wifi", Sev: "warn", Msg: "RBS50 went offline"},
		{At: at(21, 1, 12), Kind: "wan", Sev: "ok", Msg: "WAN session re-established"},
		{At: at(20, 23, 48), Kind: "wan", Sev: "crit", Msg: "WAN session dropped"},  // spans midnight
		{At: at(20, 12, 0), Kind: "monitor", Sev: "info", Msg: "Monitor restarted"}, // oldest = coverage start
	}
	wins := dayWindows(4, now)
	days := make([]ReportDay, len(wins))
	buildEventMetrics(days, wins, evs, now.Unix())

	if wins[0].date != "2026-10-19" || wins[3].date != "2026-10-22" || wins[3].end != now.Unix() {
		t.Fatalf("windows = %+v", wins)
	}
	if days[0].WANUpPct != nil {
		t.Errorf("Oct 19 is before any history; want null, got %v", *days[0].WANUpPct)
	}
	// Oct 20: observed 12:00–24:00 (43200 s), down 12 min (720 s) → 98.3 %.
	if p := days[1].WANUpPct; p == nil || *p != 98.3 {
		t.Errorf("Oct 20 wan_up_pct = %v, want 98.3", p)
	}
	// Oct 21: down 00:00–01:12 (72 min) + 23:00–23:30 (30 min) = 102 min of 1440 → 92.9 %.
	if p := days[2].WANUpPct; p == nil || *p != 92.9 {
		t.Errorf("Oct 21 wan_up_pct = %v, want 92.9", p)
	}
	if p := days[3].WANUpPct; p == nil || *p != 100 {
		t.Errorf("Oct 22 wan_up_pct = %v, want 100", p)
	}
	if days[2].SatelliteOfflineMin != 30 || days[3].SatelliteOfflineMin != 60 {
		t.Errorf("satellite minutes = %v / %v, want 30 / 60", days[2].SatelliteOfflineMin, days[3].SatelliteOfflineMin)
	}
	if days[3].BufferingEvents != 2 || days[2].BufferingEvents != 0 {
		t.Errorf("buffering = %d / %d, want 0 / 2", days[2].BufferingEvents, days[3].BufferingEvents)
	}
}

func get(t *testing.T, s *Server, path string) (string, []byte) {
	t.Helper()
	resp, err := s.app.Test(httptest.NewRequest("GET", path, nil))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("%s: HTTP %d %s", path, resp.StatusCode, b)
	}
	return resp.Header.Get("Content-Type"), b
}

func TestNewEndpointsUnconfigured(t *testing.T) {
	s := New("127.0.0.1:0", time.Second, 10, "", "test", orbi.New("", "", "", nil))
	s.SetUsageStore(usage.New(t.TempDir()))

	cases := map[string]string{
		"/api/config": `{"alerts":{"enabled":false}}`,
		"/api/radio":  `{"ok":false,"iface":"","err":"radio interface not configured","bands":[],"neighbours":[],"at":0}`,
		"/api/usage":  `{"days":[],"days_total":7}`,
	}
	for path, want := range cases {
		if _, b := get(t, s, path); string(b) != want {
			t.Errorf("%s = %s, want %s", path, b, want)
		}
	}

	var orb map[string]json.RawMessage
	_, b := get(t, s, "/api/orbi")
	if err := json.Unmarshal(b, &orb); err != nil || string(orb["radioenv"]) != "[]" {
		t.Errorf("/api/orbi radioenv = %s (%v), want []", orb["radioenv"], err)
	}

	var rep Report
	_, b = get(t, s, "/api/report?days=40")
	if err := json.Unmarshal(b, &rep); err != nil || len(rep.Days) != 30 || rep.Alerts.Enabled {
		t.Errorf("/api/report?days=40: %d days, err %v", len(rep.Days), err)
	}

	if ct, b := get(t, s, "/manifest.webmanifest"); ct != "application/manifest+json" || len(b) == 0 {
		t.Errorf("manifest content-type %q len %d", ct, len(b))
	}
	if ct, b := get(t, s, "/icon-512.png"); ct != "image/png" || len(b) < 8 || string(b[1:4]) != "PNG" {
		t.Errorf("icon content-type %q", ct)
	}

	n, _ := alerts.New("https://ntfy.sh/topic", "")
	s.SetAlerts(n)
	if _, b := get(t, s, "/api/config"); string(b) != `{"alerts":{"enabled":true,"target":"ntfy"}}` {
		t.Errorf("/api/config with alerts = %s", b)
	}
}
