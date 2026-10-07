#!/usr/bin/env python3
"""Offline, candidate-only benchmark for #93; deliberately not a job engine.

prepare writes full-transcript classification windows. render validates an
externally supplied classification, then renders and decodes a separate copy.
No API credentials, feed writes, scheduling or production worker changes.
"""
import argparse
import hashlib
import json
import math
from pathlib import Path
import shutil
import subprocess
import time

KINDS = {"paid_ad", "house_promo", "cross_promo", "content", "uncertain"}


def read(path):
    return json.loads(Path(path).read_text())


def write(path, value):
    path = Path(path)
    temp = path.with_name(path.name + ".partial")
    temp.write_text(json.dumps(value, indent=2, ensure_ascii=False) + "\n")
    temp.replace(path)


def fingerprint(path):
    h = hashlib.sha256()
    with Path(path).open("rb") as source:
        for chunk in iter(lambda: source.read(1024 * 1024), b""):
            h.update(chunk)
    return "sha256:" + h.hexdigest()


def probe(path):
    p = subprocess.run(["ffprobe", "-v", "error", "-show_entries",
                        "format=duration,size:stream=codec_type,codec_name,sample_rate,channels",
                        "-of", "json", str(path)], check=True, capture_output=True, text=True)
    result = json.loads(p.stdout)
    duration = float(result["format"]["duration"])
    if not math.isfinite(duration) or duration <= 0:
        raise ValueError("Invalid probed audio duration")
    if not any(s.get("codec_type") == "audio" for s in result["streams"]):
        raise ValueError("No audio stream")
    return result


def validate_transcript(transcript):
    if transcript.get("schema_version") != 1 or not transcript.get("units"):
        raise ValueError("Empty/unsupported transcript")
    duration = transcript["duration_ms"]
    if type(duration) is not int or duration <= 0:
        raise ValueError("Invalid transcript duration")
    ids, previous = set(), 0
    for unit in transcript["units"]:
        if not isinstance(unit["id"], str) or unit["id"] in ids:
            raise ValueError("Duplicate/invalid unit ID")
        ids.add(unit["id"])
        start, end = unit["start_ms"], unit["end_ms"]
        if (type(start) is not int or type(end) is not int
                or start < previous or end <= start or end > duration + 250):
            raise ValueError("Invalid/overlapping native timing at " + unit["id"])
        if not isinstance(unit["text"], str) or not unit["text"].strip():
            raise ValueError("Empty timed text")
        if unit.get("timing") not in {"native_attributed_run", "native_result"}:
            raise ValueError("Unknown timing granularity")
        previous = end


def windows(transcript, seconds=240, overlap_seconds=30):
    if seconds <= 0 or overlap_seconds < 0 or overlap_seconds >= seconds:
        raise ValueError("Invalid window/overlap duration")
    units = transcript["units"]
    blocks, start = [], 0
    while start < len(units):
        boundary = units[start]["start_ms"] + seconds * 1000
        end = start + 1
        while end < len(units) and units[end]["start_ms"] < boundary:
            end += 1
        blocks.append({"block_id": f"b{len(blocks)+1:04d}",
                       "first_unit_id": units[start]["id"],
                       "last_unit_id": units[end-1]["id"]})
        if end == len(units):
            break
        overlap_start = units[end-1]["end_ms"] - overlap_seconds * 1000
        next_start = end
        if overlap_seconds:
            while next_start > start + 1 and units[next_start-1]["start_ms"] >= overlap_start:
                next_start -= 1
        start = next_start
    return blocks


def prepare(source, transcript_path, workdir):
    transcript = read(transcript_path)
    validate_transcript(transcript)
    source_hash = fingerprint(source)
    if transcript["source_hash"] != source_hash:
        raise ValueError("Transcript/source hash mismatch")
    info = probe(source)
    if abs(float(info["format"]["duration"]) * 1000 - transcript["duration_ms"]) > 250:
        raise ValueError("Transcript/source duration mismatch")
    blocks = windows(transcript)
    out = Path(workdir)
    out.mkdir(parents=True, exist_ok=True)
    write(out / "blocks.json", {"schema_version": 1, "source_hash": source_hash,
          "transcript_sha256": fingerprint(transcript_path),
          "window_seconds": 240, "overlap_seconds": 30, "blocks": blocks})
    lines = [f"{u['id']} {u['text']}" for u in transcript["units"]]
    (out / "transcript-for-classifier.txt").write_text("\n".join(lines) + "\n")
    block_dir = out / "windows"
    block_dir.mkdir(exist_ok=True)
    indexes = {u["id"]: i for i, u in enumerate(transcript["units"])}
    for block in blocks:
        lo, hi = indexes[block["first_unit_id"]], indexes[block["last_unit_id"]]
        (block_dir / (block["block_id"] + ".txt")).write_text("\n".join(lines[lo:hi+1]) + "\n")
    write(out / "source.json", {"source_hash": source_hash, "probe": info})
    print(json.dumps({"units": len(lines), "blocks": len(blocks), "coverage_percent": 100,
                      "source_hash": source_hash, "duration_seconds": transcript["duration_ms"] / 1000}))


