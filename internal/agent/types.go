package agent

// Service status values, matching the public Zensu runtime-monitoring contract.
const (
	StatusUp       = "up"
	StatusDegraded = "degraded"
	StatusDown     = "down"
)

// ServiceHeartbeat is one service's status within a heartbeat batch. Field names
// and JSON tags mirror the public contract at
// docs/runtime-monitoring-contract.md in the zensu monorepo.
type ServiceHeartbeat struct {
	Slug            string         `json:"slug"`
	Name            string         `json:"name,omitempty"`
	Status          string         `json:"status"`
	ReadyReplicas   *int32         `json:"readyReplicas,omitempty"`
	DesiredReplicas *int32         `json:"desiredReplicas,omitempty"`
	RestartCount    *int32         `json:"restartCount,omitempty"`
	IntervalSeconds int32          `json:"intervalSeconds,omitempty"`
	Metrics         []MetricSample `json:"metrics,omitempty"`
}

// MetricSample is a single typed metric reading attached to a service heartbeat.
// Key is a string from the backend's metric registry; today only
// "cpu_millicores" and "memory_bytes" are recognized — the backend silently
// skips unknown keys. Value is a JSON number.
type MetricSample struct {
	Key   string  `json:"key"`
	Value float64 `json:"value"`
}

// Metric keys recognized by the backend metric registry. Producing other keys
// is harmless (the backend skips them), but these are the ones the agent emits.
const (
	MetricCPUMillicores = "cpu_millicores"
	MetricMemoryBytes   = "memory_bytes"
)

// HeartbeatBatch is the POST body for /api/runtime/heartbeat.
type HeartbeatBatch struct {
	ProductID string             `json:"productId"`
	Source    string             `json:"source,omitempty"`
	Services  []ServiceHeartbeat `json:"services"`
}

// DeriveStatus maps Deployment replica counts to a service status:
//   - desired<=0 or ready<=0 -> down
//   - ready>=desired         -> up
//   - otherwise              -> degraded
func DeriveStatus(ready, desired int32) string {
	switch {
	case desired <= 0 || ready <= 0:
		return StatusDown
	case ready >= desired:
		return StatusUp
	default:
		return StatusDegraded
	}
}
