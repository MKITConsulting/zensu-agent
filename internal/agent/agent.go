package agent

import (
	"context"
	"log/slog"
	"time"
)

// reporter is the subset of *Reporter the Agent depends on (eases testing).
type reporter interface {
	Send(ctx context.Context, batch HeartbeatBatch) error
}

// Config holds the agent's runtime configuration.
type Config struct {
	ProductID  string
	Source     string
	Namespaces []string
	Interval   time.Duration
}

// Agent collects annotated Deployment statuses and reports them to Zensu.
type Agent struct {
	cfg      Config
	lister   DeploymentLister
	reporter reporter
	log      *slog.Logger
}

// New builds an Agent.
func New(cfg Config, lister DeploymentLister, r reporter, log *slog.Logger) *Agent {
	if log == nil {
		log = slog.Default()
	}
	return &Agent{cfg: cfg, lister: lister, reporter: r, log: log}
}

// Collect lists annotated workloads across the configured namespaces,
// deduplicated by service slug.
func (a *Agent) Collect(ctx context.Context) ([]ServiceHeartbeat, error) {
	seen := map[string]bool{}
	var out []ServiceHeartbeat
	for _, ns := range a.cfg.Namespaces {
		deps, err := a.lister.ListDeployments(ctx, ns)
		if err != nil {
			return nil, err
		}
		for _, d := range deps {
			entry, ok := MapDeployment(d)
			if !ok || seen[entry.Slug] {
				continue
			}
			if a.cfg.Interval > 0 {
				entry.IntervalSeconds = int32(a.cfg.Interval.Seconds())
			}
			seen[entry.Slug] = true
			out = append(out, entry)
		}
	}
	return out, nil
}

// Tick collects and reports a single heartbeat batch.
func (a *Agent) Tick(ctx context.Context) error {
	services, err := a.Collect(ctx)
	if err != nil {
		return err
	}
	if len(services) == 0 {
		a.log.Info("no annotated workloads found", "annotation", AnnotationService)
		return nil
	}
	batch := HeartbeatBatch{ProductID: a.cfg.ProductID, Source: a.cfg.Source, Services: services}
	if err := a.reporter.Send(ctx, batch); err != nil {
		return err
	}
	a.log.Info("heartbeat sent", "services", len(services))
	return nil
}

// Run executes a single Tick when once is true, otherwise loops on the
// configured interval until ctx is cancelled. Tick errors are logged, not fatal,
// so a transient API blip does not crash the agent.
func (a *Agent) Run(ctx context.Context, once bool) error {
	if once {
		return a.Tick(ctx)
	}
	if err := a.Tick(ctx); err != nil {
		a.log.Error("heartbeat tick failed", "error", err)
	}
	t := time.NewTicker(a.cfg.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			if err := a.Tick(ctx); err != nil {
				a.log.Error("heartbeat tick failed", "error", err)
			}
		}
	}
}
