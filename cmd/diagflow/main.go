// Command diagflow is the CLI demo and HTTP API entrypoint for the Go port.
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

	"github.com/Admaing/diagflow/internal/config"
	"github.com/Admaing/diagflow/internal/diagagent"
	"github.com/Admaing/diagflow/internal/observability/report"
	"github.com/Admaing/diagflow/internal/observability/store"
	"github.com/Admaing/diagflow/internal/rag/knowledgebase"
	"github.com/Admaing/diagflow/internal/rag/vectorstore"
	"github.com/Admaing/diagflow/internal/server"
	"github.com/Admaing/diagflow/internal/simulated"
	"github.com/Admaing/diagflow/internal/tools/v3tools"
)

func main() {
	serve := flag.Bool("serve", false, "run the HTTP API server instead of the CLI demo")
	port := flag.Int("port", 0, "HTTP port override (default from DIAGFLOW_SERVER__PORT)")
	scenario := flag.String("scenario", "flink_oom", "scenario name")
	list := flag.Bool("list", false, "list available scenarios")
	all := flag.Bool("all", false, "run all scenarios")
	investigate := flag.Bool("investigate", false, "use intent-driven internal investigation")
	flag.Parse()

	if *serve {
		runServer(*port)
		return
	}

	if *list {
		for name := range simulated.Scenarios {
			fmt.Println(name)
		}
		return
	}

	apiKey := config.Get().LLM.APIKey

	if *all {
		for name := range simulated.Scenarios {
			runOne(name, apiKey, *investigate)
		}
		return
	}
	runOne(*scenario, apiKey, *investigate)
}

func runServer(portOverride int) {
	cfg := config.Get()
	if portOverride > 0 {
		cfg.Server.Port = portOverride
	}
	if cfg.LLM.APIKey == "" {
		fmt.Fprintln(os.Stderr, "warning: DEEPSEEK_API_KEY/LLM_API_KEY not set — diagnoses will degrade")
	}

	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: parseLogLevel(cfg.LogLevel),
	}))

	if problems := cfg.Validate(); len(problems) > 0 {
		for _, p := range problems {
			log.Error("invalid config", "problem", p)
		}
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	dbStore := store.New(log)
	srv := server.New(log, dbStore)
	if err := srv.ListenAndServe(ctx); err != nil && err != context.Canceled {
		fmt.Fprintf(os.Stderr, "server error: %v\n", err)
		os.Exit(1)
	}
	log.Info("server stopped")
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

func runOne(name, apiKey string, investigate bool) {
	ctx := context.Background()

	c, err := simulated.NewCluster(name)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("\n%s\n🔍 %s\n%s\n", strings.Repeat("=", 50), name, strings.Repeat("=", 50))
	fmt.Println(c.Summary())

	if apiKey == "" {
		fmt.Printf("\n**预期根因**: %s\n> 设置 DEEPSEEK_API_KEY 启用 LLM\n", c.ExpectedRootCause())
		return
	}

	store := vectorstore.NewMemory()
	kb := knowledgebase.New(store)
	defs := v3tools.BuildV3Tools(c, kb)

	agent := diagagent.New(
		diagagent.WithModel("deepseek-v4-flash"),
		diagagent.WithKB(kb),
		diagagent.WithEventFunc(func(msg string) { fmt.Printf("  📋 %s\n", msg) }),
	)
	agent.RegisterTools(defs)

	component := stringOr(c.Context()["component"], "flink")
	problem := stringOr(c.Context()["problem"], "unknown")

	var result *diagagent.Report
	if investigate {
		result, err = agent.Investigate(ctx, component, problem, c.Context())
	} else {
		result, err = agent.Diagnose(ctx, component, problem, c.Context())
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("\n%s\n", report.Render(result))
	if result.Trace != nil {
		fmt.Printf("\n%s\n", result.Trace.RenderMarkdown())
	}
}

func stringOr(v any, def string) string {
	if s, ok := v.(string); ok {
		return s
	}
	return def
}
