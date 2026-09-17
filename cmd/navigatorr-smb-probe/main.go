package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/jakenesler/navigatorr/internal/smbprobe"
	"gopkg.in/yaml.v3"
)

func main() {
	configPath := flag.String("config", "", "path to probe YAML (required)")
	source := flag.String("source", "", "small share-relative fixture name (required)")
	allowWrite := flag.Bool("allow-write-probe", false, "acknowledge isolated temporary writes to the share")
	seedFixture := flag.Bool("seed-fixture", false, "create the deterministic artificial fixture and exit")
	allowSeed := flag.Bool("allow-seed-fixture", false, "acknowledge exclusive creation of the artificial fixture")
	printSeedSHA := flag.Bool("print-seed-sha256", false, "print the deterministic fixture SHA-256 and exit without network access")
	flag.Parse()

	fail := func(err error) {
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"ok": false, "error": err.Error()})
		os.Exit(1)
	}
	if *printSeedSHA {
		if *configPath != "" || *source != "" || *seedFixture || *allowSeed || *allowWrite {
			fail(fmt.Errorf("--print-seed-sha256 is an offline mode and cannot be combined with config, source, seed, or write-probe flags"))
		}
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{
			"source": smbprobe.SeedSourceName,
			"bytes":  smbprobe.DeterministicFixtureSize(),
			"sha256": smbprobe.DeterministicFixtureSHA256(),
		})
		return
	}
	if *configPath == "" || *source == "" {
		fail(fmt.Errorf("--config and --source are required"))
	}
	if *seedFixture {
		if !*allowSeed {
			fail(fmt.Errorf("refusing fixture seed without --allow-seed-fixture"))
		}
		if *allowWrite {
			fail(fmt.Errorf("--allow-write-probe is not used with --seed-fixture; seeding and gate execution are separate operations"))
		}
	} else {
		if *allowSeed {
			fail(fmt.Errorf("--allow-seed-fixture requires --seed-fixture"))
		}
		if !*allowWrite {
			fail(fmt.Errorf("refusing write probe without --allow-write-probe"))
		}
	}
	data, err := os.ReadFile(*configPath)
	if err != nil {
		fail(fmt.Errorf("reading probe config: %w", err))
	}
	var cfg smbprobe.Config
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&cfg); err != nil {
		fail(fmt.Errorf("parsing probe config: %w", err))
	}
	if err := smbprobe.ValidateConfig(&cfg); err != nil {
		fail(fmt.Errorf("validating probe config: %w", err))
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if *seedFixture {
		result, seedErr := smbprobe.Seed(ctx, cfg, *source)
		if seedErr != nil {
			payload := map[string]any{"ok": false, "error": seedErr.Error(), "result": result}
			_ = json.NewEncoder(os.Stdout).Encode(payload)
			os.Exit(1)
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(result)
		return
	}
	result, err := smbprobe.Run(ctx, cfg, *source)
	if err != nil {
		payload := map[string]any{"ok": false, "error": err.Error(), "result": result}
		_ = json.NewEncoder(os.Stdout).Encode(payload)
		os.Exit(1)
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(result)
}
