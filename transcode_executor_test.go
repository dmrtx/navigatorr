package main

import (
	"testing"

	"github.com/jakenesler/navigatorr/config"
	"github.com/jakenesler/navigatorr/transcode"
)

func TestBuildAutomaticTranscodeExecutorDisabled(t *testing.T) {
	exec, err := buildAutomaticTranscodeExecutor(config.TranscodeConfig{})
	if err != nil {
		t.Fatalf("disabled: unexpected error: %v", err)
	}
	if exec != nil {
		t.Fatalf("disabled: expected nil executor, got %T", exec)
	}
}

func TestBuildAutomaticTranscodeExecutorValidHTTP(t *testing.T) {
	exec, err := buildAutomaticTranscodeExecutor(config.TranscodeConfig{
		Enabled:  true,
		Executor: "http",
		HTTP: config.HTTPExecutorConfig{
			BaseURL: "http://127.0.0.1:8097",
		},
	})
	if err != nil {
		t.Fatalf("valid http: unexpected error: %v", err)
	}
	if exec == nil {
		t.Fatal("valid http: expected non-nil executor")
	}
	if _, ok := exec.(*transcode.HTTPExecutor); !ok {
		t.Fatalf("valid http: expected *transcode.HTTPExecutor, got %T", exec)
	}
}

func TestBuildAutomaticTranscodeExecutorInvalidHTTP(t *testing.T) {
	exec, err := buildAutomaticTranscodeExecutor(config.TranscodeConfig{
		Enabled:  true,
		Executor: "http",
		HTTP: config.HTTPExecutorConfig{
			BaseURL: "not-an-absolute-url",
		},
	})
	if err == nil {
		t.Fatalf("invalid http: expected error, got executor %T", exec)
	}
	if exec != nil {
		t.Fatalf("invalid http: expected nil executor, got %T", exec)
	}
}

func TestBuildAutomaticTranscodeExecutorSSHRejected(t *testing.T) {
	exec, err := buildAutomaticTranscodeExecutor(config.TranscodeConfig{
		Enabled:  true,
		Executor: "ssh",
	})
	if err == nil {
		t.Fatalf("ssh: expected error, got executor %T", exec)
	}
	if exec != nil {
		t.Fatalf("ssh: expected nil executor, got %T", exec)
	}
}

func TestBuildAutomaticTranscodeExecutorBlankRejected(t *testing.T) {
	exec, err := buildAutomaticTranscodeExecutor(config.TranscodeConfig{
		Enabled: true,
	})
	if err == nil {
		t.Fatalf("blank: expected error, got executor %T", exec)
	}
	if exec != nil {
		t.Fatalf("blank: expected nil executor, got %T", exec)
	}
}

func TestBuildAutomaticTranscodeExecutorUnknownRejected(t *testing.T) {
	exec, err := buildAutomaticTranscodeExecutor(config.TranscodeConfig{
		Enabled:  true,
		Executor: "carrier-pigeon",
	})
	if err == nil {
		t.Fatalf("unknown: expected error, got executor %T", exec)
	}
	if exec != nil {
		t.Fatalf("unknown: expected nil executor, got %T", exec)
	}
}