def consolidate(transcript, manifest, classification):
    """Require explicit, contiguous labels for EVERY unit of EVERY window.

    A claimed coverage field alone is insufficient. Overlap disagreement is
    always a review blocker, including disagreement between two kept kinds.
    """
    validate_transcript(transcript)
    expected_hash = transcript["source_hash"]
    if (classification.get("schema_version") != 1
            or manifest.get("source_hash") != expected_hash
            or classification.get("source_hash") != expected_hash):
        raise ValueError("Classification/manifest/source identity mismatch")
    units = transcript["units"]
    indexes = {u["id"]: i for i, u in enumerate(units)}
    expected = {b["block_id"]: b for b in manifest["blocks"]}
    actual = classification["blocks"]
    if (len(expected) != len(manifest["blocks"]) or len(actual) != len(expected)
            or {b["block_id"] for b in actual} != set(expected)):
        raise ValueError("Missing/duplicate/unexpected classification blocks")
    labels = [None] * len(units)
    evidence = [set() for _ in units]
    for block in actual:
        spec = expected[block["block_id"]]
        lo, hi = indexes[spec["first_unit_id"]], indexes[spec["last_unit_id"]]
        if block.get("coverage") != {"first_unit_id": spec["first_unit_id"], "last_unit_id": spec["last_unit_id"]}:
            raise ValueError("Incorrect declared block coverage")
        cursor = lo
        for decision in block["decisions"]:
            kind = decision["kind"]
            if kind not in KINDS or not isinstance(decision.get("reason"), str) or not decision["reason"].strip():
                raise ValueError("Invalid classification kind/evidence")
            first, last = indexes[decision["first_unit_id"]], indexes[decision["last_unit_id"]]
            if first != cursor or last < first or last > hi:
                raise ValueError("Classification has gap, overlap or out-of-block IDs")
            for i in range(first, last + 1):
                if labels[i] is not None and labels[i] != kind:
                    raise ValueError("Overlap conflict at " + units[i]["id"] + ": " + labels[i] + " vs " + kind)
                labels[i] = kind
                evidence[i].add(block["block_id"])
            cursor = last + 1
        if cursor != hi + 1:
            raise ValueError("Incomplete block; missing result is not no-ads")
    if any(label is None for label in labels):
        raise ValueError("Transcript classification coverage is incomplete")
    if "uncertain" in labels:
        raise ValueError("Uncertain labels require review before rendering")
    cuts, i = [], 0
    while i < len(units):
        if labels[i] != "paid_ad":
            i += 1
            continue
        first = i
        while i + 1 < len(units) and labels[i + 1] == "paid_ad":
            i += 1
        # A long untimed pause is not classified speech. Don't auto-remove it.
        if any(units[j + 1]["start_ms"] - units[j]["end_ms"] > 2000 for j in range(first, i)):
            raise ValueError("Ad range spans an unverified audio gap; review required")
        cuts.append({"id": f"cut-{len(cuts)+1:03d}", "kind": "paid_ad",
                     "start_ms": units[first]["start_ms"], "end_ms": units[i]["end_ms"],
                     "first_unit_id": units[first]["id"], "last_unit_id": units[i]["id"],
                     "classification_ids": sorted(set().union(*evidence[first:i+1]))})
        i += 1
    if sum(c["end_ms"] - c["start_ms"] for c in cuts) > transcript["duration_ms"] * 0.35:
        raise ValueError("More than 35% removed; review required")
    return {"schema_version": 1, "source_hash": expected_hash, "coverage_percent": 100,
            "remove_kinds": ["paid_ad"], "silence_snap": False, "ranges": cuts}, labels


