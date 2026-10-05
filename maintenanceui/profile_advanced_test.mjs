import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";
import vm from "node:vm";

const source = await readFile(
  new URL("./assets/app.js", import.meta.url),
  "utf8",
);
class Element {
  children = [];
  listeners = new Map();
  dataset = {};
  hidden = false;
  disabled = false;
  value = "";
  textContent = "";
  type = "";
  addEventListener(name, fn) {
    this.listeners.set(name, fn);
  }
  setAttribute() {}
  append(...children) {
    this.children.push(...children);
  }
  replaceChildren(...children) {
    this.children = children;
  }
  closest() {
    return this;
  }
  setCustomValidity(value) {
    this.validationMessage = value;
  }
  focus() {}
  close() {}
}
function harness(codec = "hevc_videotoolbox", extra = {}) {
  const elements = new Map();
  const document = {
    hidden: false,
    getElementById(id) {
      if (!elements.has(id)) elements.set(id, new Element());
      return elements.get(id);
    },
    createElement: () => new Element(),
    querySelectorAll: () => [],
    querySelector: (selector) =>
      selector === ".preservation-note"
        ? document.getElementById("note")
        : null,
  };
  const context = vm.createContext({
    document,
    URLSearchParams,
    setTimeout() {},
    clearTimeout() {},
    setInterval() {},
  });
  vm.runInContext(source.replace(/safe\(initialize\);\s*$/, ""), context);
  context.profile = {
    container: "mkv",
    video: {
      codec,
      quality: codec === "libx265" ? 23 : 65,
      ...(codec === "libx265" ? { preset: "medium" } : {}),
    },
    audio: { mode: "copy" },
    preserve: { metadata: true, chapters: true, attachments: true },
    optimization: { enabled: false },
    ...extra,
  };
  const run = (code) => vm.runInContext(code, context);
  run("setRecipeControls(profile);");
  const fields = () => {
    const walk = (el) => [el, ...el.children.flatMap(walk)];
    return walk(document.getElementById("recipe-extra-fields"));
  };
  const field = (path) => {
    const result = fields().find((el) => el.dataset.profileField === path);
    assert.ok(result, `Missing control ${path}`);
    return result;
  };
  const edit = (path, value, event = "input") => {
    const input = field(path);
    if (input.type === "checkbox") input.checked = value;
    else input.value = String(value);
    const listener = input.listeners.get(event);
    assert.ok(listener, `Missing ${event} handler for ${path}`);
    listener();
  };
  const result = () => JSON.parse(JSON.stringify(run("recipeFromControls()")));
  return { run, field, edit, result, elements, profile: context.profile };
}

test("a new VideoToolbox profile exposes typed optional controls without inserting defaults", () => {
  const h = harness();
  for (const path of [
    "video.rate_control",
    "video.average_bitrate_kbps",
    "video.max_bitrate_kbps",
    "video.spatial_aq",
    "video.qmin",
    "video.max_ref_frames",
  ])
    h.field(path);
  assert.equal(h.field("video.average_bitrate_kbps").disabled, true);
  assert.equal(h.field("video.spatial_aq").value, "");
  assert.deepEqual(h.result(), h.profile);
});

test("bitrate mode activates average/max bitrate and preserves explicit AQ/CBR switches", () => {
  const h = harness();
  h.edit("video.rate_control", "bitrate", "change");
  assert.equal(h.field("video.average_bitrate_kbps").disabled, false);
  h.edit("video.average_bitrate_kbps", "4200");
  h.edit("video.max_bitrate_kbps", "4600");
  h.edit("video.spatial_aq", "true", "change");
  h.edit("video.constant_bitrate", "false", "change");
  assert.deepEqual(h.result().video, {
    codec: "hevc_videotoolbox",
    quality: 0,
    average_bitrate_kbps: 4200,
    max_bitrate_kbps: 4600,
    spatial_aq: true,
    constant_bitrate: false,
  });
  assert.equal(h.elements.get("recipe-quality").disabled, true);
});

