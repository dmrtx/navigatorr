package config

import (
	"strings"
	"testing"
)

func TestTranscodeWorkersList(t *testing.T) {
	p := writeHTTPConfig(t, "workers.yaml", `
transcode:
  enabled: true
  workers:
    - name: m1
      base_url: "http://192.0.2.1:8097"
      token_file: "/run/secrets/tok"
      path_mappings:
        - local_prefix: "/media"
          remote_prefix: "/Volumes/media"
    - name: m4
      base_url: "http://192.0.2.2:8097"
      token_file: "/run/secrets/tok"
      max_parallel_jobs: 2
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	workers, err := cfg.Transcode.BuildWorkerConfigs()
	if err != nil {
		t.Fatal(err)
	}
	if len(workers) != 2 || workers[0].Name != "m1" || workers[1].Name != "m4" {
		t.Fatalf("workers in config order: %+v", workers)
	}
	if !workers[0].Podcast || workers[1].Podcast {
		t.Fatal("without a podcast worker named, the first one runs podcast work")
	}
	if workers[1].MaxParallelJobs != 2 || workers[1].HTTP.BaseURL != "http://192.0.2.2:8097" || len(workers[0].HTTP.PathMappings) != 1 {
		t.Fatalf("worker settings: %+v", workers)
	}
}

func TestTranscodeWorkersValidation(t *testing.T) {
	worker := func(name, url, extra string) string {
		return "    - name: " + name + "\n      base_url: \"" + url + "\"\n" + extra
	}
	list := func(workers ...string) string {
		return "transcode:\n  enabled: true\n  workers:\n" + strings.Join(workers, "")
	}
	for _, tc := range []struct {
		name, content, wantErr string
	}{
		{"both forms", "transcode:\n  enabled: true\n  http:\n    base_url: \"http://192.0.2.1:8097\"\n  workers:\n" + worker("m4", "http://192.0.2.2:8097", ""), "not both"},
		{"duplicate name", list(worker("m1", "http://192.0.2.1:8097", ""), worker("m1", "http://192.0.2.2:8097", "")), "used twice"},
		{"invalid name", list(worker("M1 node", "http://192.0.2.1:8097", "")), "invalid worker name"},
		{"missing name", list(worker("", "http://192.0.2.1:8097", "")), "invalid worker name"},
		{"same base_url", list(worker("m1", "http://192.0.2.1:8097", ""), worker("m4", "http://192.0.2.1:8097/", "")), "same base_url"},
		{"bad url", list(worker("m1", "notaurl", "")), "absolute http(s) URL"},
		{"negative cap", list(worker("m1", "http://192.0.2.1:8097", "      max_parallel_jobs: -1\n")), "must not be negative"},
		{"two podcast workers", list(worker("m1", "http://192.0.2.1:8097", "      podcast: true\n"), worker("m4", "http://192.0.2.2:8097", "      podcast: true\n")), "only one worker may set podcast"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeHTTPConfig(t, "case.yaml", tc.content))
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}
