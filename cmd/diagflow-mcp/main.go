// Command diagflow-mcp runs the cluster-diagnosis MCP server shared by
// DiagFlow and other agents.
//
// It exposes two data planes:
//   - cluster metadata (cluster info, nodes, services) via the UHadoop access
//     HTTP action API, discovered through ZooKeeper;
//   - node operations (logs, resource snapshots, process checks, YARN) via
//     read-only SSH to each node.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/Admaing/diagflow/internal/config"
	"github.com/Admaing/diagflow/internal/infra"
	"github.com/Admaing/diagflow/internal/mcpserver"
)

func main() {
	port := flag.Int("port", 8090, "HTTP port")
	flag.Parse()

	cfg := config.Get()
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: parseLogLevel(cfg.LogLevel),
	}))

	// Tokens: "agent1=token1,agent2=token2". Refuse to run token-less unless
	// explicitly allowed (dev mode).
	tokens := parseTokens(os.Getenv("DIAGFLOW_MCP_TOKENS"))
	if len(tokens) == 0 && os.Getenv("DIAGFLOW_MCP_ALLOW_NO_AUTH") != "1" {
		log.Error("DIAGFLOW_MCP_TOKENS must define at least one agent token " +
			"(or set DIAGFLOW_MCP_ALLOW_NO_AUTH=1 for dev)")
		os.Exit(1)
	}

	// A DiagFlow deployment serves one availability zone; the zone id scopes
	// ZooKeeper discovery of that zone's UHadoop access service.
	zoneID := firstNonEmpty(cfg.ZKZone(), os.Getenv("ZONE"))
	if zoneID == "" {
		log.Error("zone is required (DIAGFLOW_ZONE): the MCP server is scoped to one availability zone")
		os.Exit(1)
	}
	zkHosts := cfg.Infra.ZKHosts
	if len(zkHosts) == 0 {
		log.Error("ZooKeeper hosts are required (DIAGFLOW_ZK_HOSTS): " +
			"cluster endpoints are discovered, not hard-coded")
		os.Exit(1)
	}

	access := infra.NewZoneClusterAccess(infra.ZKOptions{
		Hosts:   zkHosts,
		Root:    cfg.ZKRootOr(infra.DefaultZKRoot),
		ZoneID:  zoneID,
		Timeout: time.Duration(cfg.Infra.ZKTimeoutS) * time.Second,
	})
	defer access.Close()
	exec := infra.NewSSHExec()
	source := mcpserver.NewInfraSource(access, exec)

	log.Info("mcp server configured",
		"zone", zoneID, "zk", strings.Join(zkHosts, ","), "service", infra.ServiceUHadoopAccess)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	srv := mcpserver.New(log, source, tokens)
	if err := srv.ListenAndServe(ctx, *port); err != nil && err != context.Canceled {
		fmt.Fprintf(os.Stderr, "mcp server error: %v\n", err)
		os.Exit(1)
	}
	log.Info("mcp server stopped")
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func parseTokens(s string) map[string]string {
	out := map[string]string{}
	for _, pair := range strings.Split(s, ",") {
		agent, token, ok := strings.Cut(strings.TrimSpace(pair), "=")
		if ok && agent != "" && token != "" {
			out[token] = agent
		}
	}
	return out
}

func parseLogLevel(s string) slog.Level {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "DEBUG":
		return slog.LevelDebug
	case "INFO":
		return slog.LevelInfo
	case "ERROR":
		return slog.LevelError
	default:
		return slog.LevelWarn
	}
}
