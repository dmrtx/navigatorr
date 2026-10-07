import copy
import json
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest

import benchmark as b


def fixture():
    t = {"schema_version": 1, "source_hash": "sha256:test", "duration_ms": 6000,
         "units": [{"id": f"u{i:06d}", "start_ms": (i-1)*1000, "end_ms": i*1000,
                    "text": f"word{i}", "timing": "native_attributed_run"} for i in range(1, 7)]}
    blocks = b.windows(t, seconds=4, overlap_seconds=2)
    manifest = {"source_hash": t["source_hash"], "blocks": blocks}
    classification = {"schema_version": 1, "source_hash": t["source_hash"],
                      "blocks": [{"block_id": x["block_id"],
                                  "coverage": {k: x[k] for k in ("first_unit_id", "last_unit_id")},
                                  "decisions": [{"first_unit_id": x["first_unit_id"],
                                                 "last_unit_id": x["last_unit_id"],
                                                 "kind": "content", "reason": "Case discussion"}]}
                                 for x in blocks]}
    return t, manifest, classification


class ClassificationTests(unittest.TestCase):
    def test_overlap_windows_cover_every_unit(self):
        t, m, c = fixture()
        self.assertEqual(m["blocks"][1]["first_unit_id"], "u000003")
        plan, labels = b.consolidate(t, m, c)
        self.assertEqual(labels, ["content"]*6)
        self.assertEqual(plan["ranges"], [])

    def test_ad_ids_become_native_times_with_evidence(self):
        t, m, c = fixture()
        c["blocks"][0]["decisions"] = [
            {"first_unit_id": "u000001", "last_unit_id": "u000002", "kind": "paid_ad", "reason": "Sponsor"},
            {"first_unit_id": "u000003", "last_unit_id": "u000004", "kind": "content", "reason": "Story"}]
        plan, _ = b.consolidate(t, m, c)
        self.assertEqual([(x["start_ms"], x["end_ms"]) for x in plan["ranges"]], [(0, 2000)])
        self.assertEqual(plan["ranges"][0]["classification_ids"], ["b0001"])

    def test_missing_block_is_not_no_ads(self):
        t, m, c = fixture()
        c["blocks"].pop()
        with self.assertRaisesRegex(ValueError, "blocks"):
            b.consolidate(t, m, c)

    def test_duplicate_block_rejected(self):
        t, m, c = fixture()
        c["blocks"][1] = copy.deepcopy(c["blocks"][0])
        with self.assertRaisesRegex(ValueError, "blocks"):
            b.consolidate(t, m, c)

    def test_claimed_coverage_without_decisions_rejected(self):
        t, m, c = fixture()
        c["blocks"][0]["decisions"] = []
        with self.assertRaisesRegex(ValueError, "Incomplete"):
            b.consolidate(t, m, c)

    def test_wrong_declared_coverage_rejected(self):
        t, m, c = fixture()
        c["blocks"][0]["coverage"]["last_unit_id"] = "u000003"
        with self.assertRaisesRegex(ValueError, "coverage"):
            b.consolidate(t, m, c)

    def test_internal_gap_rejected(self):
        t, m, c = fixture()
        c["blocks"][0]["decisions"][0]["first_unit_id"] = "u000002"
        with self.assertRaisesRegex(ValueError, "gap"):
            b.consolidate(t, m, c)

    def test_invented_id_rejected(self):
        t, m, c = fixture()
        c["blocks"][0]["decisions"][0]["last_unit_id"] = "invented"
        with self.assertRaises(KeyError):
            b.consolidate(t, m, c)

    def test_out_of_block_id_rejected(self):
        t, m, c = fixture()
        c["blocks"][0]["decisions"][0]["last_unit_id"] = "u000005"
        with self.assertRaisesRegex(ValueError, "out-of-block"):
            b.consolidate(t, m, c)

    def test_overlap_conflict_blocks_render(self):
        t, m, c = fixture()
        c["blocks"][1]["decisions"][0]["kind"] = "paid_ad"
        with self.assertRaisesRegex(ValueError, "Overlap conflict"):
            b.consolidate(t, m, c)

    def test_uncertainty_requires_review(self):
        t, m, c = fixture()
        for block in c["blocks"]:
            block["decisions"][0]["kind"] = "uncertain"
        with self.assertRaisesRegex(ValueError, "review"):
            b.consolidate(t, m, c)

    def test_source_hash_mismatch_rejected(self):
        t, m, c = fixture()
        c["source_hash"] = "sha256:wrong"
        with self.assertRaisesRegex(ValueError, "identity"):
            b.consolidate(t, m, c)

    def test_duplicate_and_overlapping_transcript_rejected(self):
        t, _, _ = fixture()
        t["units"][1]["start_ms"] = 500
        with self.assertRaisesRegex(ValueError, "timing"):
            b.validate_transcript(t)
        t, _, _ = fixture()
        t["units"][1]["id"] = t["units"][0]["id"]
        with self.assertRaisesRegex(ValueError, "Duplicate"):
            b.validate_transcript(t)

    def test_excessive_cut_requires_review(self):
        t, m, c = fixture()
        for block in c["blocks"]:
            block["decisions"][0]["kind"] = "paid_ad"
        with self.assertRaisesRegex(ValueError, "35%"):
            b.consolidate(t, m, c)

    def test_invalid_window_policy_rejected(self):
        t, _, _ = fixture()
        with self.assertRaises(ValueError):
            b.windows(t, seconds=30, overlap_seconds=30)

    def test_long_gap_inside_ad_requires_review(self):
        t, m, c = fixture()
        t["duration_ms"] = 18000
        for unit in t["units"][1:]:
            unit["start_ms"] += 12000
            unit["end_ms"] += 12000
        # Keep the same authorized IDs so this exercises audio gaps, not windows.
        c["blocks"][0]["decisions"] = [
            {"first_unit_id": "u000001", "last_unit_id": "u000002", "kind": "paid_ad", "reason": "Sponsor"},
            {"first_unit_id": "u000003", "last_unit_id": "u000004", "kind": "content", "reason": "Story"}]
        with self.assertRaisesRegex(ValueError, "audio gap"):
            b.consolidate(t, m, c)


