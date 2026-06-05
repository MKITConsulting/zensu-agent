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
	Slug            string `json:"slug"`
	Name            string `json:"name,omitempty"`
	Status          string `json:"status"`
	ReadyReplicas   *int32 `json:"readyReplicas,omitempty"`
	DesiredReplicas *int32 `json:"desiredReplicas,omitempty"`
	IntervalSeconds int32  `json:"intervalSeconds,omitempty"`
}

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
