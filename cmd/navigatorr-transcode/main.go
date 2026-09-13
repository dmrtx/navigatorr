package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/jakenesler/navigatorr/internal/transcodeworker"
)

func main() {
	var configPath string

	// Custom parsing to allow --config anywhere
	args := os.Args[1:]
	var subcmd string
	var subcmdArgs []string

	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--config" || arg == "-config" {
			if i+1 < len(args) {
				configPath = args[i+1]
				i++
			}
		} else if stringsHasPrefix(arg, "--config=") {
			configPath = arg[9:]
		} else if stringsHasPrefix(arg, "-config=") {
			configPath = arg[8:]
		} else if subcmd == "" {
			subcmd = arg
		} else {
			subcmdArgs = append(subcmdArgs, arg)
		}
	}

	if subcmd == "" {
		fmt.Fprintf(os.Stderr, "Usage: %s [--config <path>] <doctor|capabilities|submit|status|cancel|_internal_run> [args...]\n", os.Args[0])
		os.Exit(1)
	}

	cfg, err := transcodeworker.LoadWorkerConfig(configPath)
	if err != nil {
		printJSON(map[string]any{"error": err.Error()})
		os.Exit(1)
	}

	worker := transcodeworker.NewWorker(cfg)
	ctx := context.Background()

	selfExe, err := os.Executable()
	if err != nil {
		selfExe = os.Args[0]
	}

	switch subcmd {
	case "doctor":
		res := worker.Doctor(ctx)
		printJSON(res)
		if !res.OK {
			os.Exit(1)
		}

	case "capabilities":
		caps, err := worker.Capabilities(ctx)
		if err != nil {
			printJSON(map[string]any{"error": err.Error(), "capabilities": caps})
			os.Exit(1)
		}
		printJSON(caps)

	case "submit":
		inputData, err := io.ReadAll(os.Stdin)
		if err != nil {
			printJSON(map[string]any{"error": fmt.Sprintf("reading stdin: %v", err)})
			os.Exit(1)
		}
		var req transcodeworker.SubmitRequest
		if err := json.Unmarshal(inputData, &req); err != nil {
			printJSON(map[string]any{"error": fmt.Sprintf("parsing submit JSON: %v", err)})
			os.Exit(1)
		}

		resp, err := worker.Submit(ctx, req, selfExe, configPath)
		printJSON(resp)
		if err != nil {
			os.Exit(1)
		}

	case "_internal_run":
		if len(subcmdArgs) < 1 {
			fmt.Fprintf(os.Stderr, "missing job id for _internal_run\n")
			os.Exit(1)
		}
		jobID := subcmdArgs[0]
		if err := worker.InternalRun(ctx, jobID); err != nil {
			fmt.Fprintf(os.Stderr, "internal_run failed for job %s: %v\n", jobID, err)
			os.Exit(1)
		}

	case "status":
		if len(subcmdArgs) < 1 {
			printJSON(map[string]any{"error": "missing job id"})
			os.Exit(1)
		}
		jobID := subcmdArgs[0]
		st, err := worker.Status(ctx, jobID)
		printJSON(st)
		if err != nil {
			os.Exit(1)
		}

	case "cancel":
		if len(subcmdArgs) < 1 {
			printJSON(map[string]any{"error": "missing job id"})
			os.Exit(1)
		}
		jobID := subcmdArgs[0]
		res, err := worker.Cancel(ctx, jobID)
		printJSON(res)
		if err != nil {
			os.Exit(1)
		}

	default:
		printJSON(map[string]any{"error": fmt.Sprintf("unknown command %q", subcmd)})
		os.Exit(1)
	}
}

func printJSON(v any) {
	data, _ := json.MarshalIndent(v, "", "  ")
	fmt.Println(string(data))
}

func stringsHasPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}

// Keep flag package imported if needed
var _ = flag.String
