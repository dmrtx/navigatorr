const $ = (id) => document.getElementById(id);
const state = {
  tab: "library",
  media: null,
  files: new Map(),
  selected: new Set(),
  libraryOffset: 0,
  jobsOffset: 0,
  recipe: null,
  recipes: [],
  recipeOffset: 0,
  detail: null,
  chunk: 0,
  stepOffset: 0,
};
const names = {
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
  transcode_batch: "Lote de serie",
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
    notify(e.message);
  }
}
async function api(path, body) {
  const res = await fetch(`/api/maintenance/${path}`, {
    method: body === undefined ? "GET" : "POST",
    headers: {
      "Content-Type": "application/json",
      "X-Navigatorr-Request": "1",
    },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  const data = await res.json();
  if (res.status === 401) {
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
  if (tab === "jobs") safe(loadJobs);
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
  info.services.forEach((s) =>
    option(
      $("service"),
      s.name,
      s.kind === "movie" ? "Películas · Radarr" : "Series · Sonarr",
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
  if (info.services.length) await loadLibrary();
  else
    $("library-items").replaceChildren(
      node(
        "p",
        "Configura Sonarr o Radarr para navegar la biblioteca. También puedes elegir un archivo en las carpetas permitidas.",
        "empty",
      ),
    );
  if (info.tools.includes("recipe_list")) await loadRecipes(true);
  controls();
}

async function loadLibrary() {
  const query = new URLSearchParams({
    service: $("service").value,
    q: $("search").value,
    offset: state.libraryOffset,
    limit: 100,
  });
  if (state.media) query.set("id", state.media.id);
  const page = await api(`library?${query}`);
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
      row.append(check);
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
  state.media = null;
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
  resetLibrary();
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
async function browse(path) {
  const entries = await tool("fs_list", { path, depth: 1, limit: 100 });
  $("file-items").replaceChildren();
  const files = Array.isArray(entries)
    ? entries
    : entries.entries || entries.files || [];
  for (const file of files)
    $("file-items").append(
      button(
        `${file.is_dir ? "↳ " : "▰ "}${file.path}`,
        () =>
          file.is_dir
            ? browse(file.path)
            : (() => {
                $("path").value = file.path;
                state.file = null;
                state.fileMedia = null;
                state.fileService = null;
                $("scope").value = "file";
                controls();
              })(),
        "recipe-button",
      ),
    );
}
$("browse-root").addEventListener("click", () =>
  safe(() => browse($("root").value)),
);
$("path").addEventListener("input", () => {
  state.file = null;
  state.fileMedia = null;
  state.fileService = null;
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
    ? `Lote: ${state.media?.title || "Selecciona una serie de Sonarr"}`
    : $("path").value || "Selecciona un archivo.";
  $("batch-options").hidden = !batch;
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
  $("enqueue").disabled = !state.info?.transcode_enabled;
  $("benchmark").disabled = !state.info?.transcode_enabled;
  $("preview").disabled = !state.info?.transcode_enabled;
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
  const batch = $("scope").value === "batch";
  let name = batch ? "transcode_batch" : "transcode_media";
  const inputs = { preserve_source_bit_depth: $("preserve-depth").checked };
  if (batch) {
    if (!state.media || $("service").value !== "sonarr")
      throw new Error("Selecciona una serie de Sonarr antes de crear un lote.");
    Object.assign(inputs, { service: "sonarr", series_id: state.media.id });
    if ($("season").value !== "") inputs.season = Number($("season").value);
    if ($("max-items").value) inputs.max_items = numeric("max-items");
    if ($("selected-only").checked) {
      if (!state.selected.size) throw new Error("Marca al menos un archivo.");
      inputs.episode_file_ids = [...state.selected];
    }
    if ($("promote-batch").checked) inputs.promote_candidates = true;
    if (mode === "preview") inputs.dry_run = true;
  } else {
    if (!$("path").value.trim())
      throw new Error("Selecciona o escribe la ruta de un archivo.");
    inputs.path = $("path").value.trim();
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
    await openJob(r.id);
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

async function loadJobs() {
  if ($("workspace").hidden) return;
  const page = await tool("action_list", {
    status: $("job-filter").value,
    limit: 25,
    offset: state.jobsOffset,
  });
  if (!Array.isArray(page))
    throw new Error("Respuesta de historial inesperada.");
  $("jobs-list").replaceChildren();
  for (const job of page.filter((j) => workflows[j.action_name])) {
    const row = node("div", "", "job-row"),
      meta = node("div", "", "job-meta");
    meta.append(
      node(
        "h3",
        job.batch?.title ||
          job.source_path?.split("/").pop() ||
          workflows[job.action_name],
      ),
    );
    if (job.source_path) meta.append(node("span", job.source_path, "metadata"));
    if (job.parent_action_id)
      meta.append(
        button(
          "Ver lote principal",
          () => openJob(job.parent_action_id),
          "text-button",
        ),
      );
    meta.append(
      node("span", names[job.status] || job.status, `badge ${job.status}`),
      node(
        "span",
        job.origin === "web"
          ? "Interfaz"
          : job.origin === "mcp"
            ? "Agente MCP"
            : "Origen histórico desconocido",
        "badge",
      ),
      node("span", job.id, "metadata"),
    );
    if (job.batch)
      meta.append(
        node(
          "span",
          `Resultado: ${job.batch.outcome || "en curso"} · ${job.batch.completed ?? 0} candidatos listos · ${job.batch.failed ?? 0} fallos`,
          "metadata",
        ),
      );
    if (job.waiting_reason || job.error)
      meta.append(node("p", job.error || job.waiting_reason, "muted"));
    const progress = node("div", "", "job-progress");
    progress.append(node("span", job.progress || "En cola", "metadata"));
    row.append(
      meta,
      progress,
      button("Ver trabajo ↗", () => openJob(job.id)),
    );
    $("jobs-list").append(row);
  }
  if (!$("jobs-list").children.length)
    $("jobs-list").append(
      node("p", "No hay trabajos en esta página.", "empty"),
    );
  $("jobs-prev").disabled = state.jobsOffset === 0;
  $("jobs-next").disabled = page.length < 25;
  $("jobs-page").textContent = `Página ${1 + state.jobsOffset / 25}`;
  $("active-count").textContent = "";
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
  await refreshDetail();
  if (!$("job-detail").open) $("job-detail").showModal();
  await loadDetail();
}
async function refreshDetail() {
  const data = await tool("action_status", { id: state.detail }),
    job = data.action || data;
  state.detailJob = job;
  const summary = $("detail-summary");
  summary.replaceChildren(
    node("p", job.id, "metadata"),
    node("span", names[job.status] || job.status, `badge ${job.status}`),
    node("p", job.error || job.waiting_reason || job.progress, "muted"),
  );
  if (job.worker) {
    const w = job.worker,
      p = Number(w.progress);
    summary.append(
      node(
        "p",
        [
          w.transcode_phase || w.benchmark_phase,
          w.queue_position ? `Cola #${w.queue_position}` : null,
          w.speed != null ? `${w.speed}×` : null,
          w.fps != null ? `${w.fps} fps` : null,
          w.progress_is_stale ? "Progreso sin actualizar" : null,
        ]
          .filter(Boolean)
          .join(" · "),
        "metadata",
      ),
    );
    if (Number.isFinite(p)) {
      const m = document.createElement("meter");
      m.min = 0;
      m.max = 100;
      m.value = p;
      m.setAttribute("aria-label", `Progreso ${p}%`);
      summary.append(m, node("span", `${p.toFixed(1)}%`, "metadata"));
    }
  }
  if (job.batch)
    summary.append(
      node(
        "p",
        `Resultado del lote: ${job.batch.outcome}. Un flujo completado puede contener archivos omitidos o fallidos.`,
        "muted",
      ),
    );
  if (job.promotion)
    summary.append(
      node(
        "p",
        `${job.promotion.original_path} → ${job.promotion.candidate_path} · ${bytes(job.promotion.original_bytes)} → ${bytes(job.promotion.candidate_bytes)}`,
        "metadata",
      ),
    );
  const controls = $("detail-controls");
  controls.replaceChildren();
  const control = async (name, args) => {
    await tool(name, { id: state.detail, ...args });
    await refreshDetail();
    await loadJobs();
  };
  for (const choice of job.waiting_options || [])
    controls.append(
      button(choice.description || choice.decision, async () => {
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
        await control("action_resume", { decision: choice.decision });
      }),
    );
  if (job.status === "failed")
    controls.append(
      button("Reintentar desde punto seguro", () =>
        control("action_retry", {}),
      ),
    );
  if (
    ["pending", "running", "waiting_external", "waiting_decision"].includes(
      job.status,
    )
  )
    controls.append(
      button("Cancelar trabajo", async () => {
        if (
          confirm("Cancelar este trabajo y detener los jobs remotos admitidos?")
        )
          await control("action_cancel", {
            reason: "Cancelled from maintenance UI",
          });
      }),
    );
  if (
    job.action_name === "transcode_batch" &&
    ["running", "waiting_external"].includes(job.status)
  )
    controls.append(
      button("Pausar admisión del lote", () =>
        control("action_resume", { decision: "pause" }),
      ),
    );
  if (
    job.action_name === "transcode_media" &&
    job.status === "completed" &&
    state.info.allow_destructive
  )
    controls.append(
      button("Preparar reemplazo aprobado", async () => {
        let context;
        try {
          context = JSON.parse(
            localStorage.getItem(`navigatorr_media:${job.id}`) || "null",
          );
        } catch {}
        if (!context) {
          const service = prompt(
            "Servicio de biblioteca (sonarr o radarr):",
            "sonarr",
          );
          if (!["sonarr", "radarr"].includes(service)) return;
          const id = Number(
            prompt(
              service === "radarr"
                ? "ID de película en Radarr:"
                : "ID de serie en Sonarr:",
            ),
          );
          if (!Number.isInteger(id) || id <= 0) return;
          context = { service, id };
        }
        const inputs = {
          transcode_action_id: job.id,
          service: context.service,
          [context.service === "radarr" ? "movie_id" : "series_id"]: context.id,
        };
        const r = await tool("action_run", {
          action: "promote_transcode_candidate",
          inputs: JSON.stringify(inputs),
          idempotency_key: submissionID(),
        });
        await openJob(r.id);
      }),
    );
}
async function loadDetail() {
  const section = $("detail-section").value;
  let data;
  if (section === "logs")
    data = await api(`logs?id=${encodeURIComponent(state.detail)}`);
  else
    data = await tool("action_detail", {
      id: state.detail,
      section,
      chunk: state.chunk,
      step_offset: state.stepOffset,
      step_limit: 10,
    });
  showData("detail-content", data);
  $("detail-next").hidden = !data.has_more;
}

async function reviewBatchPromotion(id) {
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
    $("approve-batch-review").disabled = true;
    try {
      await tool("action_resume", { id, decision: "approve" });
      $("batch-review").close();
      await refreshDetail();
      await loadJobs();
    } finally {
      $("approve-batch-review").disabled = false;
    }
  }),
);
$("close-detail").addEventListener("click", () => $("job-detail").close());
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
setInterval(() => {
  if (document.hidden || $("workspace").hidden) return;
  if (state.tab === "jobs") safe(loadJobs);
  if ($("job-detail").open) safe(refreshDetail);
}, 5000);
safe(initialize);