@unittest.skipUnless(shutil.which("ffmpeg") and shutil.which("ffprobe"), "Requires FFmpeg")
class RenderTests(unittest.TestCase):
    def test_no_ad_candidate_is_exact_mp3_copy_and_source_drift_blocks(self):
        with tempfile.TemporaryDirectory() as directory:
            p = Path(directory)
            source = p / "source.mp3"
            subprocess.run(["ffmpeg", "-nostdin", "-v", "error", "-f", "lavfi", "-i",
                            "sine=frequency=330:duration=6", "-c:a", "libmp3lame", str(source)], check=True)
            t, _, _ = fixture()
            t.update(source_hash=b.fingerprint(source), wall_seconds=1.0, realtime_factor=1/6,
                     duration_ms=round(float(b.probe(source)["format"]["duration"])*1000))
            transcript = p / "transcript.json"
            b.write(transcript, t)
            b.prepare(source, transcript, p / "prepared")
            block = b.read(p / "prepared" / "blocks.json")["blocks"][0]
            classification = {"schema_version": 1, "source_hash": t["source_hash"], "blocks": [{
                "block_id": block["block_id"], "coverage": {k: block[k] for k in ("first_unit_id", "last_unit_id")},
                "decisions": [{"first_unit_id": "u000001", "last_unit_id": "u000006",
                               "kind": "content", "reason": "No ads in synthetic source"}]}]}
            result = p / "classification.json"
            b.write(result, classification)
            b.render(source, transcript, p / "prepared", result, p / "output")
            self.assertEqual(b.fingerprint(source), b.fingerprint(p / "output" / "cleaned.mp3"))
            self.assertEqual(b.read(p / "output" / "validation.json")["removed_seconds"], 0)
            with source.open("ab") as media:
                media.write(b"changed")
            with self.assertRaisesRegex(ValueError, "Source changed"):
                b.render(source, transcript, p / "prepared", result, p / "drifted")

    def test_real_render_decode_duration_original_and_identity_guards(self):
        with tempfile.TemporaryDirectory() as directory:
            p = Path(directory)
            source = p / "source.mp3"
            subprocess.run(["ffmpeg", "-nostdin", "-v", "error", "-f", "lavfi", "-i",
                            "sine=frequency=440:duration=6", "-ac", "2", "-c:a", "libmp3lame", str(source)], check=True)
            original = b.fingerprint(source)
            t, _, _ = fixture()
            # Non-aligned stereo trim exercises the real M1 libmp3lame failure.
            t["units"][1]["end_ms"] = 2001
            t["units"][2]["start_ms"] = 2001
            t.update(source_hash=original, wall_seconds=1.0, realtime_factor=1/6,
                     duration_ms=round(float(b.probe(source)["format"]["duration"])*1000))
            transcript = p / "transcript.json"
            b.write(transcript, t)
            b.prepare(source, transcript, p / "prepared")
            manifest = b.read(p / "prepared" / "blocks.json")
            first = manifest["blocks"][0]
            classification = {"schema_version": 1, "source_hash": original, "blocks": [{
                "block_id": first["block_id"], "coverage": {k: first[k] for k in ("first_unit_id", "last_unit_id")},
                "decisions": [
                    {"first_unit_id": "u000001", "last_unit_id": "u000002", "kind": "paid_ad", "reason": "Known synthetic range"},
                    {"first_unit_id": "u000003", "last_unit_id": "u000006", "kind": "content", "reason": "Known retained range"}]}]}
            result = p / "classification.json"
            b.write(result, classification)
            b.render(source, transcript, p / "prepared", result, p / "output")
            validation = b.read(p / "output" / "validation.json")
            self.assertTrue(validation["technical_validation_passed"])
            self.assertFalse(validation["human_audio_review_passed"])
            self.assertEqual(validation["removed_seconds"], 2.001)
            self.assertEqual(b.fingerprint(source), original)
            self.assertTrue((p / "output" / "cleaned.mp3").exists())
            with self.assertRaisesRegex(ValueError, "overwrite"):
                b.render(source, transcript, p / "prepared", result, p / "output")
            t["units"][0]["text"] = "tampered"
            b.write(transcript, t)
            with self.assertRaisesRegex(ValueError, "Transcript changed"):
                b.render(source, transcript, p / "prepared", result, p / "tampered")


if __name__ == "__main__":
    unittest.main()
