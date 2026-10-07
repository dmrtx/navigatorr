import assert from "node:assert/strict";
import test from "node:test";
import { NavigatorrPodcastHook } from "./moonstation-hook.mjs";

test("post-download identity is stable, policy invalidates it and pending work has no publication", async () => {
  const calls = []; let digest = "policy-v1";
  const hook = new NavigatorrPodcastHook({ baseURL: "http://navigatorr", token: "test", fetchImpl: async (url, options) => { const body = options.body && JSON.parse(options.body); calls.push(body); return { ok: true, json: async () => url.endsWith("/podcasts") ? { enabled: true, policies: { genwhy: { digest, pipeline_version: 1, prompt_version: "podcast-labels-v1" } } } : body.arguments.action ? { id: "action" } : { action: { status: "waiting_decision" } } }; } });
  const episode = { podcastId: "genwhy", feedId: "same-feed", episodeId: "same-guid", sourcePath: "/audio/episode.mp3", outputDirectory: "/audio/clean", sourceSHA256: "a".repeat(64) };
  const first = await hook.submitDownloadedEpisode(episode), again = await hook.submitDownloadedEpisode(episode);
  assert.equal(first.outputPath, again.outputPath); assert.equal(await hook.publication(first), null);
  digest = "policy-v2"; const changed = await hook.submitDownloadedEpisode(episode); assert.notEqual(first.outputPath, changed.outputPath);
  const submission = calls.find(call => call?.arguments.action);
  const input = JSON.parse(submission.arguments.inputs); assert.equal(input.feed_id, "same-feed"); assert.equal(input.episode_id, "same-guid"); assert.equal(input.source_sha256, "a".repeat(64)); assert.notEqual(input.path, input.output_path);
});
test("only an attested result matching the receipt can be served by the existing feed", async () => {
  const receipt = { actionId: "action", feedId: "feed", episodeId: "guid", sourceHash: `sha256:${"a".repeat(64)}`, policyDigest: "policy", outputPath: "/audio/clean.mp3" };
  const podcast = { feed_ready: true, decode_passed: true, original_preserved: true, source_hash: receipt.sourceHash, policy_digest: "policy", feed_id: "feed", episode_id: "guid", output_path: receipt.outputPath, output_sha256: "b".repeat(64) };
  const hook = new NavigatorrPodcastHook({ baseURL: "http://navigatorr", token: "test", fetchImpl: async () => ({ ok: true, json: async () => ({ action: { status: "completed", podcast } }) }) });
  assert.equal((await hook.publication(receipt)).filePath, receipt.outputPath); podcast.source_hash = `sha256:${"c".repeat(64)}`; await assert.rejects(hook.publication(receipt), /does not match/);
});
