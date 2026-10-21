// Package clients collects per-LAN-client traffic accounting from nlbwmon
// (conntrack netlink counters), resolves friendly names from DHCP leases and
// neighbour states, computes live rates and pushes cumulative counters into
// VictoriaMetrics so per-client history is queryable.
package clients

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/galpt/cake-stats/pkg/vm"
)

// Client is one LAN device as reported to the UI.
type Client struct {
	MAC      string  `json:"mac"`
	IP       string  `json:"ip"`
	Name     string  `json:"name"`
	Vendor   string  `json:"vendor,omitempty"` // MAC OUI vendor, may be empty
	Conns    int     `json:"conns"`
	RxTotal  int64   `json:"rx_total"` // bytes this accounting period
	TxTotal  int64   `json:"tx_total"`
	RxRate   float64 `json:"rx_rate"` // bytes/s (averaged over the poll interval)
	TxRate   float64 `json:"tx_rate"`
	Online   bool    `json:"online"`
	LastSeen int64   `json:"last_seen"` // unix ts of last poll with traffic
}

type sample struct {
	conns  int
	rx, tx int64
	at     time.Time
}

// Store keeps the latest client snapshot and runs the poller.
type Store struct {
	mu         sync.RWMutex
	clients    map[string]*Client // by MAC
	prev       map[string]sample
	prevAt     time.Time
	interval   time.Duration
	vmc        *vm.Client
	nlbwPath   string
	leasesAt   time.Time
	leases     map[string]string // MAC -> hostname (refreshed every 10 min)
	hosts      map[string]string // IP -> hostname from /etc/hosts
	vendors    map[string]string // OUI -> vendor name (cached + persisted)
	namesAt    time.Time         // gate for the slow name-resolution cycle
	namesPath  string            // persisted vendor cache
	httpClient *http.Client
	havePolled bool
}

// New creates a Store. vmURL may be empty to disable history pushes.
func New(interval time.Duration, vmURL string) *Store {
	s := &Store{
		clients:    map[string]*Client{},
		prev:       map[string]sample{},
		interval:   interval,
		leases:     map[string]string{},
		hosts:      map[string]string{},
		vendors:    map[string]string{},
		namesPath:  "/tmp/cake-client-names.json",
		nlbwPath:   "/usr/sbin/nlbw",
		httpClient: &http.Client{Timeout: 5 * time.Second},
	}
	if p, err := exec.LookPath("nlbw"); err == nil {
		s.nlbwPath = p
	}
	s.loadNamesCache()
	return s
}

// Snapshot returns the current client list sorted by total traffic.
func (s *Store) Snapshot() []Client {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Client, 0, len(s.clients))
	for _, c := range s.clients {
		out = append(out, *c)
	}
	// sort by rx+tx total descending
	for i := 1; i < len(out); i++ {
		for j := i; j > 0; j-- {
			a, b := out[j-1], out[j]
			if a.RxTotal+a.TxTotal < b.RxTotal+b.TxTotal {
				out[j-1], out[j] = out[j], out[j-1]
			}
		}
	}
	return out
}

// Name returns the friendly name for a MAC (or the short MAC if unknown).
func (s *Store) Name(mac string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if c, ok := s.clients[mac]; ok && c.Name != "" {
		return c.Name
	}
	return mac
}

// Available reports whether nlbw data has been collected at least once.
func (s *Store) Available() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.clients) > 0 || s.havePolled
}

// Run starts the poll loop until ctx is cancelled.
func (s *Store) Run(ctx context.Context, vmClient *vm.Client) {
	s.vmc = vmClient
	if err := s.poll(); err != nil {
		log.Printf("[clients] nlbwmon poll failed: %v (is nlbwmon installed?)", err)
	}
	t := time.NewTicker(s.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := s.poll(); err != nil {
				log.Printf("[clients] nlbwmon poll failed: %v", err)
			}
		}
	}
}