test("switching rate-control dimensions removes conflicting fields and recalibrates search", () => {
  const h = harness("hevc_videotoolbox", {
    optimization: {
      enabled: true,
      search: { max_candidates: 3, quality_values: [55, 65, 75] },
    },
  });
  h.edit("video.rate_control", "bitrate", "change");
  h.edit("video.max_bitrate_kbps", "5000");
  h.edit("video.constant_bitrate", "true", "change");
  assert.deepEqual(
    h.result().optimization.search.bitrate_values,
    [2800, 3500, 4200],
  );
  assert.equal(h.result().optimization.search.quality_values, undefined);
  h.edit("video.rate_control", "quality", "change");
  const p = h.result();
  assert.equal(p.video.average_bitrate_kbps, undefined);
  assert.equal(p.video.max_bitrate_kbps, undefined);
  assert.equal(p.video.constant_bitrate, undefined);
  assert.equal(p.video.quality, 65);
  assert.deepEqual(p.optimization.search.quality_values, [55, 65, 75]);
  assert.equal(p.optimization.search.bitrate_values, undefined);
});

test("optional VideoToolbox knobs retain explicit zero and support returning to automatic", () => {
  const h = harness();
  h.edit("video.b_frames", "0");
  h.edit("video.qmin", "5");
  h.edit("video.power_efficient", "false", "change");
  assert.equal(h.result().video.b_frames, 0);
  assert.equal(h.result().video.power_efficient, false);
  h.edit("video.qmin", "");
  assert.equal(h.result().video.qmin, undefined);
});

test("new x265 optimization can enable complete sampling/quality/search policies and edit targets", () => {
  const h = harness("libx265");
  h.edit("optimization.quality.vmaf.target", "97");
  h.edit("optimization.search.quality_values", "20, 24, 28");
  h.edit("optimization.enabled", true);
  const p = h.result();
  assert.equal(p.optimization.enabled, true);
  assert.equal(p.optimization.sampling.sample_count, 3);
  assert.equal(p.optimization.sampling.sample_seconds, 20);
  assert.equal(p.optimization.quality.preferred_metric, "vmaf");
  assert.equal(p.optimization.quality.vmaf.target, 97);
  assert.deepEqual(p.optimization.search.quality_values, [20, 24, 28]);
  assert.equal(p.optimization.search.max_candidates, 3);
  assert.ok(p.optimization.search.quality_values.every((n) => n <= 51));
});

test("required preservation stays readonly and legacy false policies can only be corrected", () => {
  const h = harness("libx265", {
    preserve: { metadata: false, chapters: true, attachments: false },
  });
  assert.equal(
    h.elements.get("note").textContent,
    "Unsupported policy: metadata, attachments must be enabled before saving.",
  );
  assert.equal(h.field("preserve.chapters").disabled, true);
  assert.equal(h.field("preserve.metadata").disabled, false);
  h.edit("preserve.metadata", true);
  assert.deepEqual(h.result().preserve, {
    metadata: true,
    chapters: true,
    attachments: false,
  });
  assert.equal(h.field("preserve.metadata").disabled, true);
  assert.equal(
    h.elements.get("note").textContent,
    "Unsupported policy: attachments must be enabled before saving.",
  );
  h.edit("preserve.attachments", true);
  assert.equal(
    h.elements.get("note").textContent,
    "Metadata, chapters and attachments are preserved.",
  );
  assert.deepEqual(h.result().preserve, {
    metadata: true,
    chapters: true,
    attachments: true,
  });
  h.edit("preserve.metadata", false);
  assert.equal(h.result().preserve.metadata, true);
});

test("invalid optional numbers and lists block serialization without poisoning the draft", () => {
  const h = harness();
  h.edit("video.qmin", "99");
  assert.throws(h.result, /valid range/);
  h.edit("video.qmin", "5");
  h.edit("optimization.search.quality_values", "55, nope, 75");
  assert.throws(h.result, /comma-separated/);
  h.edit("optimization.search.quality_values", "55,65,75");
  assert.equal(h.result().video.qmin, 5);
  h.edit("video.rate_control", "bitrate", "change");
  h.edit("video.average_bitrate_kbps", "");
  assert.throws(h.result, /required in bitrate mode/);
});

test("x265 offers tune but no VideoToolbox-only controls", () => {
  const h = harness("libx265");
  h.edit("video.tune", "grain", "change");
  assert.equal(h.result().video.tune, "grain");
  assert.throws(() => h.field("video.spatial_aq"), /Missing control/);
});
