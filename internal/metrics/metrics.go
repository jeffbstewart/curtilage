// Package metrics serves the Prometheus text exposition for the
// server (docs/DESIGN.md "Observability").  Hand-written: one
// dependency fewer, and the metric set is small and ours.
package metrics

import (
	"fmt"
	"net/http"
	"sort"

	"github.com/jeffbstewart/curtilage/internal/mqtt"
	"github.com/jeffbstewart/curtilage/internal/record"
	"github.com/jeffbstewart/curtilage/internal/store"
)

// Handler serves /metrics.  st may be nil when there is no event
// store (record-only runs).
func Handler(version string, st *store.Store) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		conn, reconn, dropped, msgs := mqtt.Stats()
		written, files, werrs := record.Stats()
		if st != nil {
			s := st.Stats()
			fmt.Fprintf(w, "# HELP curtilage_events Events in the store.\n# TYPE curtilage_events gauge\n")
			fmt.Fprintf(w, "curtilage_events %d\n", s.Events)
			fmt.Fprintf(w, "# HELP curtilage_events_live Events still running.\n# TYPE curtilage_events_live gauge\n")
			fmt.Fprintf(w, "curtilage_events_live %d\n", s.Live)
			fmt.Fprintf(w, "# HELP curtilage_event_changes_total Changes the policy engine emitted.\n# TYPE curtilage_event_changes_total counter\n")
			fmt.Fprintf(w, "curtilage_event_changes_total %d\n", s.Applied)
			fmt.Fprintf(w, "# HELP curtilage_events_pruned_total Events forgotten at the edge of retention.\n# TYPE curtilage_events_pruned_total counter\n")
			fmt.Fprintf(w, "curtilage_events_pruned_total %d\n", s.Pruned)
			fmt.Fprintf(w, "# HELP curtilage_watchers Open WatchEvents streams.\n# TYPE curtilage_watchers gauge\n")
			fmt.Fprintf(w, "curtilage_watchers %d\n", s.Watchers)
		}

		fmt.Fprintf(w, "# HELP curtilage_build_info Build information.\n# TYPE curtilage_build_info gauge\n")
		fmt.Fprintf(w, "curtilage_build_info{version=%q} 1\n", version)

		fmt.Fprintf(w, "# HELP curtilage_mqtt_connected 1 while the broker session is up.\n# TYPE curtilage_mqtt_connected gauge\n")
		fmt.Fprintf(w, "curtilage_mqtt_connected %d\n", b2i(conn))
		fmt.Fprintf(w, "# HELP curtilage_mqtt_reconnects_total Broker sessions re-established after a loss.\n# TYPE curtilage_mqtt_reconnects_total counter\n")
		fmt.Fprintf(w, "curtilage_mqtt_reconnects_total %d\n", reconn)
		fmt.Fprintf(w, "# HELP curtilage_mqtt_messages_total Messages received, by the topic's first segment.\n# TYPE curtilage_mqtt_messages_total counter\n")
		keys := make([]string, 0, len(msgs))
		for k := range msgs {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(w, "curtilage_mqtt_messages_total{prefix=%q} %d\n", k, msgs[k])
		}
		fmt.Fprintf(w, "# HELP curtilage_mqtt_dropped_total Messages dropped because the recorder was not keeping up.\n# TYPE curtilage_mqtt_dropped_total counter\n")
		fmt.Fprintf(w, "curtilage_mqtt_dropped_total %d\n", dropped)

		fmt.Fprintf(w, "# HELP curtilage_records_written_total Records written to MCAP.\n# TYPE curtilage_records_written_total counter\n")
		fmt.Fprintf(w, "curtilage_records_written_total %d\n", written)
		fmt.Fprintf(w, "# HELP curtilage_record_files_opened_total MCAP files started.\n# TYPE curtilage_record_files_opened_total counter\n")
		fmt.Fprintf(w, "curtilage_record_files_opened_total %d\n", files)
		fmt.Fprintf(w, "# HELP curtilage_record_write_errors_total Records that could not be written.\n# TYPE curtilage_record_write_errors_total counter\n")
		fmt.Fprintf(w, "curtilage_record_write_errors_total %d\n", werrs)
	})
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}