def render(source, transcript_path, workdir, classification_path, output_dir):
    started = time.monotonic()
    source = Path(source).resolve()
    out = Path(output_dir).resolve()
    out.mkdir(parents=True, exist_ok=True)
    transcript, manifest = read(transcript_path), read(Path(workdir) / "blocks.json")
    classification = read(classification_path)
    if manifest["transcript_sha256"] != fingerprint(transcript_path):
        raise ValueError("Transcript changed after window preparation")
    plan, labels = consolidate(transcript, manifest, classification)
    if fingerprint(source) != plan["source_hash"]:
        raise ValueError("Source changed since transcription")
    source_info = probe(source)
    duration = float(source_info["format"]["duration"])
    if any(c["end_ms"] / 1000 > duration for c in plan["ranges"]):
        raise ValueError("Cut outside probed source duration")
    final, partial = out / "cleaned.mp3", out / "cleaned.partial.mp3"
    if final == source or partial == source or final.exists() or partial.exists():
        raise ValueError("Refusing to overwrite source/existing output")
    write(out / "cuts.json", plan)
    removed = sum((c["end_ms"] - c["start_ms"]) / 1000 for c in plan["ranges"])
    expected = duration - removed
    if plan["ranges"] or source.suffix.lower() != ".mp3":
        keeps, cursor = [], 0.0
        for cut in plan["ranges"]:
            start, end = cut["start_ms"] / 1000, cut["end_ms"] / 1000
            if start > cursor:
                keeps.append((cursor, start))
            cursor = end
        if cursor < duration:
            keeps.append((cursor, duration))
        filters = [f"[0:a:0]atrim=start={a:.3f}:end={b:.3f},asetpts=PTS-STARTPTS[k{i}]"
                   for i, (a, b) in enumerate(keeps)]
        # atrim may expose unaligned slices to libmp3lame on Apple Silicon.
        # Repack into freshly allocated MP3-sized frames, with no extra silence.
        filters.append("".join(f"[k{i}]" for i in range(len(keeps))) +
                       f"concat=n={len(keeps)}:v=0:a=1[joined]")
        filters.append("[joined]asetnsamples=n=1152:p=0[out]")
        script = out / "filtergraph.txt"
        script.write_text(";\n".join(filters))
        command = ["ffmpeg", "-nostdin", "-v", "error", "-xerror", "-n", "-i", str(source),
                   "-filter_complex", script.read_text(), "-map", "[out]", "-map_metadata", "0",
                   "-c:a", "libmp3lame", "-q:a", "2", str(partial)]
        subprocess.run(command, check=True, timeout=600)
    else:
        shutil.copyfile(source, partial)
    render_wall = time.monotonic() - started
    output_info = probe(partial)
    actual = float(output_info["format"]["duration"])
    if abs(actual - expected) > 0.25:
        raise ValueError(f"Output duration mismatch: expected {expected}, got {actual}")
    subprocess.run(["ffmpeg", "-nostdin", "-v", "error", "-xerror", "-i", str(partial),
                    "-map", "0:a:0", "-f", "null", "-"], check=True, timeout=600)
    if fingerprint(source) != plan["source_hash"]:
        raise ValueError("Source changed during rendering")
    validation = {"schema_version": 1, "source_hash": plan["source_hash"],
                  "classification_sha256": fingerprint(classification_path),
                  "cuts_sha256": fingerprint(out / "cuts.json"),
                  "output_hash": fingerprint(partial), "technical_validation_passed": True,
                  "human_audio_review_passed": False, "output_probe": output_info,
                  "expected_duration_seconds": expected, "actual_duration_seconds": actual,
                  "removed_seconds": removed, "cuts": len(plan["ranges"]),
                  "render_wall_seconds": render_wall, "total_validation_wall_seconds": time.monotonic()-started,
                  "coverage_percent": 100, "coverage_basis": "every transcript unit, not every audio second",
                  "native_transcription_wall_seconds": transcript["wall_seconds"],
                  "native_transcription_realtime_factor": transcript["realtime_factor"],
                  "source_preserved": True, "published": False}
    write(out / "validation.json", validation)
    partial.replace(final)
    print(json.dumps(validation))


def main():
    p = argparse.ArgumentParser(description=__doc__)
    sub = p.add_subparsers(dest="command", required=True)
    for name in ("prepare", "render"):
        s = sub.add_parser(name)
        s.add_argument("source")
        s.add_argument("transcript")
        s.add_argument("workdir")
        if name == "render":
            s.add_argument("classification")
            s.add_argument("output_dir")
    args = vars(p.parse_args())
    command = args.pop("command")
    # Keep CLI argument names compatible with the named function parameters.
    args["transcript_path"] = args.pop("transcript")
    if command == "render":
        args["classification_path"] = args.pop("classification")
    try:
        (prepare if command == "prepare" else render)(**args)
    except (ValueError, KeyError, OSError, subprocess.SubprocessError) as error:
        p.exit(1, f"benchmark blocked: {error}\n")


if __name__ == "__main__":
    main()
