const $ = (id) => document.getElementById(id);
const state = {
  tab: "library",
  media: null,
  files: new Map(),
  selected: new Set(),
  libraryOffset: 0,
  libraryRevision: 0,
  jobsOffset: 0,
  recipe: null,
  recipes: [],
  recipeOffset: 0,
  detail: null,
  chunk: 0,
  stepOffset: 0,
  batchItemsOffset: 0,
  jobsRevision: 0,
  detailRevision: 0,
  authRevision: 0,
  busyJobs: new Set(),
  submitting: false,
  approvingBatch: false,
};
const names = {
  queued: "En cola",
  waiting_for_slot: "Esperando turno",
  skip: "Omitido",
  review: "Requiere revisión",
  pending: "En cola",
  running: "En ejecución",
  waiting_external: "Esperando worker",
  waiting_decision: "Necesita tu decisión",
  completed: "Completado",
  failed: "Falló",
  cancelled: "Cancelado",
};
const workflows = {
  transcode_media: "Transcode de archivo",
  transcode_batch: "Lote de archivos",
  benchmark_transcode: "Benchmark",
  promote_transcode_candidate: "Reemplazo aprobado",
};
function node(tag, text = "", className = "") {
  const n = document.createElement(tag);
  n.textContent = text;
  n.className = className;
  return n;
}
function button(text, onClick, className = "") {
  const b = node("button", text, className);
  b.type = "button";
  b.addEventListener("click", () => safe(onClick));
  return b;
}
function bytes(n) {
  if (n == null) return "—";
  const v = Number(n);
  if (!Number.isFinite(v)) return "—";
  return v >= 1e9 ? `${(v / 1e9).toFixed(2)} GB` : `${(v / 1e6).toFixed(1)} MB`;
}
function notify(text) {
  $("notice").textContent = text;
  $("notice").hidden = false;
  clearTimeout(notify.timer);
  notify.timer = setTimeout(() => {
    $("notice").hidden = true;
  }, 9000);
}
async function safe(fn) {
  try {
    return await fn();
  } catch (e) {
    if (e.message !== "sign in to Navigatorr") notify(e.message);
  }
}
let serverReachable =
  typeof navigator === "undefined" || navigator.onLine !== false;
function setConnection(online) {
  serverReachable = online;
  $("connection-status").hidden = online;
  document.body?.classList.toggle("disconnected", !online);
  if (!online && $("workspace").hidden) $("login").hidden = false;
  document.querySelectorAll("[data-job-control]").forEach((button) => {
    button.disabled = !online || state.busyJobs.has(button.dataset.jobControl);
  });
  document.querySelectorAll(".dialog-connection").forEach((notice) => {
    notice.hidden = online;
  });
  $("approve-batch-review").disabled = !online || state.approvingBatch;
  controls();
}
async function reconnect() {
  await api("bootstrap");
  if ($("workspace").hidden) await initialize();
  else {
    await loadJobs();
    if ($("job-detail").open) await refreshDetail();
  }
}
$("reconnect").addEventListener("click", () => safe(reconnect));
if (typeof window !== "undefined") {
  window.addEventListener("offline", () => setConnection(false));
  window.addEventListener("online", () => safe(reconnect));
  document.addEventListener("visibilitychange", () => {
    if (!document.hidden) safe(reconnect);
  });
}
async function api(path, body) {
  if (body !== undefined && !serverReachable)
    throw new Error("Sin conexión: reconecta antes de enviar cambios.");
  let res;
  try {
    res = await fetch(`/api/maintenance/${path}`, {
      method: body === undefined ? "GET" : "POST",
      headers: {
        "Content-Type": "application/json",
        "X-Navigatorr-Request": "1",
      },
      body: body === undefined ? undefined : JSON.stringify(body),
    });
  } catch {
    setConnection(false);
    throw new Error(
      "No se puede conectar con Navigatorr. Reconecta para consultar el estado.",
    );
  }
  if ([502, 503, 504].includes(res.status)) {
    setConnection(false);
    throw new Error(
      "Navigatorr no está disponible. Reconecta para consultar el estado.",
    );
  }
  setConnection(true);
  const data = await res.json();
  if (res.status === 401) {
    state.authRevision++;
    state.libraryRevision++;
    state.jobsRevision++;
    state.detailRevision++;
    state.detail = null;
    state.batchApproval = null;
    $("job-detail").close();
    $("batch-review").close();
    $("login").hidden = false;
    $("workspace").hidden = true;
    $("logout").hidden = true;
  }
  if (!res.ok) throw new Error(data.error || `HTTP ${res.status}`);
  return data;
}
async function tool(name, args = {}) {
  const result = await api("tool", { name, arguments: args });
  if (!result.content) return result;
  const text = result.content
    .filter((c) => c.type === "text")
    .map((c) => c.text)
    .join("\n");
  if (result.isError) throw new Error(text);
  try {
    return JSON.parse(text);
  } catch {
    return text;
  }
}
function showData(id, data) {
  $(id).hidden = false;
  $(id).textContent =
    typeof data === "string" ? data : JSON.stringify(data, null, 2);
}
function submissionID() {
  const b = crypto.getRandomValues(new Uint8Array(16));
  b[6] = (b[6] & 15) | 64;
  b[8] = (b[8] & 63) | 128;
  const h = [...b].map((v) => v.toString(16).padStart(2, "0")).join("");
  return `${h.slice(0, 8)}-${h.slice(8, 12)}-${h.slice(12, 16)}-${h.slice(16, 20)}-${h.slice(20)}`;
}
function option(select, value, text = value) {
  const o = node("option", text);
  o.value = value;
  select.append(o);
}
function selectTab(tab) {
  state.tab = tab;
  document
    .querySelectorAll(".tab-content")
    .forEach((n) => (n.hidden = n.id !== tab));
  document
    .querySelectorAll("[data-tab]")
    .forEach((n) =>
      n.setAttribute("aria-selected", String(n.dataset.tab === tab)),
    );
  $("jobs").hidden = !["library", "jobs"].includes(tab);
  if (typeof window !== "undefined") window.scrollTo({ top: 0 });
  if (["library", "jobs"].includes(tab)) safe(loadJobs);
  if (tab === "recipes") safe(loadRecipes);
}
document
  .querySelectorAll("[data-tab]")
  .forEach((n) => n.addEventListener("click", () => selectTab(n.dataset.tab)));
