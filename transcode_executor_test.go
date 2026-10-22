package main

import (
	"testing"

	"github.com/jakenesler/navigatorr/config"
	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/httpclient"
)

func TestBuildTranscodeWorkersDisabled(t *testing.T) {
	workers, err := buildTranscodeWorkers(config.TranscodeConfig{})
	if err != nil {
		t.Fatalf("disabled: unexpected error: %v", err)
	}
	if workers != nil {
		t.Fatalf("disabled: expected no workers, got %d", workers.Len())
	}
}

func TestBuildTranscodeWorkersLegacyHTTP(t *testing.T) {
	workers, err := buildTranscodeWorkers(config.TranscodeConfig{
		Enabled:  true,
		Executor: "http",
		HTTP: config.HTTPExecutorConfig{
			BaseURL: "http://127.0.0.1:8097",
		},
	})
	if err != nil {
		t.Fatalf("valid http: unexpected error: %v", err)
	}
	w, ok := workers.Get(transcode.DefaultWorkerName)
	if workers.Len() != 1 || !ok || !w.Podcast || w.Address != "127.0.0.1:8097" {
		t.Fatalf("the single http block is one podcast worker named %s: %+v", transcode.DefaultWorkerName, workers.All())
	}
	if _, ok := w.Executor.(*httpclient.HTTPExecutor); !ok {
		t.Fatalf("valid http: expected *httpclient.HTTPExecutor, got %T", w.Executor)
	}
}

func TestBuildTranscodeWorkersNamedList(t *testing.T) {
	workers, err := buildTranscodeWorkers(config.TranscodeConfig{
		Enabled:  true,
		Executor: "http",
		Workers: []config.TranscodeWorkerConfig{
			{Name: "m1", HTTPExecutorConfig: config.HTTPExecutorConfig{BaseURL: "http://192.0.2.1:8097"}},
			{Name: "m4", HTTPExecutorConfig: config.HTTPExecutorConfig{BaseURL: "http://192.0.2.2:8097"}, MaxParallelJobs: 2, Podcast: true},
		},
	})
	if err != nil {
		t.Fatalf("named list: %v", err)
	}
	all := workers.All()
	if len(all) != 2 || all[0].Name != "m1" || all[1].Name != "m4" || all[0].Podcast || !all[1].Podcast || all[1].MaxParallelJobs != 2 {
		t.Fatalf("workers in config order with their settings: %+v", all)
	}
}

func TestBuildTranscodeWorkersInvalidHTTP(t *testing.T) {
	workers, err := buildTranscodeWorkers(config.TranscodeConfig{
		Enabled:  true,
		Executor: "http",
		HTTP: config.HTTPExecutorConfig{
			BaseURL: "not-an-absolute-url",
		},
	})
	if err == nil || workers != nil {
		t.Fatalf("invalid http: expected an error and no workers, got %v, %v", workers, err)
	}
}

func TestBuildTranscodeWorkersRejectedExecutors(t *testing.T) {
	for _, executor := range []string{"ssh", "", "carrier-pigeon"} {
		workers, err := buildTranscodeWorkers(config.TranscodeConfig{Enabled: true, Executor: executor})
		if err == nil || workers != nil {
			t.Fatalf("executor %q: expected an error and no workers, got %v, %v", executor, workers, err)
		}
	}
}
