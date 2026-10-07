import { createHash } from "node:crypto";
import path from "node:path";

// Thin post-download adapter: no model calls, polling loop, scheduler or RSS.
// The caller retains its existing feed, GUID and enclosure URL and uses the
// returned validated local path in that enclosure's existing audio handler.
export class NavigatorrPodcastHook {
  constructor({ baseURL, token, fetchImpl = fetch }) {
    this.baseURL = baseURL.replace(/\/$/, "");
    this.token = token;
    this.fetch = fetchImpl;
  }
  async request(endpoint, body) {
    const response = await this.fetch(`${this.baseURL}/api/maintenance/${endpoint}`, {
      method: body ? "POST" : "GET",
      headers: { Authorization: `Bearer ${this.token}`, "Content-Type": "application/json", "X-Navigatorr-Request": "1" },
      ...(body ? { body: JSON.stringify(body) } : {}),
      redirect: "error",
    });
    const result = await response.json();
    if (!response.ok) throw new Error(result.error || `Navigatorr returned HTTP ${response.status}`);
    if (!result.content) return result;
    const text = result.content.filter(item => item.type === "text").map(item => item.text).join("\n");
    if (result.isError) throw new Error(text);
    return JSON.parse(text);
  }
  async submitDownloadedEpisode({ podcastId, feedId, episodeId, sourcePath, outputDirectory, sourceSHA256 }) {
    const hash = sourceSHA256?.replace(/^sha256:/, "").toLowerCase();
    if (!/^[a-f0-9]{64}$/.test(hash || "") || !feedId || !episodeId || !sourcePath || !path.posix.isAbsolute(outputDirectory || "")) throw new Error("Downloaded episode needs identities, audio SHA-256 and an absolute output directory");
    const settings = await this.request("podcasts");
    const policy = settings.policies?.[podcastId];
    if (!settings.enabled || !policy) throw new Error("Podcast cleaning is not enabled for this podcast");
    const key = createHash("sha256").update(JSON.stringify({ podcastId, feedId, episodeId, hash, policy })).digest("hex");
    const outputPath = path.posix.join(outputDirectory, `${path.posix.basename(sourcePath)}.cleaned-${key.slice(0, 16)}.mp3`);
    const action = await this.request("tool", { name: "action_run", arguments: { action: "clean_podcast_ads", idempotency_key: `podcast-${key}`, inputs: JSON.stringify({ path: sourcePath, output_path: outputPath, source_sha256: hash, podcast_id: podcastId, feed_id: feedId, episode_id: episodeId }) }, background: true });
    return { actionId: action.id, outputPath, sourceHash: `sha256:${hash}`, policyDigest: policy.digest, feedId, episodeId };
  }
  async publication(receipt) {
    const status = await this.request("tool", { name: "action_status", arguments: { id: receipt.actionId } });
    const action = status.action;
    if (!action) throw new Error("Missing action status");
    if (["failed", "cancelled"].includes(action.status)) throw new Error(action.error || `Podcast job ${action.status}`);
    if (action.status !== "completed") return null;
    const result = action.podcast;
    if (!result?.feed_ready || !result.decode_passed || !result.original_preserved || result.source_hash !== receipt.sourceHash || result.policy_digest !== receipt.policyDigest || result.feed_id !== receipt.feedId || result.episode_id !== receipt.episodeId || result.output_path !== receipt.outputPath || !/^(sha256:)?[a-f0-9]{64}$/.test(result.output_sha256 || "")) throw new Error("Completed podcast result does not match this download receipt");
    return { filePath: result.output_path, sourceHash: result.source_hash, outputHash: result.output_sha256, feedId: receipt.feedId, episodeId: receipt.episodeId, originalPreserved: true };
  }
}