$("login-form").addEventListener("submit", (e) => {
  e.preventDefault();
  safe(async () => {
    await api("login", { token: $("token").value });
    $("token").value = "";
    await initialize();
  });
});
$("logout").addEventListener("click", () =>
  safe(async () => {
    await api("logout", {});
    $("workspace").hidden = true;
    $("login").hidden = false;
    $("logout").hidden = true;
  }),
);

async function initialize() {
  const info = await api("bootstrap");
  state.info = info;
  $("login").hidden = true;
  $("workspace").hidden = false;
  $("logout").hidden = false;
  $("service").replaceChildren();
  info.roots.forEach((r) =>
    option($("service"), `folder:${r}`, `Carpeta · ${r.split("/").pop() || r}`),
  );
  info.services.forEach((s) =>
    option(
      $("service"),
      s.name,
      s.kind === "movie"
        ? "Películas (catálogo e importación)"
        : "Series (catálogo e importación)",
    ),
  );
  $("root").replaceChildren();
  info.roots.forEach((r) => option($("root"), r));
  $("tool-name").replaceChildren();
  info.tools.forEach((n) => option($("tool-name"), n));
  describeTool();
  document.querySelector('[data-tab="recipes"]').disabled =
    !info.tools.includes("recipe_list");
  $("promote-batch").disabled = !info.allow_destructive;
  $("enqueue").disabled = !info.transcode_enabled;
  if (!info.transcode_enabled)
    notify(
      "Transcode está desactivado en la configuración del servidor. Puedes consultar biblioteca e historial.",
    );
  resetLibrary();
  if (info.roots.length || info.services.length) await loadLibrary();
  else
    $("library-items").replaceChildren(
      node(
        "p",
        "Configura media.allowed_read_roots para navegar tus carpetas.",
        "empty",
      ),
    );
  if (info.tools.includes("recipe_list")) await loadRecipes(true);
  controls();
  if (
    typeof matchMedia === "function" &&
    matchMedia("(max-width: 600px)").matches
  )
    selectTab("jobs");
  await loadJobs();
}

async function loadLibrary() {
  const revision = ++state.libraryRevision;
  if ($("service").value.startsWith("folder:")) return loadFolder(revision);
  const query = new URLSearchParams({
    service: $("service").value,
    q: $("search").value,
    offset: state.libraryOffset,
    limit: 100,
  });
  if (state.media) query.set("id", state.media.id);
  const page = await api(`library?${query}`);
  if (revision !== state.libraryRevision) return;
  $("library-total").textContent =
    `${page.total} ${state.media ? "archivos" : "títulos"}`;
  $("library-items").replaceChildren();
  for (const item of page.items) {
    const row = node("div", "", "media-row");
    if (state.media) {
      const check = document.createElement("input");
      check.type = "checkbox";
      check.checked = state.selected.has(item.id);
      check.setAttribute(
        "aria-label",
        `Marcar ${item.relativePath || item.path}`,
      );
      check.addEventListener("change", () => {
        check.checked
          ? state.selected.add(item.id)
          : state.selected.delete(item.id);
      });
      const checkLabel = node("label", "", "media-check");
      checkLabel.append(check);
      row.append(checkLabel);
      state.files.set(item.id, item);
    } else
      row.append(
        node("span", $("service").value === "radarr" ? "▰" : "▤", "media-icon"),
      );
    const title =
      item.title || item.relativePath || item.path || `Archivo ${item.id}`;
    const b = button(
      "",
      async () => {
        if (state.media) {
          $("path").value = item.path || "";
          state.file = item;
          state.fileService = $("service").value;
          state.fileMedia = state.media;
          $("scope").value = "file";
          controls();
          notify(`Seleccionado: ${title}`);
          return;
        }
        state.media = item;
        state.libraryOffset = 0;
        state.selected.clear();
        state.files.clear();
        $("path").value = "";
        state.file = null;
        state.fileMedia = null;
        state.fileService = null;
        $("selection-title").textContent = item.title;
        $("back").hidden = false;
        $("scope").value = $("service").value === "sonarr" ? "batch" : "file";
        await loadLibrary();
        controls();
      },
      "open-media",
    );
    b.append(node("span", title, "media-title"));
    b.append(
      node(
        "span",
        [
          item.year,
          item.mediaInfo?.videoCodec,
          item.seasonNumber != null ? `Temporada ${item.seasonNumber}` : null,
          item.episodeCount != null ? `${item.episodeCount} archivos` : null,
        ]
          .filter((v) => v != null)
          .join(" · "),
        "metadata",
      ),
    );
    row.append(b, node("span", bytes(item.size), "media-size"));
    $("library-items").append(row);
  }
  if (!page.items.length)
    $("library-items").append(
      node("p", "No hay archivos o títulos que coincidan.", "empty"),
    );
  $("library-prev").disabled = state.libraryOffset === 0;
  $("library-next").disabled = !page.has_more;
  if (state.media && $("service").value === "sonarr") {
    const selectedSeason = $("season").value;
    const seasons = [
      ...new Set(
        [...(state.media.seasons || []), ...state.files.values()]
          .map((f) => f.seasonNumber)
          .filter((n) => n != null),
      ),
    ].sort((a, b) => a - b);
    $("season").replaceChildren();
    option($("season"), "", "Toda la serie");
    seasons.forEach((n) => option($("season"), n, `Temporada ${n}`));
    $("season").value = selectedSeason;
  }
}
function resetLibrary() {
  state.libraryRevision++;
  state.media = null;
  state.folder = null;
  state.folderSelected = new Set();
  state.file = null;
  state.fileMedia = null;
  state.fileService = null;
  state.libraryOffset = 0;
  state.files.clear();
  state.selected.clear();
  $("path").value = "";
  $("back").hidden = true;
  $("selection-title").textContent = "Selecciona una película o serie";
  $("scope").value = "file";
  controls();
}
$("service").addEventListener("change", () => {
  resetLibrary();
  safe(loadLibrary);
});
$("back").addEventListener("click", () => {
  if ($("service").value.startsWith("folder:")) {
    const root = $("service").value.slice(7);
    state.folder =
      state.folder === root
        ? root
        : state.folder?.slice(0, state.folder.lastIndexOf("/")) || root;
    state.folderSelected = new Set();
    state.libraryOffset = 0;
    clearFileSelection();
  } else resetLibrary();
  safe(loadLibrary);
});
$("search-button").addEventListener("click", () => {
  state.libraryOffset = 0;
  safe(loadLibrary);
});
$("search").addEventListener("keydown", (e) => {
  if (e.key === "Enter") {
    e.preventDefault();
    state.libraryOffset = 0;
    safe(loadLibrary);
  }
});
$("library-prev").addEventListener("click", () => {
  state.libraryOffset = Math.max(0, state.libraryOffset - 100);
  safe(loadLibrary);
});
$("library-next").addEventListener("click", () => {
  state.libraryOffset += 100;
  safe(loadLibrary);
});
const videoFile = (path) =>
  /\.(mkv|mp4|m4v|avi|mov|ts|m2ts|webm|mpg|mpeg)$/i.test(path);