func (s *Store) poll() error {
	out, err := exec.Command(s.nlbwPath, "-c", "csv", "-g", "mac", "-q").Output()
	if err != nil {
		s.mu.Lock()
		s.havePolled = false
		s.mu.Unlock()
		return err
	}

	now := time.Now()
	type parsed struct {
		mac   string
		conns int
		rx    int64
		tx    int64
	}
	var rows []parsed
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.Fields(line)
		if len(f) < 6 {
			continue
		}
		mac := f[0]
		if !strings.Contains(mac, ":") { // skip header / junk
			continue
		}
		conns, _ := strconv.Atoi(f[1])
		rx, _ := strconv.ParseInt(f[2], 10, 64)
		tx, _ := strconv.ParseInt(f[4], 10, 64)
		rows = append(rows, parsed{mac, conns, rx, tx})
	}

	s.refreshNames()
	online := s.onlineMacs()

	s.mu.Lock()
	s.havePolled = true

	elapsed := now.Sub(s.prevAt).Seconds()
	samples := map[string]sample{}
	for _, r := range rows {
		samples[r.mac] = sample{conns: r.conns, rx: r.rx, tx: r.tx, at: now}
	}

	for _, r := range rows {
		c := s.clients[r.mac]
		if c == nil {
			c = &Client{MAC: r.mac}
			s.clients[r.mac] = c
		}
		c.Conns = r.conns
		c.RxTotal = r.rx
		c.TxTotal = r.tx
		c.IP = s.ipForMac(r.mac)
		if r.mac == "00:00:00:00:00:00" {
			// nlbwmon's catch-all for traffic it cannot attribute to a MAC
			// (locally originated or non-bridge interfaces).
			c.Name = "untracked"
			c.Vendor = ""
		} else {
			c.Name = s.nameFor(r.mac, c.IP)
			c.Vendor = s.vendorFor(r.mac)
		}

		// Rates from previous sample.
		c.RxRate, c.TxRate = 0, 0
		if p, ok := s.prev[r.mac]; ok && elapsed > 0 && r.rx >= p.rx && r.tx >= p.tx {
			c.RxRate = float64(r.rx-p.rx) / elapsed
			c.TxRate = float64(r.tx-p.tx) / elapsed
		}
		if c.RxRate > 0 || c.TxRate > 0 {
			c.LastSeen = now.Unix()
		}
		c.Online = online[r.mac] || c.RxRate > 0 || c.TxRate > 0

		// Keep lastSeen across overwrites (client is a fresh struct only once).
	}

	// Retain lastSeen values: rebuild map of previous samples for next round.
	s.prev = samples
	s.prevAt = now
	s.mu.Unlock() // release before the (potentially slow) VM import

	if s.vmc != nil {
		s.pushVM(now)
	}
	return nil
}

// pushVM imports cumulative per-client counters into VictoriaMetrics.
func (s *Store) pushVM(now time.Time) {
	s.mu.RLock()
	var b strings.Builder
	ts := strconv.FormatInt(now.UnixMilli(), 10)
	for _, c := range s.clients {
		name := sanitizeLabel(c.Name)
		mac := sanitizeLabel(c.MAC)
		ip := sanitizeLabel(c.IP)
		b.WriteString("client_bytes_total{mac=\"" + mac + "\",name=\"" + name + "\",ip=\"" + ip + "\",dir=\"rx\"} " +
			strconv.FormatInt(c.RxTotal, 10) + " " + ts + "\n")
		b.WriteString("client_bytes_total{mac=\"" + mac + "\",name=\"" + name + "\",ip=\"" + ip + "\",dir=\"tx\"} " +
			strconv.FormatInt(c.TxTotal, 10) + " " + ts + "\n")
		b.WriteString("client_conns{mac=\"" + mac + "\",name=\"" + name + "\"} " +
			strconv.Itoa(c.Conns) + " " + ts + "\n")
	}
	s.mu.RUnlock()
	if b.Len() == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.vmc.Import(ctx, b.String()); err != nil {
		log.Printf("[clients] VM import failed: %v", err)
	}
}

func sanitizeLabel(v string) string {
	v = strings.ReplaceAll(v, "\\", "\\\\")
	v = strings.ReplaceAll(v, "\"", "\\\"")
	v = strings.ReplaceAll(v, "\n", " ")
	return v
}

// refreshNames re-reads fast local sources every 10 minutes and slowly
// resolves MAC vendors for devices that still have no name. Runs only from
// the poll goroutine, so its maps need no extra locking.
func (s *Store) refreshNames() {
	if time.Since(s.namesAt) < 10*time.Minute {
		return
	}
	s.namesAt = time.Now()
	s.refreshLeases()
	s.refreshHosts()
	s.resolveVendors()
}

// refreshLeases re-reads /tmp/dhcp.leases (the DHCP hostname source).
func (s *Store) refreshLeases() {
	out, err := exec.Command("cat", "/tmp/dhcp.leases").Output()
	if err != nil {
		return
	}
	leases := map[string]string{}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) < 4 {
			continue
		}
		mac := strings.ToLower(f[1])
		host := f[3]
		if host == "*" || host == "" {
			continue
		}
		leases[mac] = host
	}
	s.leases = leases
}

// refreshHosts reads static IP -> hostname mappings from /etc/hosts.
func (s *Store) refreshHosts() {
	out, err := exec.Command("cat", "/etc/hosts").Output()
	if err != nil {
		return
	}
	hosts := map[string]string{}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 || strings.HasPrefix(f[0], "#") {
			continue
		}
		ip := f[0]
		if ip == "127.0.0.1" || ip == "::1" {
			continue
		}
		if hosts[ip] == "" {
			hosts[ip] = f[1]
		}
	}
	s.hosts = hosts
}

// loadNamesCache restores the persisted vendor cache from /tmp.
func (s *Store) loadNamesCache() {
	b, err := os.ReadFile(s.namesPath)
	if err != nil {
		return
	}
	var cache map[string]string
	if json.Unmarshal(b, &cache) == nil {
		s.vendors = cache
	}
}

