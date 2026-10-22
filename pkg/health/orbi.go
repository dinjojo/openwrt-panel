package health

import (
	"fmt"
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
