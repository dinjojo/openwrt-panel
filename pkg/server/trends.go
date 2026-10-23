package server

// Trends extend the percentiles report with range-aggregated views of the
// data VictoriaMetrics already holds: latency percentiles, shaper
// utilisation, AQM (CAKE) marks and drops, router headroom, Wi-Fi quality,
// bufferbloat grade history and the reliability ledger. Everything here is a
// read over existing series — no new collectors except wan_session_up, which
// the poller pushes once a minute.

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/galpt/cake-stats/pkg/vm"
)

// TrendPair is p50/p95/p99 of one measurement over the selected range.
type TrendPair struct {
	P50     float64 `json:"p50"`
	P95     float64 `json:"p95"`
	P99     float64 `json:"p99"`
	Max     float64 `json:"max,omitempty"`
	Samples int     `json:"samples"`
}

// ShaperTrend is utilisation of one shaped direction over the range.
type ShaperTrend struct {
	P95Mbps     float64 `json:"p95_mbps"`     // p95 transfer rate through the shaper
	CeilingMbps float64 `json:"ceiling_mbps"` // configured CAKE rate
	PinnedPct   float64 `json:"pinned_pct"`   // % of samples above 90% of the ceiling
	Samples     int     `json:"samples"`
}

// AQMTrend is CAKE congestion-management activity for one direction.
type AQMTrend struct {
	Marks        float64 `json:"marks"`          // ECN marks over the range
	Drops        float64 `json:"drops"`          // packets dropped over the range
	PeakDelayP95 float64 `json:"peak_delay_p95"` // p95 of per-sample peak queue delay, ms
	TargetMs     float64 `json:"target_ms"`      // CAKE target, ms
}

// HeadroomTrend is router resource headroom over the range.
type HeadroomTrend struct {
	CPUP95       float64 `json:"cpu_p95"`
	TempMax      float64 `json:"temp_max"`
	ConntrackP95 float64 `json:"conntrack_p95"`
	LoadP95      float64 `json:"load_p95"`
}

// WifiDeviceTrend is one client's signal quality over the range.
type WifiDeviceTrend struct {
	Name       string  `json:"name"`
	MAC        string  `json:"mac"`
	Band       string  `json:"band"`
	SignalP50  float64 `json:"signal_p50"`
	SignalP95  float64 `json:"signal_p95"` // p95 WORST signal (5th percentile of RSSI)
	LinkP95    float64 `json:"link_p95"`   // p95 link rate, Mbit/s
	PresentPct float64 `json:"present_pct"`
}

// BloatDay is one day of bufferbloat probe results for one direction.
type BloatDay struct {
	Day    string   `json:"day"` // YYYY-MM-DD
	MaxMS  float64  `json:"max_ms"`
	AvgMS  float64  `json:"avg_ms"`
	Runs   int      `json:"runs"`
	Grades []string `json:"grades,omitempty"`
}

// TrendReport is everything /api/percentiles returns beyond the legacy fields.
type TrendReport struct {
	Range       string                 `json:"range"`
	Latency     []TrendPair            `json:"latency,omitempty"` // per ping target
	Stddev      float64                `json:"stddev_ms,omitempty"`
	Shaper      map[string]ShaperTrend `json:"shaper,omitempty"` // down | up
	AQM         map[string]AQMTrend    `json:"aqm,omitempty"`    // down | up
	Headroom    *HeadroomTrend         `json:"headroom,omitempty"`
	WifiClients *TrendPair             `json:"wifi_clients,omitempty"`
	WifiDevices []WifiDeviceTrend      `json:"wifi_devices,omitempty"`
	Bloat       map[string][]BloatDay  `json:"bloat,omitempty"` // down | up
	WanChanges  int                    `json:"wan_changes"`
	WanP99Loss  float64                `json:"wan_p99_loss"`
}

// trendRanges maps the report range to a PromQL range selector string.
func trendRangeSelector(rng string) string {
	return rng // "24h", "7d", "30d" are already valid selectors
}