// saveNamesCache persists the vendor cache so restarts don't re-query.
func (s *Store) saveNamesCache() {
	b, err := json.Marshal(s.vendors)
	if err != nil {
		return
	}
	tmp := s.namesPath + ".tmp"
	if os.WriteFile(tmp, b, 0644) == nil {
		_ = os.Rename(tmp, s.namesPath)
	}
}

// resolveVendors looks up OUI vendor names for MACs that still have no name.
// Queries the public macvendors API politely (max 3 per cycle, ~1 rps) and
// caches results both in memory and on disk. "unknown" marks MACs with no
// vendor so they are not re-queried.
func (s *Store) resolveVendors() {
	const maxLookups = 3
	n := 0
	for mac := range s.clients {
		oui := macToOUI(mac)
		if oui == "" {
			continue
		}
		if _, done := s.vendors[oui]; done {
			continue
		}
		if n >= maxLookups {
			break
		}
		vendor := s.queryVendor(oui)
		s.vendors[oui] = vendor
		n++
		if n < maxLookups {
			time.Sleep(1100 * time.Millisecond) // stay under the 1 rps limit
		}
	}
	if n > 0 {
		s.saveNamesCache()
	}
}

// macToOUI normalizes a MAC to its "aa:bb:cc" prefix, or "" if malformed.
// Locally administered (privacy-randomized) addresses return "randomized".
func macToOUI(mac string) string {
	mac = strings.ToLower(strings.TrimSpace(mac))
	f := strings.Split(mac, ":")
	if len(f) < 3 {
		return ""
	}
	oui := strings.Join(f[:3], ":")
	first, err := strconv.ParseInt(f[0], 16, 32)
	if err != nil {
		return ""
	}
	if first&0x02 != 0 { // locally administered bit: phone/laptop random MAC
		return "randomized"
	}
	return oui
}

// queryVendor fetches the vendor name for one OUI from macvendors.
func (s *Store) queryVendor(oui string) string {
	resp, err := s.httpClient.Get("https://api.macvendors.com/" + oui)
	if err != nil {
		return "" // offline: retry next cycle
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
	if resp.StatusCode == 404 {
		return "unknown" // no vendor for this OUI; cache the negative
	}
	if resp.StatusCode != 200 {
		return "" // rate limited or error: leave uncached, retry later
	}
	return cleanVendor(string(body))
}

// cleanVendor trims legal suffixes: "Apple, Inc." -> "Apple".
func cleanVendor(v string) string {
	v = strings.TrimSpace(v)
	if i := strings.Index(v, ","); i > 0 {
		v = v[:i]
	}
	return strings.TrimSpace(v)
}

// nameFor resolves the friendliest name for a device: DHCP hostname, then
// /etc/hosts by IP, then vendor-derived, then a stable device-xxxx fallback.
func (s *Store) nameFor(mac, ip string) string {
	if name := s.leases[mac]; name != "" {
		return name
	}
	if ip != "" {
		if name := s.hosts[ip]; name != "" {
			return name
		}
	}
	oui := macToOUI(mac)
	if vendor := s.vendors[oui]; vendor != "" && vendor != "unknown" {
		return vendor + " " + macSuffix(mac)
	}
	return "device-" + macSuffix(mac)
}

// vendorFor returns the display vendor label, if any.
func (s *Store) vendorFor(mac string) string {
	oui := macToOUI(mac)
	switch oui {
	case "":
		return ""
	case "randomized":
		return "randomized MAC"
	}
	if v := s.vendors[oui]; v != "" && v != "unknown" {
		return v
	}
	return ""
}

// macSuffix returns the last two octets, e.g. "9d:b0" from fa:69:..:9d:b0.
func macSuffix(mac string) string {
	f := strings.Split(mac, ":")
	if len(f) < 2 {
		return mac
	}
	return strings.Join(f[len(f)-2:], "")
}

// ipForMac looks up the IP from the current clients snapshot or leases.
func (s *Store) ipForMac(mac string) string {
	// Cheap arp/neigh lookup (fast, kernel table).
	out, err := exec.Command("ip", "neigh", "show").Output()
	if err == nil {
		for _, line := range strings.Split(string(out), "\n") {
			f := strings.Fields(line)
			if len(f) >= 5 && strings.EqualFold(f[4], mac) {
				return f[0]
			}
		}
	}
	return ""
}

// onlineMacs returns MACs the kernel considers currently reachable.
func (s *Store) onlineMacs() map[string]bool {
	online := map[string]bool{}
	out, err := exec.Command("ip", "neigh", "show", "dev", "br-lan").Output()
	if err != nil {
		return online
	}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		// format: IP dev br-lan lladdr MAC STATE
		if len(f) < 4 {
			continue
		}
		state := f[len(f)-1]
		if state == "REACHABLE" || state == "DELAY" || state == "PROBE" || state == "STALE" {
			for i, tok := range f {
				if strings.EqualFold(tok, "lladdr") && i+1 < len(f)-1 {
					online[strings.ToLower(f[i+1])] = true
				}
			}
		}
	}
	return online
}