function clearFileSelection() {
  $("path").value = "";
  state.file = null;
  state.fileMedia = null;
  state.fileService = null;
  controls();
}
async function loadFolder(revision = ++state.libraryRevision) {
  const root = $("service").value.slice(7);
  if (
    !state.folder ||
    (state.folder !== root &&
      !state.folder.startsWith(`${root.replace(/\/$/, "")}/`))
  )
    state.folder = root;
  state.folderSelected ||= new Set();
  const page = await api(
    `folder?${new URLSearchParams({ path: state.folder, q: $("search").value, offset: state.libraryOffset })}`,
  );
  if (revision !== state.libraryRevision) return;
  $("selection-title").textContent = page.path;
  $("back").hidden = state.folder === root;
  $("library-total").textContent =
    `${page.total} carpetas / vídeos · tamaños del disco`;
  $("library-items").replaceChildren();
  for (const file of page.items) {
    const row = node("div", "", "media-row");
    if (!file.is_dir) {
      const check = document.createElement("input");
      check.type = "checkbox";
      check.checked = state.folderSelected.has(file.path);
      check.setAttribute("aria-label", `Marcar ${file.path.split("/").pop()}`);
      check.addEventListener("change", () =>
        check.checked
          ? state.folderSelected.add(file.path)
          : state.folderSelected.delete(file.path),
      );
      const checkLabel = node("label", "", "media-check");
      checkLabel.append(check);
      row.append(checkLabel);
    } else row.append(node("span", "↳", "media-icon"));
    row.append(
      button(
        file.path.split("/").pop(),
        async () => {
          if (file.is_dir) {
            state.folder = file.path;
            state.folderSelected.clear();
            state.libraryOffset = 0;
            clearFileSelection();
            await loadFolder();
          } else {
            $("path").value = file.path;
            state.file = file;
            state.fileMedia = null;
            state.fileService = null;
            $("scope").value = "file";
          }
          controls();
        },
        "media-title",
      ),
      node("span", file.is_dir ? "Carpeta" : bytes(file.size), "media-size"),
    );
    $("library-items").append(row);
  }
  if (!page.items.length)
    $("library-items").append(
      node("p", "Sin vídeos o carpetas en esta selección.", "empty"),
    );
  $("library-prev").disabled = state.libraryOffset === 0;
  $("library-next").disabled = !page.has_more;
  controls();
}
async function browse(path) {
  if (
    ![...$("service").options].some(
      (o) => o.value === `folder:${$("root").value}`,
    )
  )
    return;
  $("service").value = `folder:${$("root").value}`;
  resetLibrary();
  state.folder = path;
  await loadFolder();
}
$("browse-root").addEventListener("click", () =>
  safe(() => browse($("root").value)),
);
$("path").addEventListener("input", () => {
  state.file = null;
  state.fileMedia = null;
  state.fileService = null;
  $("scope").value = "file";
  controls();
});
$("inspect").addEventListener("click", () =>
  safe(async () =>
    showData(
      "inspection",
      await tool("inspect_media", { path: $("path").value }),
    ),
  ),
);

function controls() {
  const batch = $("scope").value === "batch";
  $("source-summary").textContent = batch
    ? `Lote: ${state.folder || state.media?.title || "Selecciona una carpeta o serie"}`
    : $("path").value || "Selecciona un archivo.";
  $("batch-options").hidden = !batch;
  const folder = $("service").value.startsWith("folder:");
  $("media-kind").closest("label").hidden = !folder;
  $("season").closest("label").hidden = folder;
  $("recursive").closest("label").hidden = !folder;
  $("promote-batch").closest("label").hidden =
    folder || $("service").value !== "sonarr";
  $("priority-label").hidden =
    !batch || $("profile").value !== "auto" || $("custom").checked;
  $("custom-fields").hidden = !$("custom").checked;
  $("optimization-fields").hidden = !$("optimize").checked;
  const vt = $("encoder").value === "hevc_videotoolbox";
  $("vt-fields").hidden = !vt;
  $("x265-fields").hidden = vt;
  $("quality").max = vt ? "100" : "51";
  $("quality-label").firstChild.textContent = vt
    ? "Calidad VideoToolbox (más = mejor)"
    : "CRF x265 (menos = mejor)";
  $("quality").disabled = vt && $("rate-mode").value === "bitrate";
  $("bitrate").disabled = $("rate-mode").value !== "bitrate";
  $("preview").hidden = !batch;
  $("benchmark").hidden = batch;
  $("audio-note").hidden = $("audio").value !== "compact";
  $("enqueue").disabled =
    !state.info?.transcode_enabled || state.submitting || !serverReachable;
  $("benchmark").disabled =
    !state.info?.transcode_enabled || state.submitting || !serverReachable;
  $("preview").disabled =
    !state.info?.transcode_enabled || state.submitting || !serverReachable;
}
for (const id of [
  "scope",
  "custom",
  "optimize",
  "profile",
  "audio",
  "rate-mode",
])
  $(id).addEventListener("change", controls);
