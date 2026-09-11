# SSH transcoding

Navigatorr can coordinate a remote Apple Silicon worker over system OpenSSH. Keep deployment-specific hostnames, usernames, keys, and storage paths outside the public repository.

Example configuration:

```yaml
transcode:
  enabled: true
  executor: ssh
  default_action: manual_approval
  min_savings_percent: 15.0
  max_parallel_jobs: 1
  ssh:
    host: "192.0.2.10"
    port: 22
    user: "transcoder"
    ssh_key_path: "/run/secrets/navigatorr_transcode_ssh"
    known_hosts_path: "/run/secrets/navigatorr_known_hosts"
    remote_binary: "/opt/homebrew/bin/navigatorr-transcode"
    connect_timeout_sec: 5
    command_timeout_sec: 60
    path_mappings:
      - local_prefix: "/media"
        remote_prefix: "/Volumes/media"
```

`192.0.2.0/24` is the documentation-only TEST-NET-1 range. Replace every example value in private deployment configuration.