// addTrends fills the trend fields of the percentiles response. Every query
// is best-effort: a failed or empty series leaves the field absent and the
// UI renders an honest "no data" note instead of zeros.
func (s *Server) addTrends(ctx context.Context, out *PercentileReport, rng string, start, end int64) {
	tr := TrendReport{Range: rng, Shaper: map[string]ShaperTrend{}, AQM: map[string]AQMTrend{}, Bloat: map[string][]BloatDay{}}

	// ── Latency percentiles per ping target ──────────────────────────
	targets, _ := s.vmc.Query(ctx,
		fmt.Sprintf(`quantile_over_time(0.99, collectd_ping[%s])`, trendRangeSelector(rng)))
	p95s, _ := s.vmc.Query(ctx,
		fmt.Sprintf(`quantile_over_time(0.95, collectd_ping[%s])`, trendRangeSelector(rng)))
	p50s, _ := s.vmc.Query(ctx,
		fmt.Sprintf(`quantile_over_time(0.50, collectd_ping[%s])`, trendRangeSelector(rng)))
	counts, _ := s.vmc.Query(ctx,
		fmt.Sprintf(`count_over_time(collectd_ping[%s])`, trendRangeSelector(rng)))
	if len(p95s) > 0 {
		p99By, p50By, cntBy := pointsByLabel(targets, "ping"), pointsByLabel(p50s, "ping"), pointsByLabel(counts, "ping")
		for _, pt := range p95s {
			name := pt.Labels["ping"]
			if name == "" {
				name = "wan"
			}
			tp := TrendPair{P95: pt.Val, Samples: int(cntBy[name])}
			if v, ok := p50By[name]; ok {
				tp.P50 = v
			}
			if v, ok := p99By[name]; ok {
				tp.P99 = v
			}
			tr.Latency = append(tr.Latency, tp)
		}
		sort.Slice(tr.Latency, func(i, j int) bool { return tr.Latency[i].Samples > tr.Latency[j].Samples })
	}
	if se, err := s.vmc.Query(ctx,
		fmt.Sprintf(`avg_over_time(collectd_ping_ping_stddev[%s])`, trendRangeSelector(rng))); err == nil {
		for _, pt := range se {
			tr.Stddev = pt.Val
		}
	}

	// ── Shaper utilisation per direction ─────────────────────────────
	// Throughput: the "tx" byte counter on the whole-interface series.
	for _, dir := range []struct{ key, iface string }{{"down", "ifb4eth0"}, {"up", "eth0"}} {
		st := ShaperTrend{}
		if se, err := s.vmc.QueryRange(ctx,
			fmt.Sprintf(`sum(rate(collectd_sqm_bytes{type="tx",sqm="%s"}[5m]))*8`, dir.iface),
			start, end, rangeStep(rng)); err == nil && len(se) > 0 {
			st.P95Mbps = percentile(se[0].Val, 95) / 1e6
			st.Samples = len(se[0].Val)
		}
		if se, err := s.vmc.Query(ctx,
			fmt.Sprintf(`max(collectd_sqm_gauge{type="configured_bw_bps",sqm="%s"})`, dir.iface)); err == nil && len(se) > 0 {
			// tc reports cake's bandwidth option in bytes/s — convert to Mbit/s.
			st.CeilingMbps = se[0].Val * 8 / 1e6
		}
		if st.P95Mbps > 0 && st.CeilingMbps > 0 && st.Samples > 0 {
			tr.Shaper[dir.key] = st
		}
	}
	// Pinned % needs the rate samples again; compute here where we have them.
	for _, dir := range []struct{ key, iface string }{{"down", "ifb4eth0"}, {"up", "eth0"}} {
		st, ok := tr.Shaper[dir.key]
		if !ok || st.CeilingMbps <= 0 {
			continue
		}
		if se, err := s.vmc.QueryRange(ctx,
			fmt.Sprintf(`sum(rate(collectd_sqm_bytes{type="tx",sqm="%s"}[5m]))*8`, dir.iface),
			start, end, rangeStep(rng)); err == nil && len(se) > 0 {
			pinned := 0
			for _, v := range se[0].Val {
				if v/1e6 >= 0.9*st.CeilingMbps {
					pinned++
				}
			}
			st.PinnedPct = 100 * float64(pinned) / float64(len(se[0].Val))
			tr.Shaper[dir.key] = st
		}
	}

	// ── AQM marks / drops per direction ──────────────────────────────
	// CAKE gauges live on the per-tin series: eth0-tin-t0 (upload),
	// ifb4eth0-tin-t0 (download).
	aqm, _ := s.vmc.Query(ctx,
		fmt.Sprintf(`sum by (type, sqm) (increase(collectd_sqm_gauge{type=~"ecn_marks|drops",sqm=~"(eth0|ifb4eth0)-tin.*"}[%s]))`,
			trendRangeSelector(rng)))
	aqmBy := func(typ, iface string) float64 {
		for _, pt := range aqm {
			if pt.Labels["type"] == typ && pt.Labels["sqm"] == iface+"-tin-t0" {
				return pt.Val
			}
		}
		return 0
	}
	peakP95, _ := s.vmc.Query(ctx,
		fmt.Sprintf(`quantile_over_time(0.95, collectd_sqm_gauge{type="peak_delay_us",sqm=~"(eth0|ifb4eth0)-tin.*"}[%s])/1000`,
			trendRangeSelector(rng)))
	targetMs, _ := s.vmc.Query(ctx, `max(collectd_sqm_gauge{type="target_us"})/1000`)
	target := 5.0
	if len(targetMs) > 0 {
		target = targetMs[0].Val
	}
	peakBy := func(iface string) float64 {
		for _, pt := range peakP95 {
			if pt.Labels["sqm"] == iface+"-tin-t0" {
				return pt.Val
			}
		}
		return 0
	}
	for _, dir := range []struct{ key, iface string }{{"down", "ifb4eth0"}, {"up", "eth0"}} {
		a := AQMTrend{
			Marks:        aqmBy("ecn_marks", dir.iface),
			Drops:        aqmBy("drops", dir.iface),
			PeakDelayP95: peakBy(dir.iface),
			TargetMs:     target,
		}
		if a.Marks > 0 || a.Drops > 0 || a.PeakDelayP95 > 0 {
			tr.AQM[dir.key] = a
		}
	}

	// ── Router headroom ──────────────────────────────────────────────
	h := HeadroomTrend{}
	// CPU busy %: collectd reports per-core, per-type percentages, so busy
	// is 100 minus the per-core idle average across the range.
	if se, err := s.vmc.QueryRange(ctx, `100 - avg(collectd_cpu_percent{type="idle"})`, start, end, rangeStep(rng)); err == nil && len(se) > 0 {
		h.CPUP95 = percentile(se[0].Val, 95)
	}
	if se, err := s.vmc.Query(ctx,
		fmt.Sprintf(`max_over_time(collectd_thermal_temperature[%s])`, trendRangeSelector(rng))); err == nil && len(se) > 0 {
		for _, pt := range se {
			if pt.Val > h.TempMax {
				h.TempMax = pt.Val
			}
		}
	}
	if se, err := s.vmc.Query(ctx,
		fmt.Sprintf(`quantile_over_time(0.95, collectd_conntrack{conntrack=""}[%s])`, trendRangeSelector(rng))); err == nil && len(se) > 0 {
		h.ConntrackP95 = se[0].Val
	}
	if se, err := s.vmc.Query(ctx,
		fmt.Sprintf(`quantile_over_time(0.95, collectd_load_0[%s])`, trendRangeSelector(rng))); err == nil && len(se) > 0 {
		h.LoadP95 = se[0].Val
	}
	if h.CPUP95 > 0 || h.TempMax > 0 || h.ConntrackP95 > 0 {
		tr.Headroom = &h
	}

	// ── Wi-Fi quality ────────────────────────────────────────────────
	if se, err := s.vmc.Query(ctx,
		fmt.Sprintf(`quantile_over_time(0.95, orbi_wifi_clients[%s])`, trendRangeSelector(rng))); err == nil && len(se) > 0 {
		tr.WifiClients = &TrendPair{P95: se[0].Val}
	}
	if se, err := s.vmc.QueryRange(ctx, `orbi_client_signal`, start, end, rangeStep(rng)); err == nil {
		type dev struct {
			name, band, mac string
			vals            []float64
			link            []float64
		}
		byMac := map[string]*dev{}
		for _, series := range se {
			mac := series.Labels["mac"]
			if mac == "" {
				continue
			}
			d := byMac[mac]
			if d == nil {
				d = &dev{name: series.Labels["name"], band: series.Labels["band"], mac: mac}
				byMac[mac] = d
			}
			for _, v := range series.Val {
				if v != 0 {
					d.vals = append(d.vals, v)
				}
			}
		}
		if ls, err := s.vmc.QueryRange(ctx, `orbi_client_linkspeed`, start, end, rangeStep(rng)); err == nil {
			for _, series := range ls {
				if d := byMac[series.Labels["mac"]]; d != nil {
					d.link = append(d.link, series.Val...)
				}
			}
		}
		devices := make([]WifiDeviceTrend, 0, len(byMac))
		for _, d := range byMac {
			if len(d.vals) < 3 {
				continue
			}
			sorted := append([]float64(nil), d.vals...)
			sort.Float64s(sorted)
			wt := TrendPair{P50: percentile(sorted, 50)}
			// Signal is negative dBm: the p95 WORST signal is the 5th
			// percentile of the sorted values.
			wt.P95 = percentile(sorted, 5)
			wt.Samples = len(sorted)
			wd := WifiDeviceTrend{
				Name: d.name, MAC: d.mac, Band: d.band,
				SignalP50: wt.P50, SignalP95: wt.P95,
				PresentPct: 100 * float64(len(d.vals)) / float64(expectedSamples(rng, rangeStep(rng))),
			}
			if len(d.link) > 0 {
				wd.LinkP95 = percentile(d.link, 95)
			}
			devices = append(devices, wd)
		}
		sort.Slice(devices, func(i, j int) bool { return devices[i].PresentPct > devices[j].PresentPct })
		if len(devices) > 6 {
			devices = devices[:6]
		}
		tr.WifiDevices = devices
	}

	// ── Bufferbloat grade history (always 30d — grades are hourly) ───
	hStart := time.Now().Add(-30 * 24 * time.Hour).Unix()
	for _, dir := range []struct{ key, label string }{{"down", "down"}, {"up", "up"}} {
		raw, err := s.vmc.QueryRaw(ctx,
			fmt.Sprintf(`probe_bloat_ms{direction="%s"}`, dir.label), hStart, end)
		if err != nil {
			continue
		}
		grades, _ := s.vmc.QueryRaw(ctx,
			fmt.Sprintf(`probe_bloat_grade{direction="%s"}`, dir.label), hStart, end)
		gradeByDay := map[string][]string{}
		for _, se := range grades {
			for _, ts := range se.TS {
				day := time.Unix(ts, 0).Format("2006-01-02")
				if g := se.Labels["grade"]; g != "" {
					gradeByDay[day] = append(gradeByDay[day], g)
				}
			}
		}
		days := map[string]*BloatDay{}
		var order []string
		for _, se := range raw {
			for i, v := range se.Val {
				day := time.Unix(se.TS[i], 0).Format("2006-01-02")
				// loaded RTT can sit a hair below idle from jitter; that is
				// noise, not negative bloat.
				if v < 0 {
					v = 0
				}
				d := days[day]
				if d == nil {
					d = &BloatDay{Day: day}
					days[day] = d
					order = append(order, day)
				}
				if v > d.MaxMS {
					d.MaxMS = v
				}
				d.AvgMS += v
				d.Runs++
			}
		}
		out := make([]BloatDay, 0, len(order))
		sort.Strings(order)
		for _, day := range order {
			d := days[day]
			d.AvgMS /= float64(d.Runs)
			d.Grades = gradeByDay[day]
			out = append(out, *d)
		}
		if len(out) > 0 {
			tr.Bloat[dir.key] = out
		}
	}

	// ── Reliability ledger ───────────────────────────────────────────
	if se, err := s.vmc.Query(ctx,
		fmt.Sprintf(`quantile_over_time(0.99, collectd_ping_ping_droprate[%s])`, trendRangeSelector(rng))); err == nil && len(se) > 0 {
		tr.WanP99Loss = se[0].Val
	}
	if se, err := s.vmc.Query(ctx,
		fmt.Sprintf(`changes(wan_session_up[%s])`, trendRangeSelector(rng))); err == nil && len(se) > 0 {
		tr.WanChanges = int(se[0].Val)
	}

	out.Trends = &tr
}

// pointsByLabel indexes instant-query points by one label's value.
func pointsByLabel(points []vm.Point, label string) map[string]float64 {
	m := make(map[string]float64, len(points))
	for _, pt := range points {
		m[pt.Labels[label]] = pt.Val
	}
	return m
}

// rangeStep picks a query step coarse enough to keep ranges cheap.
func rangeStep(rng string) string {
	switch rng {
	case "24h":
		return "120s"
	case "7d":
		return "900s"
	default:
		return "3600s"
	}
}

// expectedSamples is how many range-step points a full range should hold.
func expectedSamples(rng, step string) float64 {
	dur := map[string]float64{"24h": 86400, "7d": 604800, "30d": 2592000}[rng]
	st := map[string]float64{"60s": 60, "120s": 120, "900s": 900, "1800s": 1800, "3600s": 3600}[step]
	if dur == 0 || st == 0 {
		return 1
	}
	return dur / st
}