$("encoder").addEventListener("change", () => {
  $("quality").value = $("encoder").value === "libx265" ? "23" : "65";
  $("ladder").value =
    $("encoder").value === "libx265" ? "20,23,26" : "55,65,75";
  controls();
});
function numeric(id) {
  const n = Number($(id).value);
  if (!Number.isFinite(n)) throw new Error(`Valor inválido: ${id}`);
  return n;
}
function profileFromControls() {
  const vt = $("encoder").value === "hevc_videotoolbox";
  const video = { codec: $("encoder").value, quality: numeric("quality") };
  if (vt && $("rate-mode").value === "bitrate") {
    video.quality = 0;
    video.average_bitrate_kbps = numeric("bitrate");
    if ($("max-bitrate").value) video.max_bitrate_kbps = numeric("max-bitrate");
  }
  if (!vt) {
    video.preset = $("preset").value;
    if ($("tune").value) video.tune = $("tune").value;
  }
  const p = {
    container: "mkv",
    video,
    audio: { mode: $("audio").value },
    subtitles: { mode: "preserve", convert_incompatible: true },
    preserve: { metadata: true, chapters: true, attachments: true },
    resilience: {
      max_attempts: 3,
      transient_retries: 2,
      retry_backoff_seconds: [5, 20],
      max_fallbacks: 0,
      fallbacks: [],
    },
    optimization: { enabled: false },
  };
  if ($("optimize").checked) {
    const values = $("ladder")
      .value.split(",")
      .map((v) => Number(v.trim()));
    if (!values.length || values.some((v) => !Number.isInteger(v) || v <= 0))
      throw new Error("La búsqueda requiere una lista de enteros positivos.");
    const quality = {
      preferred_metric: $("metric").value === "ssim" ? "ssim" : "vmaf",
      vmaf: {
        target: numeric("vmaf-target"),
        minimum: numeric("vmaf-min"),
        marginal_tolerance: 0.5,
      },
      ssim: {
        target: numeric("ssim-target"),
        minimum: numeric("ssim-min"),
        marginal_tolerance: 0.005,
      },
    };
    if ($("final-validation").value)
      quality.final_validation = { mode: $("final-validation").value };
    p.optimization = {
      enabled: true,
      sampling: {
        strategy: "distributed",
        sample_count: numeric("sample-count"),
        sample_seconds: numeric("sample-seconds"),
      },
      quality,
      search: {
        max_candidates: values.length,
        [vt && $("rate-mode").value === "bitrate"
          ? "bitrate_values"
          : "quality_values"]: values,
      },
    };
  }
  return p;
}
$("sync-profile").addEventListener("click", () =>
  safe(() => {
    $("profile-json").value = JSON.stringify(profileFromControls(), null, 2);
  }),
);
async function submitJob(mode = "encode") {
  if (state.submitting) return;
  state.submitting = true;
  controls();
  try {
    return await buildAndSubmitJob(mode);
  } finally {
    state.submitting = false;
    controls();
  }
}
async function buildAndSubmitJob(mode = "encode") {
  const batch = $("scope").value === "batch";
  let name = batch ? "transcode_batch" : "transcode_media";
  const inputs = { preserve_source_bit_depth: $("preserve-depth").checked };
  if (batch) {
    if ($("service").value.startsWith("folder:")) {
      if ($("selected-only").checked) {
        if (!state.folderSelected?.size)
          throw new Error("Marca al menos un vídeo.");
        inputs.paths = [...state.folderSelected];
      } else {
        const selection = await api(
          `folder?${new URLSearchParams({ path: state.folder, files: "1", recursive: $("recursive").checked ? "1" : "0" })}`,
        );
        inputs.paths = selection.paths;
      }
      if (!inputs.paths.length)
        throw new Error("La carpeta no contiene vídeos.");
      inputs.media_type = $("media-kind").value;
    } else {
      if (!state.media || $("service").value !== "sonarr")
        throw new Error("Selecciona una serie o una carpeta para el lote.");
      Object.assign(inputs, { service: "sonarr", series_id: state.media.id });
      if ($("season").value !== "") inputs.season = Number($("season").value);
      if ($("selected-only").checked) {
        if (!state.selected.size) throw new Error("Marca al menos un archivo.");
        inputs.episode_file_ids = [...state.selected];
      }
      if ($("promote-batch").checked) inputs.promote_candidates = true;
    }
    if ($("max-items").value) inputs.max_items = numeric("max-items");
    if (mode === "preview") inputs.dry_run = true;
  } else {
    if (!$("path").value.trim())
      throw new Error("Selecciona o escribe la ruta de un archivo.");
    inputs.path = $("path").value.trim();
    inputs.media_type = $("media-kind").value;
    if (state.fileMedia) {
      inputs.media_type = state.fileService === "radarr" ? "movie" : "tv";
      inputs.is_anime =
        state.fileMedia.seriesType === "anime" ||
        (state.fileMedia.genres || []).some((g) => g.toLowerCase() === "anime");
    }
    if (mode === "benchmark") name = "benchmark_transcode";
  }
  if ($("custom").checked) {
    inputs.profile_config = $("profile-json").value.trim()
      ? JSON.parse($("profile-json").value)
      : profileFromControls();
    if (inputs.profile_config.optimization?.enabled)
      inputs.metric = $("metric").value;
  } else {
    inputs.profile = $("profile").value;
    if (batch && inputs.profile === "auto")
      inputs.priority = $("priority").value;
  }
  if (name !== "benchmark_transcode") {
    if ($("min-savings").value)
      inputs.min_savings_percent = numeric("min-savings");
    inputs.max_size_increase_percent = numeric("max-growth");
  }
  const request = JSON.stringify({ name, inputs });
  let receipt;
  try {
    receipt = JSON.parse(
      sessionStorage.getItem("navigatorr_submission") || "null",
    );
  } catch {}
  if (!receipt || receipt.request !== request) {
    receipt = { request, key: submissionID() };
    sessionStorage.setItem("navigatorr_submission", JSON.stringify(receipt));
  }
  $("enqueue").disabled = true;
  $("preview").disabled = true;
  $("benchmark").disabled = true;
  try {
    const r = await tool("action_run", {
      action: name,
      inputs: JSON.stringify(inputs),
      idempotency_key: receipt.key,
    });
    sessionStorage.removeItem("navigatorr_submission");
    // Context is display metadata only; actual promotion re-resolves and verifies
    // the library through the shared engine.
    if (state.fileMedia) {
      try {
        localStorage.setItem(
          `navigatorr_media:${r.id}`,
          JSON.stringify({
            service: state.fileService,
            id: state.fileMedia.id,
          }),
        );
      } catch {}
    }
    $("job-feedback").textContent = `Admitido: ${r.id}`;
    state.jobsOffset = 0;
    selectTab("jobs");
    await loadJobs();
    notify(
      `Trabajo admitido: ${r.id}. Puedes seguir preparando archivos mientras avanza.`,
    );
  } finally {
    $("preview").disabled = false;
    $("benchmark").disabled = false;
    controls();
  }
}
$("job-form").addEventListener("submit", (e) => {
  e.preventDefault();
  safe(() => submitJob());
});
$("preview").addEventListener("click", () => safe(() => submitJob("preview")));
$("benchmark").addEventListener("click", () =>
  safe(() => submitJob("benchmark")),
);

