package health

import (
	"fmt"
	"strconv"
	"time"

	"github.com/galpt/cake-stats/pkg/orbi"
)

// evalOrbi checks the WiFi AP (Netgear Orbi in AP mode): reachable, its
// admin page not permanently locked, and poll data fresh. The AP is the
// radio path for every wireless client — if it hangs, Wi-Fi is down even
// though the router itself looks healthy.
func evalOrbi(op *orbi.Poller) Check {
	const name = "WiFi AP"
	st := op.Snapshot()

	if !st.Reachable {
		return Check{Name: name, State: StateCrit, Value: "no response",
			Detail: "the Orbi AP is not responding on the network — wireless devices lose connectivity"}
	}
	if !st.At.IsZero() && time.Since(st.At) > 10*time.Minute {
		return Check{Name: name, State: StateWarn, Value: "stale",
			Detail: fmt.Sprintf("no fresh data from the AP for %s — poll blocked or AP degraded", time.Since(st.At).Round(time.Minute))}
	}

	wifi := 0
	for _, d := range st.Devices {
		if d.ConnType == "2.4G" || d.ConnType == "5G" {
			wifi++
		}
	}

	detail := fmt.Sprintf("%s (firmware %s) reachable — %d device(s) attached, %d on Wi-Fi",
		orbiLabel(st), st.Firmware, len(st.Devices), wifi)
	if st.Busy {
		detail += " — its device list is unavailable right now, showing the last known devices"
	}
	return Check{Name: name, State: StateOK, Value: fmt.Sprintf("%d devices", len(st.Devices)), Detail: detail}
}

func orbiLabel(st orbi.State) string {
	if st.Model != "" {
		return st.Model
	}
	return "Orbi AP"
}

// evalWifiSignal watches the weakest client on the mesh. A single client
// far from the AP drags its own experience; only severe weakness signals
// a placement or interference problem worth acting on.
func evalWifiSignal(op *orbi.Poller) Check {
	const name = "Wi-Fi signal"
	st := op.Snapshot()
	if st.Busy || len(st.Devices) == 0 {
		return Check{Name: name, State: StateUnknown, Value: "—",
			Detail: "no fresh Wi-Fi device list from the AP"}
	}
	weakest := -1
	who := ""
	band := ""
	for _, d := range st.Devices {
		if d.ConnType != "2.4G" && d.ConnType != "5G" {
			continue
		}
		if weakest == -1 || d.Signal < weakest {
			weakest, who, band = d.Signal, d.Name, d.ConnType
		}
	}
	if weakest == -1 {
		return Check{Name: name, State: StateUnknown, Value: "—",
			Detail: "no wireless clients are connected right now"}
	}
	detail := "weakest client is " + who + " on " + bandLabel(band) +
		" — for reference, 70%+ is strong, 50–70% is usable, below 50% shows in slow speeds and dropouts"
	if weakest < 35 {
		return Check{Name: name, State: StateCrit, Value: strconv.Itoa(weakest) + "%", Detail: detail}
	}
	if weakest < 50 {
		return Check{Name: name, State: StateWarn, Value: strconv.Itoa(weakest) + "%", Detail: detail}
	}
	return Check{Name: name, State: StateOK, Value: strconv.Itoa(weakest) + "%", Detail: detail}
}

func bandLabel(ct string) string {
	if ct == "2.4G" {
		return "2.4 GHz"
	}
	return "5 GHz"
}
