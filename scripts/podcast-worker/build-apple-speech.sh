#!/bin/sh
set -eu
output=${1:?usage: build-apple-speech.sh /absolute/output/apple-speech}
case "$output" in /*) ;; *) echo 'output must be absolute' >&2; exit 1 ;; esac
script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
mkdir -p "$(dirname -- "$output")"
cache=$(mktemp -d "${TMPDIR:-/tmp}/navigatorr-speech-cache.XXXXXX")
trap 'rm -rf "$cache"' EXIT
swiftc -parse-as-library -O -target arm64-apple-macos26.0 -module-cache-path "$cache" "$script_dir/AppleSpeech.swift" -o "$output"