function focusedControl(container) {
  const active = document.activeElement;
  return container.contains(active) && active.dataset.jobControl
    ? { id: active.dataset.jobControl, label: active.textContent }
    : null;
}
function restoreControl(container, focus) {
  if (!focus) return;
  [...container.querySelectorAll("[data-job-control]")]
    .find(
      (b) => b.dataset.jobControl === focus.id && b.textContent === focus.label,
    )
    ?.focus({ preventScroll: true });
}
function savingsLine(savings, compact = false) {
  if (!savings) return null;
  const parts = [];
  if (savings.source_bytes != null)
    parts.push(`Origen ${bytes(savings.source_bytes)}`);
  if (savings.estimated_saved_bytes != null)
    parts.push(
      `${savings.estimate_kind === "sampled_benchmark" ? (compact ? "Estimado (muestras)" : "Ahorro estimado por muestras") : compact ? "Estimado (perfil)" : "Orientación de perfiles"} ${bytes(savings.estimated_saved_bytes)}`,
    );
  if (savings.candidate_saved_bytes != null)
    parts.push(
      savings.candidate_saved_bytes < 0
        ? `Candidato: crece ${bytes(-savings.candidate_saved_bytes)}`
        : `${compact ? "Ahorro candidato" : "Ahorro en candidato"} ${bytes(savings.candidate_saved_bytes)}`,
    );
  if (savings.realized_saved_bytes != null)
    parts.push(`Liberado ${bytes(savings.realized_saved_bytes)}`);
  if (savings.partial)
    parts.push(`Medición parcial: ${savings.measured_files || 0} archivos`);
  return parts.length
    ? node("p", parts.join(" · "), "metadata savings-line")
    : null;
}
function telemetry(job) {
  const w = job.worker;
  const result = node(
    "div",
    "",
    `job-progress ${w ? "worker-progress" : "workflow-progress"}`,
  );
  if (!w) {
    result.append(node("span", job.progress || "En cola", "metadata"));
    return result;
  }
  const phase = w.transcode_phase || w.benchmark_phase || w.phase;
  const phases = {
    queued: "En cola",
    preparing: "Preparando",
    encoding: "Codificando",
    validating: "Validando",
    publishing: "Publicando",
    completed: "Completado",
    benchmarking: "Calibrando",
  };
  const parts = [
    phases[phase] || phase,
    w.queue_position > 0 ? `Cola #${w.queue_position}` : null,
    w.speed > 0 ? `${Number(w.speed).toFixed(2)}×` : null,
    w.fps > 0 ? `${Number(w.fps).toFixed(1)} fps` : null,
    w.progress_is_stale ? "Última medición; sin actualizar" : null,
  ];
  const eta = job.savings?.eta_seconds;
  if (Number.isFinite(eta) && eta > 0 && !w.progress_is_stale)
    parts.push(`Restante aprox. ${Math.ceil(eta / 60)} min`);
  result.append(
    node("span", parts.filter(Boolean).join(" · ") || job.progress, "metadata"),
  );
  const measured = w.progress ?? w.last_known_progress?.progress;
  if (measured != null && Number.isFinite(Number(measured))) {
    const p = Math.max(0, Math.min(100, Number(measured)));
    const meter = document.createElement("meter");
    meter.min = 0;
    meter.max = 100;
    meter.value = p;
    meter.setAttribute("aria-label", `Progreso del worker ${p.toFixed(1)}%`);
    result.append(meter, node("span", `${p.toFixed(1)}%`, "metadata"));
  }
  if (w.last_progress_at)
    result.append(
      node(
        "span",
        `Medición ${new Date(w.last_progress_at).toLocaleTimeString()}`,
        "metadata progress-observed-at",
      ),
    );
  return result;
}
function renderSavings(data) {
  const savings = data.savings || {};
  $("saved-total").textContent = bytes(savings.realized_bytes ?? 0);
  $("saved-count").textContent =
    `${savings.completed_replacements || 0} reemplazos verificados`;
  $("candidate-total").textContent = bytes(
    savings.candidate_saved_bytes ?? savings.candidate_bytes ?? 0,
  );
  $("estimated-total").textContent = bytes(
    savings.estimated_saved_bytes ?? savings.estimated_bytes ?? 0,
  );
  $("savings-history").replaceChildren();
  for (const item of data.history || []) {
    const row = node("div", "", "savings-history-row");
    row.append(
      node(
        "span",
        `${new Date(item.completed_at).toLocaleString()} · ${item.source_path}`,
        "metadata",
      ),
      node(
        "strong",
        `${bytes(item.saved_bytes)} · acumulado ${bytes(item.cumulative_bytes)}`,
        "metadata",
      ),
    );
    $("savings-history").append(row);
  }
  if (!$("savings-history").children.length)
    $("savings-history").append(
      node(
        "p",
        "El trazado aparecerá al completar reemplazos verificados y retirar las copias de recuperación.",
        "muted",
      ),
    );
}
async function jobControl(id, name, args = {}) {
  if (state.busyJobs.has(id)) return;
  state.busyJobs.add(id);
  document.querySelectorAll("[data-job-control]").forEach((b) => {
    if (b.dataset.jobControl === id) b.disabled = true;
  });
  try {
    await tool(name, { id, ...args });
    if (state.detail === id && $("job-detail").open) await refreshDetail();
    await loadJobs();
  } finally {
    state.busyJobs.delete(id);
    document.querySelectorAll("[data-job-control]").forEach((b) => {
      if (b.dataset.jobControl === id) b.disabled = !serverReachable;
    });
  }
}
async function prepareReplacement(job) {
  if (state.busyJobs.has(job.id)) return;
  state.busyJobs.add(job.id);
  try {
    let integration;
    try {
      integration = JSON.parse(
        localStorage.getItem(`navigatorr_media:${job.id}`) || "null",
      );
    } catch {}
    const inputs = { transcode_action_id: job.id, service: "filesystem" };
    if (
      ["sonarr", "radarr"].includes(integration?.service) &&
      Number.isInteger(integration.id) &&
      integration.id > 0
    ) {
      inputs.service = integration.service;
      inputs[integration.service === "radarr" ? "movie_id" : "series_id"] =
        integration.id;
    }
    const result = await tool("action_run", {
      action: "promote_transcode_candidate",
      inputs: JSON.stringify(inputs),
      idempotency_key: submissionID(),
    });
    await loadJobs();
    await openJob(result.id);
  } finally {
    state.busyJobs.delete(job.id);
  }
}
function batchCounts(batch, compact = false) {
  if (compact) {
    const counts = [
      [batch.queued, "en cola"],
      [batch.running, "activos"],
      [batch.waiting_for_slot, "esperando turno"],
      [batch.completed, "listos"],
      [batch.failed, "fallos"],
      [batch.waiting_decision ?? batch.review, "requieren decisión"],
      [batch.skip, "omitidos"],
    ];
    return (
      `${batch.dry_run ? "Vista previa · " : ""}${batch.total ?? 0} archivos` +
      counts
        .filter(([count]) => count > 0)
        .map(([count, label]) => ` · ${count} ${label}`)
        .join("")
    );
  }
  return `${batch.dry_run ? "Vista previa · " : ""}${batch.total ?? 0} archivos · ${batch.queued ?? 0} en cola · ${batch.running ?? 0} activos · ${batch.waiting_for_slot ?? 0} esperando turno · ${batch.completed ?? 0} listos · ${batch.failed ?? 0} fallos · ${batch.waiting_decision ?? batch.review ?? 0} requieren decisión · ${batch.skip ?? 0} omitidos`;
}
function jobControls(job) {
  const controls = node("div", "", "job-actions");
  const add = (label, fn) => {
    const b = button(label, fn);
    b.dataset.jobControl = job.id;
    b.disabled = !serverReachable || state.busyJobs.has(job.id);
    controls.append(b);
  };
  const decisionLabels = {
    approve: "Revisar reemplazo",
    reject: "Conservar originales",
    resume: "Reanudar lote",
    retry: "Reintentar",
    pause: "Pausar lote",
    accept_loss: "Aceptar pérdida de calidad",
    abort: "Cancelar",
  };
  for (const choice of job.waiting_options || []) {
    add(
      decisionLabels[choice.decision] || choice.description || choice.decision,
      async () => {
        if (choice.decision === "approve" && job.batch?.promotion_plan_ready) {
          await reviewBatchPromotion(job.id);
          return;
        }
        if (
          ["approve", "accept_loss"].includes(choice.decision) &&
          !confirm(
            `${choice.description}\n\n${job.promotion ? `${job.promotion.original_path}\n→ ${job.promotion.candidate_path}` : job.waiting_reason}`,
          )
        )
          return;
        await jobControl(job.id, "action_resume", {
          decision: choice.decision,
        });
      },
    );
    controls.children[controls.children.length - 1].title =
      choice.description || choice.decision;
  }
  if (job.status === "failed")
    add("Reintentar", () => jobControl(job.id, "action_retry"));
  if (
    job.action_name === "transcode_batch" &&
    ["running", "waiting_external"].includes(job.status)
  )
    add("Pausar lote", () =>
      jobControl(job.id, "action_resume", { decision: "pause" }),
    );
  if (
    ["pending", "running", "waiting_external", "waiting_decision"].includes(
      job.status,
    )
  )
    add("Cancelar", async () => {
      if (
        confirm("Cancelar este trabajo y detener los jobs remotos admitidos?")
      )
        await jobControl(job.id, "action_cancel", {
          reason: "Cancelled from maintenance UI",
        });
    });
  if (
    job.action_name === "transcode_media" &&
    job.status === "completed" &&
    job.candidate_ready &&
    state.info.allow_destructive
  )
    add("Preparar reemplazo", () => prepareReplacement(job));
  return controls;
}
async function loadJobs() {
  if ($("workspace").hidden) return;
  const revision = ++state.jobsRevision;
  const query = new URLSearchParams({
    status: $("job-filter").value,
    limit: 25,
    offset: state.jobsOffset,
  });
  const data = await api(`operations?${query}`);
  if (revision !== state.jobsRevision || $("workspace").hidden) return;
  const page = data.jobs;
  if (!Array.isArray(page))
    throw new Error("Respuesta de historial inesperada.");
  renderSavings(data);
  state.operationJobs = new Map(page.map((j) => [j.id, j]));
  const focus = focusedControl($("jobs-list"));
  $("jobs-list").replaceChildren();
  for (const job of page) {
    const row = node("article", "", "job-row"),
      meta = node("div", "", "job-meta");
    meta.append(
      node(
        "h3",
        job.batch?.title ||
          job.source_path?.split("/").pop() ||
          workflows[job.action_name],
      ),
    );
    if (job.source_path)
      meta.append(node("span", job.source_path, "metadata source-path"));
    meta.append(
      node("span", names[job.status] || job.status, `badge ${job.status}`),
      node(
        "span",
        job.origin === "web"
          ? "Interfaz"
          : job.origin === "mcp"
            ? "Agente MCP"
            : "Histórico",
        "badge",
      ),
    );
    if (job.batch)
      meta.append(
        node("span", batchCounts(job.batch, true), "metadata batch-counts"),
      );
    if (job.waiting_reason || job.error)
      meta.append(node("p", job.error || job.waiting_reason, "muted"));
    const savings = savingsLine(job.savings, true);
    if (savings) meta.append(savings);
    const actions = jobControls(job);
    const detail = button(
      "Detalle",
      () => openJob(job.id),
      "quiet detail-link",
    );
    detail.title = "Ver detalle del trabajo";
    detail.setAttribute("aria-label", "Detalle");
    actions.append(detail);
    if (job.parent_action_id)
      actions.append(
        button("Ver lote", () => openJob(job.parent_action_id), "quiet"),
      );
    row.append(meta, telemetry(job), actions);
    $("jobs-list").append(row);
  }
  if (!page.length)
    $("jobs-list").append(
      node("p", "No hay trabajos para este estado.", "empty"),
    );
  restoreControl($("jobs-list"), focus);
  $("jobs-prev").disabled = state.jobsOffset === 0;
  $("jobs-next").disabled = !data.has_more;
  $("jobs-page").textContent =
    `Página ${1 + state.jobsOffset / 25} · ${data.total} trabajos`;
  $("active-count").textContent = data.active_count
    ? `(${data.active_count})`
    : "";
  $("jobs-updated").textContent =
    `Actualizado ${new Date().toLocaleTimeString()} · cada 5 s`;
}
$("refresh-jobs").addEventListener("click", () => safe(loadJobs));
$("job-filter").addEventListener("change", () => {
  state.jobsOffset = 0;
  safe(loadJobs);
});
$("jobs-prev").addEventListener("click", () => {
  state.jobsOffset = Math.max(0, state.jobsOffset - 25);
  safe(loadJobs);
});
$("jobs-next").addEventListener("click", () => {
  state.jobsOffset += 25;
  safe(loadJobs);
});
async function openJob(id) {
  state.detail = id;
  state.chunk = 0;
  state.stepOffset = 0;
  state.batchItemsOffset = 0;
  $("batch-items-panel").hidden = true;
  state.detailRevision++;
  if (!$("job-detail").open) $("job-detail").showModal();
  $("detail-summary").replaceChildren(
    node("p", "Consultando trabajo…", "muted"),
  );
  $("detail-controls").replaceChildren();
  $("detail-content").textContent = "";
  await refreshDetail();
  if (state.detail === id && $("job-detail").open) await loadDetail();
}
async function refreshDetail() {
  const id = state.detail,
    revision = ++state.detailRevision;
  const data = await api(`operations?id=${encodeURIComponent(id)}`),
    job = data.jobs?.[0];
  if (!job) throw new Error("El trabajo ya no está disponible.");
  if (
    state.detail !== id ||
    revision !== state.detailRevision ||
    !$("job-detail").open
  )
    return;
  const logsOption = $("detail-section").querySelector?.(
    'option[value="logs"]',
  );
  if (logsOption) {
    logsOption.disabled = !job.logs_available;
    if (!job.logs_available && $("detail-section").value === "logs")
      $("detail-section").value = "outputs";
  }
  state.detailJob = job;
  const focus = focusedControl($("detail-controls"));
  const summary = $("detail-summary");
  summary.replaceChildren(
    node("p", job.id, "metadata"),
    node("span", names[job.status] || job.status, `badge ${job.status}`),
    node("p", job.error || job.waiting_reason || job.progress, "muted"),
    telemetry(job),
  );
  if (job.batch) summary.append(node("p", batchCounts(job.batch), "muted"));
  if (job.source_path) summary.append(node("p", job.source_path, "metadata"));
  if (job.promotion)
    summary.append(
      node(
        "p",
        `${job.promotion.original_path} → ${job.promotion.candidate_path} · ${bytes(job.promotion.original_bytes)} → ${bytes(job.promotion.candidate_bytes)}`,
        "metadata",
      ),
    );
  const savings = savingsLine(job.savings);
  if (savings) summary.append(savings);
  const controls = $("detail-controls");
  controls.replaceChildren(jobControls(job));
  $("batch-items-panel").hidden = !job.batch;
  if (job.batch) await loadBatchItems(id, revision, job.batch.total);
  restoreControl(controls, focus);
}
async function loadBatchItems(id, revision) {
  const offset = state.batchItemsOffset;
  const response = await api(
    `batch-items?${new URLSearchParams({ id, offset, limit: 25 })}`,
  );
  if (
    id !== state.detail ||
    revision !== state.detailRevision ||
    offset !== state.batchItemsOffset ||
    !$("job-detail").open
  )
    return;
  $("batch-items-list").replaceChildren();
  for (const item of response.items || []) {
    const row = node("div", "", "batch-item-row");
    row.append(
      node("span", item.display_label || item.file_path, "metadata"),
      node("span", names[item.status] || item.status || item.decision, "badge"),
    );
    if (item.child_action_id)
      row.append(
        button("Ver archivo", () => openJob(item.child_action_id), "quiet"),
      );
    if (item.error) row.append(node("span", item.error, "muted"));
    $("batch-items-list").append(row);
  }
  $("batch-items-note").textContent =
    `${response.items?.length || 0} de ${response.total} archivos · página ${1 + offset / 25}`;
  $("batch-items-prev").disabled = offset === 0;
  $("batch-items-next").disabled = !response.has_more;
}
$("batch-items-prev").addEventListener("click", () => {
  state.batchItemsOffset = Math.max(0, state.batchItemsOffset - 25);
  safe(() => loadBatchItems(state.detail, state.detailRevision));
});
$("batch-items-next").addEventListener("click", () => {
  state.batchItemsOffset += 25;
  safe(() => loadBatchItems(state.detail, state.detailRevision));
});
async function loadDetail() {
  const id = state.detail,
    section = $("detail-section").value,
    chunk = state.chunk,
    stepOffset = state.stepOffset;
  let data;
  if (section === "logs") data = await api(`logs?id=${encodeURIComponent(id)}`);
  else
    data = await tool("action_detail", {
      id,
      section,
      chunk,
      step_offset: stepOffset,
      step_limit: 10,
    });
  if (
    state.detail !== id ||
    $("detail-section").value !== section ||
    state.chunk !== chunk ||
    !$("job-detail").open
  )
    return;
  showData("detail-content", data);
  $("detail-next").hidden = !data.has_more;
}

