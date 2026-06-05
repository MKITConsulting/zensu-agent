// Command zensu-agent reports the runtime status of annotated Kubernetes
// workloads to the Zensu API using an outbound push/heartbeat model.
//
// It reads (never mutates) Deployments carrying the `zensu.dev/service`
// annotation and POSTs their up/degraded/down status to
// ${ZENSU_API_URL}/api/runtime/heartbeat with an X-API-Key. All traffic is
// outbound; the agent never exposes a server.
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/MKITConsulting/zensu-agent/internal/agent"
)

func main() {
	once := flag.Bool("once", false, "run a single heartbeat then exit (for a CronJob or host cron)")
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	apiURL := os.Getenv("ZENSU_API_URL")
	apiKey := os.Getenv("ZENSU_API_KEY")
	productID := os.Getenv("ZENSU_PRODUCT_ID")
	if apiURL == "" || apiKey == "" || productID == "" {
		log.Error("missing required config", "required", "ZENSU_API_URL, ZENSU_API_KEY, ZENSU_PRODUCT_ID")
		os.Exit(1)
	}

	cfg := agent.Config{
		ProductID:  productID,
		Source:     envOr("ZENSU_AGENT_SOURCE", "k8s-agent"),
		Namespaces: envList("ZENSU_AGENT_NAMESPACES", []string{"default"}),
		Interval:   envDuration("ZENSU_AGENT_INTERVAL", 60*time.Second),
	}

	lister, err := agent.NewInClusterLister()
	if err != nil {
		log.Error("kubernetes client", "error", err)
		os.Exit(1)
	}
	reporter := agent.NewReporter(apiURL, apiKey, 15*time.Second)
	a := agent.New(cfg, lister, reporter, log)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := a.Run(ctx, *once); err != nil && ctx.Err() == nil {
		log.Error("agent stopped", "error", err)
		os.Exit(1)
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envDuration(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

func envList(key string, def []string) []string {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return def
	}
	return out
}
