// Command diagflow is the CLI demo entrypoint for the Go port.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/Admaing/diagflow/internal/diagagent"
	"github.com/Admaing/diagflow/internal/observability/report"
	"github.com/Admaing/diagflow/internal/rag/knowledgebase"
	"github.com/Admaing/diagflow/internal/rag/vectorstore"
	"github.com/Admaing/diagflow/internal/simulated"
	"github.com/Admaing/diagflow/internal/tools/v3tools"
)

func main() {
	scenario := flag.String("scenario", "flink_oom", "scenario name")
	list := flag.Bool("list", false, "list available scenarios")
	all := flag.Bool("all", false, "run all scenarios")
	flag.Parse()

	if *list {
		for name := range simulated.Scenarios {
			fmt.Println(name)
		}
		return
	}

	apiKey := os.Getenv("DEEPSEEK_API_KEY")
	if apiKey == "" {
		apiKey = os.Getenv("LLM_API_KEY")
	}

	if *all {
		for name := range simulated.Scenarios {
			runOne(name, apiKey)
		}
		return
	}
	runOne(*scenario, apiKey)
}

func runOne(name, apiKey string) {
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

	result, err := agent.Diagnose(ctx,
		stringOr(c.Context()["component"], "flink"),
		stringOr(c.Context()["problem"], "unknown"),
		c.Context(),
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("\n%s\n", report.Render(result))
}

func stringOr(v any, def string) string {
	if s, ok := v.(string); ok {
		return s
	}
	return def
}