async function reviewBatchPromotion(id) {
  const authRevision = state.authRevision;
  const args = { id, section: "state", key: "batch_promotion_plan", chunk: 0 };
  const first = await tool("action_detail", args);
  let plan = first.data;
  if (first.total_chunks) {
    if (first.total_chunks > 128)
      throw new Error(
        "El plan excede el límite del visor. Consulta el detalle completo antes de aprobar mediante MCP.",
      );
    let content = first.content;
    for (let chunk = 1; chunk < first.total_chunks; chunk++)
      content += (await tool("action_detail", { ...args, chunk })).content;
    plan = JSON.parse(content);
  }
  if (authRevision !== state.authRevision || $("workspace").hidden) return;
  if (
    plan?.batch_id !== id ||
    !plan.digest ||
    !Array.isArray(plan.members) ||
    !plan.members.length
  )
    throw new Error("No se pudo verificar la lista de reemplazos del lote.");
  state.batchApproval = id;
  $("batch-review-data").textContent =
    `${plan.members.length} reemplazos · ${plan.digest}\n\n` +
    plan.members
      .map(
        (m) =>
          `${m.episode || m.item_key}\n${m.original_path}\n→ ${m.candidate_path}`,
      )
      .join("\n\n");
  $("batch-review").showModal();
}
$("dismiss-batch-review").addEventListener("click", () =>
  $("batch-review").close(),
);
$("approve-batch-review").addEventListener("click", () =>
  safe(async () => {
    const id = state.batchApproval;
    state.approvingBatch = true;
    $("approve-batch-review").disabled = true;
    try {
      await tool("action_resume", { id, decision: "approve" });
      $("batch-review").close();
      await refreshDetail();
      await loadJobs();
    } finally {
      state.approvingBatch = false;
      $("approve-batch-review").disabled = !serverReachable;
    }
  }),
);
$("close-detail").addEventListener("click", () => $("job-detail").close());
$("job-detail").addEventListener("close", () => state.detailRevision++);
$("load-detail").addEventListener("click", () => safe(loadDetail));
$("detail-section").addEventListener("change", () => {
  state.chunk = 0;
  state.stepOffset = 0;
  safe(loadDetail);
});
$("detail-next").addEventListener("click", () => {
  state.chunk++;
  state.stepOffset += 10;
  safe(loadDetail);
});

async function loadRecipes(initial = false, more = false) {
  if (!more) {
    state.recipeOffset = 0;
    state.recipes = [];
  }
  const page = await tool("recipe_list", {
    offset: state.recipeOffset,
    limit: 100,
  });
  const names = [
    ...new Set([
      ...(page.active_bundle_profiles || []),
      ...(page.static_config_profiles || []),
      ...(page.managed_profiles || []).map((p) => p.name),
    ]),
  ].sort();
  state.recipes = [...new Set([...state.recipes, ...names])].sort();
  state.recipeOffset += page.managed_returned || 0;
  $("recipes-more").hidden = state.recipeOffset >= (page.managed_total || 0);
  const selected = $("profile").value;
  $("profile").replaceChildren();
  option($("profile"), "auto", "Automático");
  state.recipes.forEach((n) => option($("profile"), n));
  $("profile").value = selected || "auto";
  $("recipes-list").replaceChildren();
  state.recipes.forEach((n) =>
    $("recipes-list").append(button(n, () => readRecipe(n), "recipe-button")),
  );
  if (initial) return;
}
$("recipes-more").addEventListener("click", () =>
  safe(() => loadRecipes(false, true)),
);
async function readRecipe(name) {
  const r = await tool("recipe_get", { name });
  state.recipe = r;
  $("recipe-name").value = name;
  $("recipe-description").value = r.record?.description || "";
  $("recipe-body").value = JSON.stringify(r.profile, null, 2);
  $("recipe-source").textContent =
    `Origen: ${r.source}${r.shadowed_profiles?.length ? " · Este override oculta un perfil base" : ""}`;
  $("delete-recipe").disabled = r.source !== "managed";
  showData("recipe-feedback", r.compact_audio_policy || {});
}
$("new-recipe").addEventListener("click", () =>
  safe(() => {
    state.recipe = null;
    $("recipe-name").value = "";
    $("recipe-description").value = "";
    $("recipe-source").textContent = "Perfil nuevo";
    $("recipe-body").value = JSON.stringify(profileFromControls(), null, 2);
    $("delete-recipe").disabled = true;
  }),
);
$("recipe-form").addEventListener("submit", (e) => {
  e.preventDefault();
  safe(async () => {
    const args = {
      name: $("recipe-name").value.trim(),
      description: $("recipe-description").value,
      profile: JSON.parse($("recipe-body").value),
    };
    if (state.recipe?.record && state.recipe.name === args.name) {
      args.expected_generation = state.recipe.record.generation;
      args.expected_digest = state.recipe.record.digest;
    }
    await tool("recipe_save", args);
    state.recipeOffset = 0;
    state.recipes = [];
    await loadRecipes();
    await readRecipe(args.name);
    notify("Perfil guardado. Los trabajos en curso conservan sus parámetros.");
  });
});
$("delete-recipe").addEventListener("click", () =>
  safe(async () => {
    const r = state.recipe;
    if (
      !r?.record ||
      !confirm(
        `Eliminar el override ${r.name}? Los trabajos en curso conservan sus parámetros.`,
      )
    )
      return;
    await tool("recipe_delete", {
      name: r.name,
      expected_generation: r.record.generation,
      expected_digest: r.record.digest,
    });
    state.recipe = null;
    state.recipeOffset = 0;
    state.recipes = [];
    await loadRecipes();
    notify("Override eliminado.");
  }),
);
$("recipe-history").addEventListener("click", () =>
  safe(async () =>
    showData(
      "recipe-feedback",
      await tool("recipe_history", { name: $("recipe-name").value }),
    ),
  ),
);
$("tool-form").addEventListener("submit", (e) => {
  e.preventDefault();
  safe(async () => {
    const name = $("tool-name").value,
      args = JSON.parse($("tool-args").value);
    if (!args || Array.isArray(args) || typeof args !== "object")
      throw new Error("Los argumentos deben ser un objeto JSON.");
    if (name === "action_run" && !args.idempotency_key)
      args.idempotency_key = submissionID();
    if (
      ["recipe_delete", "recipe_update", "recipe_rollback"].includes(name) ||
      args.decision === "approve" ||
      args.decision === "accept_loss"
    ) {
      if (
        !confirm(
          `Ejecutar ${name} con estos argumentos?\n${JSON.stringify(args, null, 2)}`,
        )
      )
        return;
    }
    showData("tool-output", await tool(name, args));
  });
});

function describeTool() {
  const definition = state.info?.tool_definitions?.find(
    (t) => t.name === $("tool-name").value,
  );
  $("tool-description").textContent = definition?.description || "";
  $("tool-schema").textContent = JSON.stringify(
    definition?.inputSchema || {},
    null,
    2,
  );
}
$("tool-name").addEventListener("change", describeTool);
let polling = false;
setInterval(async () => {
  if (polling || !serverReachable || document.hidden || $("workspace").hidden)
    return;
  polling = true;
  try {
    if (["library", "jobs"].includes(state.tab)) await safe(loadJobs);
    if ($("job-detail").open) await safe(refreshDetail);
  } finally {
    polling = false;
  }
}, 5000);
if (typeof window !== "undefined") setConnection(serverReachable);
safe(initialize);
