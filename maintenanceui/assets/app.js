const $ = (id) => document.getElementById(id);
const state = {
  tab: "library",
  media: null,
  files: new Map(),
  selected: new Set(),
  libraryOffset: 0,
  libraryRevision: 0,
  jobsLoaded: 0,
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
  authMode: "token",
  busyJobs: new Set(),
  submitting: false,
  approvingBatch: false,
  fileStep: "browse",
};
const names = {
  queued: "Queued",
  waiting_for_slot: "Waiting for slot",
  skip: "Skipped",
  review: "Needs review",
  pending: "Queued",
  running: "Running",
  waiting_external: "Waiting for worker",
  waiting_decision: "Needs decision",
  completed: "Completed",
  failed: "Failed",
  cancelled: "Cancelled",
};
const workflows = {
  transcode_media: "File transcode",
  transcode_batch: "File batch",
  benchmark_transcode: "Benchmark",
  promote_transcode_candidate: "Replacement",
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
function actionIcon(label) {
  if (typeof document.createElementNS !== "function") return null;
  const paths = {
    Retry: "M20 7v5h-5M4 17v-5h5M6 6a8 8 0 0 1 14 6M18 18A8 8 0 0 1 4 12",
    Review: "M5 4h14v16H5ZM8 8h8M8 12h8",
    Cancel: "M6 6l12 12M6 18L18 6",
    "Replace file": "M4 7h16M16 3l4 4-4 4M20 17H4M8 13l-4 4 4 4",
    "Review replacements": "M9 12l2 2 4-4M5 4h14v16H5Z",
    "Keep originals": "M5 4h14v16H5ZM8 8h8M8 12h8",
    "Pause batch": "M8 4v16M16 4v16",
    "Resume batch": "M7 4l13 8-13 8Z",
    Folder: "M3 6h7l2 2h9v12H3Z",
  };
  if (!paths[label]) return null;
  const svg = document.createElementNS("http://www.w3.org/2000/svg", "svg");
  for (const [name, value] of Object.entries({
    viewBox: "0 0 24 24",
    fill: "none",
    stroke: "currentColor",
    "stroke-width": "1.7",
    "stroke-linecap": "round",
    "stroke-linejoin": "round",
    "aria-hidden": "true",
  }))
    svg.setAttribute(name, value);
  const path = document.createElementNS("http://www.w3.org/2000/svg", "path");
  path.setAttribute("d", paths[label]);
  svg.append(path);
  return svg;
}

function bytes(n) {
  if (n == null) return "—";
  const v = Number(n);
  if (!Number.isFinite(v)) return "—";
  if (v >= 1e9) return `${(v / 1e9).toFixed(2)} GB`;
  if (v >= 1e6) return `${(v / 1e6).toFixed(1)} MB`;
  if (v >= 1e3) return `${(v / 1e3).toFixed(1)} KB`;
  return `${Math.round(v)} B`;
}
function notify(text) {
  const notice = $("notice");
  const dialog = document.querySelector?.("dialog[open]");
  (dialog || document.querySelector?.("main"))?.append(notice);
  notice.className = dialog ? "dialog-notice" : "";
  notice.textContent = text;
  notice.hidden = false;
  clearTimeout(notify.timer);
  notify.timer = setTimeout(() => {
    $("notice").hidden = true;
  }, 9000);
}
document.querySelectorAll("dialog").forEach((dialog) => {
  dialog.addEventListener("close", () => {
    if (dialog.contains($("notice"))) {
      document.querySelector?.("main")?.append($("notice"));
      $("notice").className = "";
    }
  });
});
async function safe(fn) {
  try {
    return await fn();
  } catch (e) {
    if (serverReachable && e.message !== "sign in to Navigatorr")
      notify(e.message);
  }
}
let serverReachable =
  typeof navigator === "undefined" || navigator.onLine !== false;
function setConnection(online) {
  serverReachable = online;
  $("connection-status").hidden = online;
  document.body?.classList.toggle("disconnected", !online);
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
    if (state.tab === "library" && state.libraryError) await loadLibrary();
    if ($("job-detail").open) await refreshDetail();
  }
}
if (typeof window !== "undefined") {
  window.addEventListener("offline", () => setConnection(false));
  window.addEventListener("online", () => safe(reconnect));
  document.addEventListener("visibilitychange", () => {
    if (!document.hidden) safe(reconnect);
  });
}
function invalidateAuthentication() {
  state.authRevision++;
  state.libraryRevision++;
  state.jobsRevision++;
  state.detailRevision++;
  state.detail = null;
  state.batchApproval = null;
  state.replacementChoice = null;
  $("job-detail").close();
  $("batch-review").close();
  $("path-dialog").close();
  $("recipe-settings").close();
  $("replacement-choice").close();
  $("login").hidden = state.authMode !== "token";
  $("access-expired").hidden = state.authMode !== "cloudflare_access";
  $("workspace").hidden = true;
  $("logout").hidden = true;
  $("local-session").hidden = true;
}
function authenticationMessage() {
  return state.authMode === "cloudflare_access"
    ? "Cloudflare Access sign-in required. Reload to sign in."
    : "sign in to Navigatorr";
}
async function api(path, body) {
  if (body !== undefined && !serverReachable)
    throw new Error(
      "Offline. Changes are unavailable until the server responds.",
    );
  let res;
  try {
    res = await fetch(`/api/maintenance/${path}`, {
      method: body === undefined ? "GET" : "POST",
      redirect: "manual",
      headers: {
        "Content-Type": "application/json",
        "X-Navigatorr-Request": "1",
      },
      body: body === undefined ? undefined : JSON.stringify(body),
    });
  } catch {
    setConnection(false);
    throw new Error("Cannot connect to Navigatorr. Retrying automatically.");
  }
  if ([502, 503, 504].includes(res.status)) {
    setConnection(false);
    throw new Error("Navigatorr is unavailable. Retrying automatically.");
  }
  setConnection(true);
  const redirected =
    res.type === "opaqueredirect" ||
    res.redirected === true ||
    (res.status >= 300 && res.status < 400);
  if (redirected) {
    // Access can intercept auth-info before the console knows its auth mode.
    // Never follow its login chain inside an API request or parse login HTML.
    if (state.authMode === "cloudflare_access" || path === "auth-info") {
      state.authMode = "cloudflare_access";
      invalidateAuthentication();
      throw new Error(authenticationMessage());
    }
    throw new Error("Unexpected API redirect. Reload the application.");
  }
  const contentType = res.headers?.get("Content-Type") || "";
  if (
    state.authMode === "cloudflare_access" &&
    res.status === 403 &&
    /text\/html/i.test(contentType)
  ) {
    invalidateAuthentication();
    throw new Error(authenticationMessage());
  }
  if (res.status === 401) {
    invalidateAuthentication();
    // Invalidate before parsing: an HTML denial must close stale approvals too.
    let denial;
    try {
      denial = await res.json();
    } catch {}
    throw new Error(denial?.error || authenticationMessage());
  }
  const data = await res.json();
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

  if (typeof window !== "undefined") window.scrollTo({ top: 0 });
  if (["jobs", "stats"].includes(tab)) safe(loadJobs);
  if (tab === "recipes")
    safe(async () => {
      await loadRecipes();
      if (!state.recipe && state.recipes.length)
        await readRecipe(state.recipes[0]);
    });
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
  const auth = await api("auth-info");
  state.authMode = auth.auth_mode;
  $("logout").hidden = state.authMode !== "token";
  $("local-session").hidden = state.authMode !== "token";
  const info = await api("bootstrap");
  state.info = info;
  $("login").hidden = true;
  $("workspace").hidden = false;
  $("logout").hidden = state.authMode !== "token";
  $("access-expired").hidden = true;
  $("service").replaceChildren();
  info.roots.forEach((r) =>
    option($("service"), `folder:${r}`, `Folder · ${r.split("/").pop() || r}`),
  );
  info.services.forEach((s) =>
    option($("service"), s.name, s.kind === "movie" ? "Movies" : "TV"),
  );
  $("root").replaceChildren();
  info.roots.forEach((r) => option($("root"), r));

  document.querySelector('[data-tab="recipes"]').disabled =
    !info.tools.includes("recipe_list");
  $("promote-batch").disabled = !info.allow_destructive;
  $("enqueue").disabled = !info.transcode_enabled;
  if (!info.transcode_enabled)
    notify("Transcoding is disabled. You can still browse files and jobs.");
  resetLibrary();
  if (info.roots.length || info.services.length) await loadLibrary();
  else
    $("library-items").replaceChildren(
      node("p", "No media folders configured.", "empty"),
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

async function loadLibrary(more = false) {
  const revision = ++state.libraryRevision;
  state.libraryLoading = true;
  if (!more) {
    state.libraryHasMore = false;
    $("library-more").hidden = true;
    $("library-items").replaceChildren(node("p", "Loading files…", "muted"));
  }
  controls();
  try {
    if (!more) state.libraryOffset = 0;
    else state.libraryOffset = state.libraryLoaded || 0;

    if ($("service").value.startsWith("folder:"))
      return await loadFolder(revision, more);
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
      `${page.total} ${state.media ? "files" : "titles"}`;
    if (!more) $("library-items").replaceChildren();
    for (const item of page.items) {
      const row = node("div", "", "media-row");
      if (state.media) {
        const check = document.createElement("input");
        check.type = "checkbox";
        check.checked = state.selected.has(item.id);
        check.setAttribute(
          "aria-label",
          `Select ${item.relativePath || item.path}`,
        );
        check.addEventListener("change", () => {
          check.checked
            ? state.selected.add(item.id)
            : state.selected.delete(item.id);
          controls();
        });
        const checkLabel = node("label", "", "media-check");
        checkLabel.append(check);
        row.append(checkLabel);
        state.files.set(item.id, item);
      } else
        row.append(
          node(
            "span",
            $("service").value === "radarr" ? "▰" : "▤",
            "media-icon",
          ),
        );
      const title =
        item.title || item.relativePath || item.path || `File ${item.id}`;
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
            setFileStep("configure");
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
            item.seasonNumber != null ? `Season ${item.seasonNumber}` : null,
            item.episodeCount != null ? `${item.episodeCount} files` : null,
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
        node("p", "No matching files or titles.", "empty"),
      );
    state.libraryLoaded = state.libraryOffset + page.items.length;
    state.libraryHasMore = page.has_more;
    state.libraryError = false;
    $("library-more").hidden = !page.has_more;
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
      option($("season"), "", "All seasons");
      seasons.forEach((n) => option($("season"), n, `Season ${n}`));
      $("season").value = selectedSeason;
    }
  } catch (error) {
    if (
      revision === state.libraryRevision &&
      !$("service").value.startsWith("folder:")
    )
      libraryFailure(error, more);
    throw error;
  } finally {
    if (revision === state.libraryRevision) {
      state.libraryLoading = false;
      controls();
    }
  }
}
function libraryFailure(error, more) {
  state.libraryError = true;
  const host = $("library-items");
  if (!more) host.replaceChildren();
  host.append(
    node("p", error.message || "Could not load files.", "muted"),
    button("Retry", () => loadLibrary(more), "quiet"),
  );
}
function resetLibrary() {
  setFileStep("browse");
  state.libraryRevision++;
  state.libraryLoaded = 0;
  state.libraryHasMore = false;
  $("library-more").hidden = true;
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
  $("selection-title").textContent = "Choose a movie or series";
  $("selection-title").hidden = false;
  $("folder-breadcrumbs").hidden = true;
  $("scope").value = "file";
  $("library-items").replaceChildren();
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
$("toggle-library-search").addEventListener("click", () => {
  const open = $("library-search-label").hidden;
  $("library-search-label").hidden = !open;
  $("search-button").hidden = !open;
  $("toggle-library-search").setAttribute("aria-expanded", String(open));
  $("toggle-library-search").setAttribute(
    "aria-label",
    open ? "Close search" : "Show search",
  );
  $("toggle-library-search").title = open ? "Close search" : "Search";
  $("toggle-library-search").classList?.toggle("search-open", open);
  if (open) $("search").focus();
  else {
    $("search").value = "";
    state.libraryOffset = 0;
    safe(loadLibrary);
  }
});
$("search").addEventListener("keydown", (e) => {
  if (e.key === "Enter") {
    e.preventDefault();
    state.libraryOffset = 0;
    safe(loadLibrary);
  }
});
async function loadMoreLibrary() {
  if (state.libraryLoading || !state.libraryHasMore || !serverReachable) return;
  state.libraryLoading = true;
  $("library-more").disabled = true;
  try {
    await loadLibrary(true);
  } finally {
    state.libraryLoading = false;
    $("library-more").disabled = false;
  }
}
$("library-more").addEventListener("click", () => safe(loadMoreLibrary));
if (typeof IntersectionObserver !== "undefined") {
  new IntersectionObserver(
    (entries) => {
      if (entries.some((e) => e.isIntersecting) && state.tab === "library")
        safe(loadMoreLibrary);
    },
    { rootMargin: "200px" },
  ).observe($("library-sentinel"));
}
const videoFile = (path) =>
  /\.(mkv|mp4|m4v|avi|mov|ts|m2ts|webm|mpg|mpeg)$/i.test(path);
function clearFileSelection() {
  $("path").value = "";
  state.file = null;
  state.fileMedia = null;
  state.fileService = null;
  controls();
}
async function navigateFolder(path) {
  state.folder = path;
  state.folderSelected = new Set();
  state.libraryOffset = 0;
  $("search").value = "";
  clearFileSelection();
  await loadFolder();
}
function folderBreadcrumbs(root, path) {
  const host = $("folder-breadcrumbs");
  host.hidden = false;
  $("selection-title").hidden = true;
  host.replaceChildren();
  const parts =
    path === root
      ? []
      : path.slice(root.replace(/\/$/, "").length + 1).split("/");
  let current = root;
  const add = (label, target, last) => {
    const b = button(label, () => navigateFolder(target), "breadcrumb-button");
    b.title = target;
    if (last) b.setAttribute("aria-current", "location");
    host.append(b);
  };
  add(root.split("/").filter(Boolean).pop() || "/", root, parts.length === 0);
  parts.forEach((part, index) => {
    current = `${current.replace(/\/$/, "")}/${part}`;
    host.append(node("span", "/", "breadcrumb-separator"));
    add(part, current, index === parts.length - 1);
  });
}
async function loadFolder(revision = ++state.libraryRevision, more = false) {
  state.libraryLoading = true;
  if (!more) {
    state.libraryHasMore = false;
    $("library-more").hidden = true;
    $("library-items").replaceChildren(node("p", "Loading files…", "muted"));
  }
  controls();
  try {
    if (!more) state.libraryOffset = 0;
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
    $("selection-title").textContent =
      page.path === root
        ? root.split("/").pop() || root
        : page.path.slice(root.length + 1);
    $("selection-title").title = page.path;
    folderBreadcrumbs(root, page.path);
    $("back").hidden = state.folder === root;
    $("library-total").textContent =
      `${page.total} item${page.total === 1 ? "" : "s"}`;
    if (!more) $("library-items").replaceChildren();
    for (const file of page.items) {
      const row = node("div", "", "media-row");
      if (!file.is_dir) {
        const check = document.createElement("input");
        check.type = "checkbox";
        check.checked = state.folderSelected.has(file.path);
        check.setAttribute(
          "aria-label",
          `Select ${file.path.split("/").pop()}`,
        );
        check.addEventListener("change", () => {
          check.checked
            ? state.folderSelected.add(file.path)
            : state.folderSelected.delete(file.path);
          controls();
        });
        const checkLabel = node("label", "", "media-check");
        checkLabel.append(check);
        row.append(checkLabel);
      } else {
        const icon = node("span", "", "media-icon");
        icon.append(actionIcon("Folder") || node("span", "↳"));
        row.append(icon);
      }
      row.append(
        button(
          file.path.split("/").pop(),
          async () => {
            if (file.is_dir) {
              await navigateFolder(file.path);
            } else {
              $("path").value = file.path;
              state.file = file;
              state.fileMedia = null;
              state.fileService = null;
              $("scope").value = "file";
              setFileStep("configure");
            }
            controls();
          },
          "media-title",
        ),
        node("span", file.is_dir ? "Folder" : bytes(file.size), "media-size"),
      );
      $("library-items").append(row);
    }
    if (!page.items.length)
      $("library-items").append(
        node("p", "No videos or folders found.", "empty"),
      );
    state.libraryLoaded = state.libraryOffset + page.items.length;
    state.libraryHasMore = page.has_more;
    state.libraryError = false;
    $("library-more").hidden = !page.has_more;
    controls();
  } catch (error) {
    if (revision === state.libraryRevision) libraryFailure(error, more);
    throw error;
  } finally {
    if (revision === state.libraryRevision) {
      state.libraryLoading = false;
      controls();
    }
  }
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
  safe(async () => {
    await browse($("root").value);
    $("path-dialog").close();
  }),
);
$("path").addEventListener("input", () => {
  state.file = null;
  state.fileMedia = null;
  state.fileService = null;
  $("scope").value = "file";
  controls();
});
$("inspect").addEventListener("click", () =>
  safe(async () => {
    let report;
    try {
      report = await tool("inspect_media", { path: $("path").value });
    } catch (error) {
      if (/no such file or directory/i.test(error.message))
        throw new Error("File not found. Choose an existing file.");
      throw error;
    }
    showData(
      "inspection",
      [
        report.container,
        report.video_codec,
        report.resolution,
        report.bit_depth ? `${report.bit_depth}-bit` : null,
        bytes(report.size_bytes),
        report.duration_sec
          ? `${Math.round(report.duration_sec / 60)} min`
          : null,
      ]
        .filter(Boolean)
        .join(" · "),
    );
    $("path-dialog").close();
    setFileStep("configure");
  }),
);

function setFileStep(step) {
  const previous = state.fileStep;
  state.fileStep = step;
  $("library").dataset.fileStep = step;
  if (
    typeof matchMedia === "function" &&
    matchMedia("(max-width: 900px)").matches
  ) {
    $("library").scrollIntoView?.({ block: "start" });
    if (step === "configure") $("back-to-files").focus();
    else if (previous === "configure")
      ($("configure-selection").hidden
        ? $("service")
        : $("configure-selection")
      ).focus();
  }
}
function configureSelection() {
  const folder = $("service").value.startsWith("folder:");
  const selected = folder
    ? [...(state.folderSelected || [])]
    : [...state.selected];
  if (selected.length > 1) {
    $("scope").value = "batch";
    $("selected-only").checked = true;
  } else if (selected.length === 1) {
    const file = folder ? { path: selected[0] } : state.files.get(selected[0]);
    if (!file?.path) throw new Error("Choose a file with a valid path.");
    $("path").value = file.path;
    state.file = file;
    state.fileMedia = folder ? null : state.media;
    state.fileService = folder ? null : $("service").value;
    $("scope").value = "file";
  } else if (!$("path").value.trim()) {
    throw new Error("Choose a file first.");
  }
  controls();
  setFileStep("configure");
}
$("configure-selection").addEventListener("click", () =>
  safe(configureSelection),
);
$("back-to-files").addEventListener("click", () => setFileStep("browse"));
$("use-container").addEventListener("click", () => {
  $("scope").value = "batch";
  $("selected-only").checked = false;
  controls();
  setFileStep("configure");
});

function controls() {
  const batch = $("scope").value === "batch";
  $("source-summary").textContent = batch
    ? `Batch: ${state.folder?.split("/").filter(Boolean).pop() || state.media?.title || "Choose a folder or series"}`
    : $("path").value.split("/").pop() || "Choose a file.";
  $("batch-options").hidden = !batch;
  const folder = $("service").value.startsWith("folder:");
  const selectedCount = folder
    ? state.folderSelected?.size || 0
    : state.selected.size;
  $("selection-count").textContent = selectedCount
    ? `${selectedCount} selected`
    : "";
  $("configure-selection").hidden = !selectedCount && !$("path").value.trim();
  $("configure-selection").disabled = state.libraryLoading;
  $("use-container").hidden = folder
    ? !state.folder
    : $("service").value !== "sonarr" || !state.media;
  $("use-container").disabled = state.libraryLoading;
  $("use-container").textContent = folder ? "Use folder" : "Use series";
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
    ? "Quality (higher is better)"
    : "CRF (lower is better)";
  $("quality").disabled = vt && $("rate-mode").value === "bitrate";
  $("bitrate").disabled = $("rate-mode").value !== "bitrate";
  $("max-bitrate").disabled = $("rate-mode").value !== "bitrate";
  $("preview").hidden = !batch;
  $("benchmark").hidden = batch;
  $("audio-note").hidden = $("audio").value !== "compact";
  const hasSource = batch
    ? Boolean(folder ? state.folder : state.media)
    : Boolean($("path").value.trim());
  $("enqueue").disabled =
    !state.info?.transcode_enabled ||
    state.submitting ||
    state.libraryLoading ||
    !serverReachable ||
    !hasSource;
  $("benchmark").disabled =
    !state.info?.transcode_enabled ||
    state.submitting ||
    state.libraryLoading ||
    !serverReachable ||
    !hasSource;
  $("preview").disabled =
    !state.info?.transcode_enabled ||
    state.submitting ||
    state.libraryLoading ||
    !serverReachable ||
    !hasSource;
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
  if (!Number.isFinite(n)) throw new Error(`Invalid value: ${id}`);
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
      throw new Error("Search values must be positive integers.");
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
          throw new Error("Select at least one video.");
        inputs.paths = [...state.folderSelected];
      } else {
        const selection = await api(
          `folder?${new URLSearchParams({ path: state.folder, files: "1", recursive: $("recursive").checked ? "1" : "0" })}`,
        );
        inputs.paths = selection.paths;
      }
      if (!inputs.paths.length)
        throw new Error("No videos found in this folder.");
      inputs.media_type = $("media-kind").value;
    } else {
      if (!state.media || $("service").value !== "sonarr")
        throw new Error("Choose a series or folder for the batch.");
      Object.assign(inputs, { service: "sonarr", series_id: state.media.id });
      if ($("season").value !== "") inputs.season = Number($("season").value);
      if ($("selected-only").checked) {
        if (!state.selected.size) throw new Error("Select at least one file.");
        inputs.episode_file_ids = [...state.selected];
      }
      if ($("promote-batch").checked) inputs.promote_candidates = true;
    }
    if ($("max-items").value) inputs.max_items = numeric("max-items");
    if (mode === "preview") inputs.dry_run = true;
  } else {
    if (!$("path").value.trim())
      throw new Error("Choose a file or enter its path.");
    inputs.path = $("path").value.trim();
    inputs.media_type = $("media-kind").value;
    if (state.fileMedia) {
      inputs.media_type = state.fileService === "radarr" ? "movie" : "tv";
      inputs.is_anime =
        state.fileMedia.seriesType === "anime" ||
        (state.fileMedia.genres || []).some((g) => g.toLowerCase() === "anime");
    }
    if (mode === "benchmark") name = "benchmark_transcode";
    else if (state.fileMedia)
      inputs.library_context = {
        service: state.fileService,
        id: state.fileMedia.id,
      };
  }
  if ($("custom").checked) {
    inputs.profile_config = profileFromControls();
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
    $("job-feedback").textContent = `Queued: ${r.id}`;
    state.jobsLoaded = 0;
    selectTab("jobs");
    await loadJobs();
    notify(`Job queued: ${r.id}.`);
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
function savingsLine(savings, compact = false, replaced = false) {
  if (!savings) return null;
  const parts = [];
  if (savings.source_bytes != null)
    parts.push(`Source ${bytes(savings.source_bytes)}`);
  if (!replaced && savings.estimated_saved_bytes != null)
    parts.push(
      `${savings.estimate_kind === "sampled_benchmark" ? (compact ? "Estimate (samples)" : "Sample estimate") : compact ? "Estimate (profile)" : "Profile estimate"} ${bytes(savings.estimated_saved_bytes)}`,
    );
  if (
    !replaced &&
    savings.candidate_saved_bytes != null &&
    savings.realized_saved_bytes == null
  )
    parts.push(
      savings.candidate_saved_bytes < 0
        ? `Candidate grows by ${bytes(-savings.candidate_saved_bytes)}`
        : `${compact ? "Potential savings" : "Potential savings"} ${bytes(savings.candidate_saved_bytes)}`,
    );
  if (savings.realized_saved_bytes != null)
    parts.push(`Freed ${bytes(savings.realized_saved_bytes)}`);
  if (savings.partial)
    parts.push(`Partial measurement: ${savings.measured_files || 0} files`);
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
    result.append(node("span", job.progress || "Queued", "metadata"));
    return result;
  }
  const phase = w.transcode_phase || w.benchmark_phase || w.phase;
  const phases = {
    queued: "Queued",
    preparing: "Preparing",
    encoding: "Encoding",
    validating: "Validating",
    publishing: "Publishing",
    completed: "Completed",
    benchmarking: "Calibrating",
  };
  const parts = [
    phases[phase] || phase,
    w.queue_position > 0 ? `Queue #${w.queue_position}` : null,
    w.speed > 0 ? `${Number(w.speed).toFixed(2)}×` : null,
    w.fps > 0 ? `${Number(w.fps).toFixed(1)} fps` : null,
    w.progress_is_stale ? "Last measurement; stale" : null,
  ];
  const eta = job.savings?.eta_seconds;
  if (Number.isFinite(eta) && eta > 0 && !w.progress_is_stale)
    parts.push(`About ${Math.ceil(eta / 60)} min`);
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
    meter.setAttribute("aria-label", `Worker progress ${p.toFixed(1)}%`);
    result.append(meter, node("span", `${p.toFixed(1)}%`, "metadata"));
  }
  if (w.last_progress_at)
    result.append(
      node(
        "span",
        `Measured ${new Date(w.last_progress_at).toLocaleTimeString()}`,
        "metadata progress-observed-at",
      ),
    );
  return result;
}
function renderSavings(data) {
  const savings = data.savings || {};
  $("saved-total").textContent = bytes(savings.realized_bytes ?? 0);
  $("saved-count").textContent =
    `${savings.completed_replacements || 0} verified replacement${savings.completed_replacements === 1 ? "" : "s"}`;
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
        `${item.source_path?.split("/").pop() || "Replacement"} · ${new Date(item.completed_at).toLocaleDateString("en")}`,
        "metadata",
      ),
      node(
        "strong",
        `${bytes(item.saved_bytes)} · total ${bytes(item.cumulative_bytes)}`,
        "metadata",
      ),
    );
    $("savings-history").append(row);
  }
  if (!$("savings-history").children.length)
    $("savings-history").append(
      node("p", "No verified replacements yet.", "muted"),
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
  if (job.replacement_action_id) {
    await openJob(job.replacement_action_id);
    return;
  }
  const durable = job.replacement_context;
  if (
    durable?.service === "filesystem" ||
    (["sonarr", "radarr"].includes(durable?.service) &&
      Number.isInteger(
        durable[durable.service === "radarr" ? "movie_id" : "series_id"],
      ) &&
      durable[durable.service === "radarr" ? "movie_id" : "series_id"] > 0)
  ) {
    await enqueueReplacement(job, durable);
    return;
  }
  let legacy;
  try {
    legacy = JSON.parse(
      localStorage.getItem(`navigatorr_media:${job.id}`) || "null",
    );
  } catch {}
  const integrations = (state.info.services || []).filter((entry) =>
    ["sonarr", "radarr"].includes(entry.name),
  );
  if (!integrations.length && !["sonarr", "radarr"].includes(legacy?.service)) {
    await enqueueReplacement(job, { service: "filesystem" });
    return;
  }
  // Old browser-only hints cannot decide how an agent-created or historical
  // candidate is imported. Ask once, then show the engine's exact approval plan.
  state.replacementChoice = { job, legacy };
  const method = $("replacement-method");
  method.replaceChildren();
  option(method, "filesystem", "Replace local file");
  for (const integration of integrations)
    option(
      method,
      integration.name,
      integration.name === "radarr"
        ? "Import through Radarr"
        : "Import through Sonarr",
    );
  method.value = integrations.some((entry) => entry.name === legacy?.service)
    ? legacy.service
    : "filesystem";
  $("replacement-source").textContent = job.source_path || job.id;
  $("prepare-replacement-choice").dataset.jobControl = job.id;
  $("prepare-replacement-choice").disabled = !serverReachable;
  updateReplacementChoice();
  if (!$("replacement-choice").open) $("replacement-choice").showModal();
}
function updateReplacementChoice() {
  const service = $("replacement-method").value;
  $("replacement-library-id-label").hidden = service === "filesystem";
  const legacy = state.replacementChoice?.legacy;
  $("replacement-library-id").value =
    service === legacy?.service && Number.isInteger(legacy.id) && legacy.id > 0
      ? String(legacy.id)
      : "";
  $("replacement-library-id").setAttribute(
    "aria-label",
    service === "radarr" ? "Movie ID" : "Series ID",
  );
}
async function enqueueReplacement(job, context) {
  if (state.busyJobs.has(job.id)) return;
  state.busyJobs.add(job.id);
  try {
    const latest = (await api(`operations?id=${encodeURIComponent(job.id)}`))
      .jobs?.[0];
    if (latest?.replacement_action_id) {
      $("replacement-choice").close();
      await openJob(latest.replacement_action_id);
      return;
    }
    if (!latest?.candidate_ready)
      throw new Error(
        "This candidate is no longer ready for replacement. Refresh the queue.",
      );
    const inputs = { transcode_action_id: job.id, service: context.service };
    if (["sonarr", "radarr"].includes(context.service)) {
      const key = context.service === "radarr" ? "movie_id" : "series_id";
      if (!Number.isInteger(context[key]) || context[key] <= 0)
        throw new Error("Choose a positive library ID.");
      inputs[key] = context[key];
    } else if (context.service !== "filesystem")
      throw new Error("Choose a supported replacement method.");
    const result = await tool("action_run", {
      action: "promote_transcode_candidate",
      inputs: JSON.stringify(inputs),
      idempotency_key: submissionID(),
    });
    $("replacement-choice").close();
    await loadJobs();
    await openJob(result.id);
  } finally {
    state.busyJobs.delete(job.id);
  }
}
$("replacement-method").addEventListener("change", updateReplacementChoice);
$("dismiss-replacement-choice").addEventListener("click", () =>
  $("replacement-choice").close(),
);
$("replacement-choice").addEventListener("close", () => {
  state.replacementChoice = null;
});
$("prepare-replacement-choice").addEventListener("click", () =>
  safe(async () => {
    const selection = state.replacementChoice;
    if (!selection) return;
    const service = $("replacement-method").value;
    const context = { service };
    if (service !== "filesystem") {
      const id = Number($("replacement-library-id").value);
      if (!Number.isInteger(id) || id <= 0)
        throw new Error(
          "Enter a positive library ID before reviewing the replacement.",
        );
      context[service === "radarr" ? "movie_id" : "series_id"] = id;
    }
    await enqueueReplacement(selection.job, context);
  }),
);
function batchCounts(batch, compact = false) {
  if (compact) {
    const counts = [
      [batch.queued, "queued"],
      [batch.running, "active"],
      [batch.waiting_for_slot, "waiting for slot"],
      [batch.completed, "completed"],
      [batch.failed, "failed"],
      [batch.waiting_decision ?? batch.review, "need a decision"],
      [batch.skip, "skipped"],
    ];
    return (
      `${batch.dry_run ? "Preview · " : ""}${batch.total ?? 0} files` +
      counts
        .filter(([count]) => count > 0)
        .map(([count, label]) => ` · ${count} ${label}`)
        .join("")
    );
  }
  return `${batch.dry_run ? "Preview · " : ""}${batch.total ?? 0} files · ${batch.queued ?? 0} queued · ${batch.running ?? 0} active · ${batch.waiting_for_slot ?? 0} waiting for slot · ${batch.completed ?? 0} completed · ${batch.failed ?? 0} failed · ${batch.waiting_decision ?? batch.review ?? 0} need a decision · ${batch.skip ?? 0} skipped`;
}
function jobControls(job) {
  const controls = node("div", "", "job-actions");
  const add = (label, fn) => {
    const b = button(label, fn);
    const icon = actionIcon(label);
    if (icon) b.append(icon);
    b.dataset.jobControl = job.id;
    b.disabled = !serverReachable || state.busyJobs.has(job.id);
    controls.append(b);
  };
  const decisionLabels = {
    approve: "Review replacements",
    reject: "Keep originals",
    resume: "Resume batch",
    retry: "Retry",
    pause: "Pause batch",
    accept_loss: "Accept quality loss",
    abort: "Cancel",
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
  if (job.status === "waiting_decision" && !job.waiting_options?.length)
    add("Review", () => openJob(job.id));
  if (job.status === "failed")
    add("Retry", () => jobControl(job.id, "action_retry"));
  if (
    job.action_name === "transcode_batch" &&
    ["running", "waiting_external"].includes(job.status)
  )
    add("Pause batch", () =>
      jobControl(job.id, "action_resume", { decision: "pause" }),
    );
  if (
    ["pending", "running", "waiting_external", "waiting_decision"].includes(
      job.status,
    )
  )
    add("Cancel", async () => {
      if (confirm("Cancel this job and stop its remote work?"))
        await jobControl(job.id, "action_cancel", {
          reason: "Cancelled from maintenance UI",
        });
    });
  if (
    job.action_name === "transcode_media" &&
    job.status === "completed" &&
    job.candidate_ready &&
    !job.replacement_action_id &&
    state.info.allow_destructive
  )
    add("Replace file", () => prepareReplacement(job));
  if (job.replacement_action_id)
    add("View replacement", () => openJob(job.replacement_action_id));
  return controls;
}
function shortJobReason(reason) {
  const text = String(reason || "")
    .replace(/\s+/g, " ")
    .trim();
  if (/no such file or directory/i.test(text)) return "File not found";
  if (/permission denied/i.test(text)) return "Permission denied";
  return text.length > 140 ? `${text.slice(0, 137)}…` : text;
}
function queuePresentation(job) {
  const kind = job.batch
    ? "Batch"
    : job.action_name === "promote_transcode_candidate"
      ? "Replacement"
      : job.action_name === "benchmark_transcode"
        ? "Benchmark"
        : "File";
  const source = job.source_path || job.promotion?.original_path;
  const title =
    job.batch?.title ||
    source?.split("/").pop() ||
    workflows[job.action_name] ||
    job.action_name;
  const outcomes = {
    preview: "Preview",
    partial: "Partial",
    needs_review: "Needs review",
    no_changes: "No changes",
    promoted: "Replaced",
    cancelling: "Cancelling",
  };
  const status = job.replaced
    ? "Replaced"
    : outcomes[job.batch?.outcome] ||
      (job.candidate_ready && !job.replacement_action_id
        ? "Candidate ready"
        : names[job.status] || job.status);
  const s = job.savings || {},
    metrics = [];
  if (s.source_bytes != null) {
    metrics.push(
      s.candidate_bytes != null && !job.replaced
        ? `${bytes(s.source_bytes)} → ${bytes(s.candidate_bytes)}`
        : `${bytes(s.source_bytes)} source`,
    );
  }
  if (s.realized_saved_bytes != null)
    metrics.push(`${bytes(s.realized_saved_bytes)} freed`);
  else if (!job.replaced && s.candidate_saved_bytes != null)
    metrics.push(
      s.candidate_saved_bytes < 0
        ? `${bytes(-s.candidate_saved_bytes)} larger`
        : `${bytes(s.candidate_saved_bytes)} potential savings`,
    );
  else if (!job.replaced && s.estimated_saved_bytes != null)
    metrics.push(`${bytes(s.estimated_saved_bytes)} estimated savings`);
  let result;
  if (job.status === "failed")
    result = shortJobReason(job.error) || "Error details unavailable";
  else if (job.replaced) result = "Original replaced";
  else if (job.batch?.promotion_plan_ready)
    result = "Replacement approval required";
  else if (job.batch) result = batchCounts(job.batch, true);
  else if (job.status === "waiting_decision")
    result = shortJobReason(job.waiting_reason) || "Review required";
  else if (job.status === "cancelled") result = "Stopped";
  const summary = [kind, result, metrics.join(" · ")]
    .filter(Boolean)
    .join(" · ");
  return {
    title,
    status,
    summary,
    showTelemetry:
      Boolean(job.worker) &&
      ["running", "waiting_external"].includes(job.status),
  };
}
async function loadJobs(more = false) {
  if (
    $("workspace").hidden ||
    (more && (state.jobsLoading || !state.jobsHasMore))
  )
    return;
  const revision = ++state.jobsRevision;
  state.jobsLoading = true;
  $("jobs-more").disabled = true;
  const status = $("job-filter").value;
  const offset = more ? state.jobsLoaded || 0 : 0;
  const target = more ? 25 : Math.max(25, state.jobsLoaded || 25);
  let data,
    page = [];
  try {
    // The server bounds each page to 100. Refresh the complete visible window.
    while (page.length < target) {
      data = await api(
        `operations?${new URLSearchParams({ status, limit: Math.min(100, target - page.length), offset: offset + page.length })}`,
      );
      if (revision !== state.jobsRevision || $("workspace").hidden) return;
      if (!Array.isArray(data.jobs))
        throw new Error("Unexpected jobs response.");
      page.push(...data.jobs);
      if (!data.has_more || !data.jobs.length) break;
    }
    renderSavings(data);
    if (!more) state.operationJobs = new Map();
    state.operationJobs ||= new Map();
    for (const job of page) state.operationJobs.set(job.id, job);
    state.jobsLoaded = offset + page.length;
    state.jobsHasMore = data.has_more;
    const focus = focusedControl($("jobs-list"));
    $("jobs-list").replaceChildren();
    for (const job of state.operationJobs.values()) {
      const row = node("article", "", "job-row"),
        meta = node("div", "", "job-meta");
      const presentation = queuePresentation(job);
      const heading = node("h3", "", "job-heading");
      heading.append(
        button(presentation.title, () => openJob(job.id), "job-title"),
      );
      heading.append(node("span", presentation.status, `badge ${job.status}`));
      const summary = node("p", presentation.summary, "metadata job-summary");
      summary.title = presentation.summary;
      meta.append(heading, summary);
      const actions = jobControls(job);
      if (job.parent_action_id)
        actions.append(
          button("View batch", () => openJob(job.parent_action_id), "quiet"),
        );
      row.append(meta);
      if (actions.children.length) {
        if (actions.children.length > 1)
          actions.classList.add("multiple-actions");
        row.append(actions);
      }
      if (presentation.showTelemetry) row.append(telemetry(job));
      $("jobs-list").append(row);
    }
    if (!state.operationJobs.size)
      $("jobs-list").append(node("p", "No jobs for this filter.", "empty"));
    restoreControl($("jobs-list"), focus);

    $("jobs-more").hidden = !data.has_more;
    $("jobs-page").textContent =
      `${state.operationJobs.size} of ${data.total} jobs`;
    $("active-count").textContent = data.active_count
      ? `(${data.active_count})`
      : "";
    $("jobs-updated").textContent =
      `Updated ${new Date().toLocaleTimeString("en")} · live`;
  } finally {
    if (revision === state.jobsRevision) {
      state.jobsLoading = false;
      $("jobs-more").disabled = false;
    }
  }
}
$("refresh-jobs").addEventListener("click", () => safe(() => loadJobs()));
$("job-filter").addEventListener("change", () => {
  state.jobsLoaded = 0;
  state.jobsHasMore = false;
  safe(() => loadJobs());
});
$("jobs-more").addEventListener("click", () => safe(() => loadJobs(true)));
if (typeof IntersectionObserver !== "undefined") {
  new IntersectionObserver(
    (entries) => {
      if (
        entries.some((e) => e.isIntersecting) &&
        state.tab === "jobs" &&
        serverReachable
      )
        safe(() => loadJobs(true));
    },
    { rootMargin: "200px" },
  ).observe($("jobs-sentinel"));
}
async function openJob(id) {
  state.detail = id;
  state.chunk = 0;
  state.stepOffset = 0;
  state.batchItemsOffset = 0;
  state.batchItemsPaging = null;
  state.batchItemsHasMore = false;
  $("batch-items-panel").hidden = true;
  state.detailRevision++;
  if (!$("job-detail").open) $("job-detail").showModal();
  $("detail-summary").replaceChildren(node("p", "Loading job…", "muted"));
  $("detail-controls").replaceChildren();
  $("detail-content").textContent = "";
  $("detail-content").hidden = true;
  await refreshDetail();
}
async function refreshDetail() {
  const id = state.detail,
    revision = ++state.detailRevision;
  try {
    const data = await api(`operations?id=${encodeURIComponent(id)}`),
      job = data.jobs?.[0];
    if (!job) throw new Error("This job is no longer available.");
    if (
      state.detail !== id ||
      revision !== state.detailRevision ||
      !$("job-detail").open
    )
      return;
    $("load-detail").hidden = !job.logs_available;
    state.detailJob = job;
    const focus = focusedControl($("detail-controls"));
    const summary = $("detail-summary");
    summary.replaceChildren(
      node(
        "p",
        job.batch?.title || job.source_path?.split("/").pop() || job.id,
        "metadata",
      ),
      node(
        "span",
        job.replaced ? "Replaced" : names[job.status] || job.status,
        `badge ${job.status}`,
      ),
      node("p", job.error || job.waiting_reason || job.progress, "muted"),
    );
    if (job.worker) summary.append(telemetry(job));
    if (job.batch)
      summary.append(node("p", batchCounts(job.batch, true), "muted"));
    if (job.source_path) summary.append(node("p", job.source_path, "metadata"));
    if (job.promotion)
      summary.append(
        node(
          "p",
          `${job.promotion.original_path} → ${job.promotion.candidate_path} · ${bytes(job.promotion.original_bytes)} → ${bytes(job.promotion.candidate_bytes)}`,
          "metadata",
        ),
      );
    const savings = savingsLine(job.savings, false, job.replaced);
    if (savings) summary.append(savings);
    const controls = $("detail-controls");
    controls.replaceChildren(jobControls(job));
    $("batch-items-panel").hidden = !job.batch;
    if (job.batch) await loadBatchItems(id, revision, job.batch.total);
    restoreControl(controls, focus);
  } catch (error) {
    if (
      state.detail === id &&
      revision === state.detailRevision &&
      $("job-detail").open
    ) {
      $("detail-summary").replaceChildren(
        node("p", error.message || "Could not load this job.", "muted"),
        button("Retry", refreshDetail, "quiet"),
      );
      $("detail-controls").replaceChildren();
      $("batch-items-panel").hidden = true;
      $("load-detail").hidden = true;
    }
  }
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
        button("View file", () => openJob(item.child_action_id), "quiet"),
      );
    if (item.error) row.append(node("span", item.error, "muted"));
    $("batch-items-list").append(row);
  }
  $("batch-items-note").textContent =
    `${response.items?.length || 0} of ${response.total} files`;
  state.batchItemsHasMore = response.has_more;
  $("batch-items-prev").disabled = offset === 0;
  $("batch-items-next").disabled = !response.has_more;
  $("batch-items-prev").hidden = offset === 0 && !response.has_more;
  $("batch-items-next").hidden = offset === 0 && !response.has_more;
}
async function changeBatchPage(delta) {
  if (state.batchItemsPaging || !state.detail) return;
  if (delta > 0 && !state.batchItemsHasMore) return;
  if (delta < 0 && state.batchItemsOffset === 0) return;
  const id = state.detail,
    revision = state.detailRevision;
  const previous = state.batchItemsOffset;
  const paging = { id, revision };
  state.batchItemsPaging = paging;
  state.batchItemsOffset = Math.max(0, previous + delta);
  $("batch-items-prev").disabled = true;
  $("batch-items-next").disabled = true;
  try {
    await loadBatchItems(id, revision);
  } catch (error) {
    if (state.detail === id && revision === state.detailRevision)
      state.batchItemsOffset = previous;
    throw error;
  } finally {
    if (state.batchItemsPaging === paging) {
      state.batchItemsPaging = null;
      if (state.detail === id && revision === state.detailRevision) {
        $("batch-items-prev").disabled = state.batchItemsOffset === 0;
        $("batch-items-next").disabled = !state.batchItemsHasMore;
      }
    }
  }
}
$("batch-items-prev").addEventListener("click", () =>
  safe(() => changeBatchPage(-25)),
);
$("batch-items-next").addEventListener("click", () =>
  safe(() => changeBatchPage(25)),
);
async function loadDetail() {
  const id = state.detail;
  const data = await api(`logs?id=${encodeURIComponent(id)}`);
  if (state.detail !== id || !$("job-detail").open) return;
  showData(
    "detail-content",
    [data.stdout_tail, data.stderr_tail].filter(Boolean).join("\n") ||
      "No worker output yet.",
  );
}

async function reviewBatchPromotion(id) {
  const authRevision = state.authRevision;
  const args = { id, section: "state", key: "batch_promotion_plan", chunk: 0 };
  const first = await tool("action_detail", args);
  let plan = first.data;
  if (first.total_chunks) {
    if (first.total_chunks > 128)
      throw new Error("This replacement plan is too large to display safely.");
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
    throw new Error("Cannot verify the batch replacement list.");
  state.batchApproval = id;
  $("batch-review-data").textContent =
    `${plan.members.length} replacement${plan.members.length === 1 ? "" : "s"}\n\n` +
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
  option($("profile"), "auto", "Automatic");
  state.recipes.forEach((n) => option($("profile"), n));
  $("profile").value = state.recipes.includes(selected) ? selected : "auto";
  renderRecipeList();
  if (initial) return;
}
$("recipes-more").addEventListener("click", () =>
  safe(() => loadRecipes(false, true)),
);
async function readRecipe(name) {
  const revision = (state.recipeRevision || 0) + 1;
  const auth = state.authRevision;
  state.recipeRevision = revision;
  $("recipe-form").inert = true;
  $("recipe-source").textContent = `Loading ${name}…`;
  try {
    const r = await tool("recipe_get", { name });
    if (revision !== state.recipeRevision || auth !== state.authRevision)
      return;
    state.recipe = r;
    $("recipe-name").value = name;
    $("recipe-description").value = r.record?.description || "";
    setRecipeControls(r.profile);
    $("recipe-source").textContent =
      `${r.source === "managed" ? "Custom profile" : "Built-in profile"}${r.shadowed_profiles?.length ? " · Overrides built-in" : ""}`;
    $("delete-recipe").disabled = r.source !== "managed";
    $("recipe-feedback").hidden = true;
    renderRecipeList();
  } catch (error) {
    if (revision === state.recipeRevision && auth === state.authRevision)
      $("recipe-source").textContent =
        `Could not load ${name}. Select it again to retry.`;
    // A failed read must not reactivate the previous profile's editable form.
    // Successful reads and new profile creation enable it in setRecipeControls.
    throw error;
  }
}
$("new-recipe").addEventListener("click", () =>
  safe(() => {
    state.recipe = null;
    $("recipe-name").value = "";
    $("recipe-description").value = "";
    $("recipe-source").textContent = "New profile";
    setRecipeControls(profileFromControls());
    $("recipe-feedback").hidden = true;
    $("delete-recipe").disabled = true;
  }),
);
async function saveRecipe() {
  if (state.recipeSaving) return;
  state.recipeSaving = true;
  const revision = state.recipeRevision;
  const auth = state.authRevision;
  try {
    const args = {
      name: $("recipe-name").value.trim(),
      description: $("recipe-description").value,
      profile: recipeFromControls(),
    };
    if (state.recipe?.record && state.recipe.name === args.name) {
      args.expected_generation = state.recipe.record.generation;
      args.expected_digest = state.recipe.record.digest;
    }
    await tool("recipe_save", args);
    if (auth !== state.authRevision) return;
    await loadRecipes();
    if (revision === state.recipeRevision && auth === state.authRevision)
      await readRecipe(args.name);
    notify("Profile saved.");
  } finally {
    state.recipeSaving = false;
  }
}
$("recipe-form").addEventListener("submit", (e) => {
  e.preventDefault();
  safe(saveRecipe);
});
$("delete-recipe").addEventListener("click", () =>
  safe(async () => {
    const r = state.recipe;
    if (
      !r?.record ||
      !confirm(`Reset profile ${r.name} to its built-in version?`)
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
    if (state.recipes.length) await readRecipe(state.recipes[0]);
    notify("Profile reset.");
  }),
);
$("recipe-history").addEventListener("click", () =>
  safe(async () => {
    const result = await tool("recipe_history", {
      name: $("recipe-name").value,
    });
    const entries = result.history || [];
    showData(
      "recipe-feedback",
      entries.length
        ? entries
            .map(
              (e) =>
                `${e.operation || e.event || "Saved"} · ${new Date(e.created_at || e.at || e.timestamp).toLocaleString("en")}`,
            )
            .join("\n")
        : "No profile changes recorded.",
    );
  }),
);
function renderRecipeList() {
  $("recipes-list").replaceChildren();
  const query = $("recipe-search").value.toLowerCase();
  for (const name of state.recipes.filter((n) =>
    n.toLowerCase().includes(query),
  )) {
    const b = button(name, () => readRecipe(name), "recipe-button");
    b.setAttribute("aria-pressed", String(state.recipe?.name === name));
    $("recipes-list").append(b);
  }
}
$("recipe-search").addEventListener("input", renderRecipeList);
function setRecipeControls(profile) {
  // New profile creation also supersedes any in-flight profile request.
  state.recipeRevision = (state.recipeRevision || 0) + 1;
  $("recipe-form").inert = false;
  state.recipeDraft = JSON.parse(JSON.stringify(profile));
  const video = profile.video || {};
  for (const [id, value] of Object.entries({
    "recipe-container": profile.container || "mkv",
    "recipe-encoder": video.codec || "libx265",
    "recipe-quality":
      video.average_bitrate_kbps > 0
        ? 0
        : (video.quality ?? (video.codec === "hevc_videotoolbox" ? 65 : 23)),
    "recipe-preset": video.preset || "medium",
    "recipe-audio": profile.audio?.mode || "copy",
  }))
    $(id).value = String(value);
  for (const key of ["metadata", "chapters", "attachments"])
    $("recipe-" + key).checked = profile.preserve?.[key] !== false;
  renderAdvancedProfile(profile);
  // Advanced rate-control inputs update the draft before bubbling here.
  $("recipe-extra-fields").oninput = recipeControls;
  recipeControls();
}
function recipeControls() {
  const software = $("recipe-encoder").value === "libx265";
  const bitrate =
    !software && state.recipeDraft?.video?.average_bitrate_kbps > 0;
  $("recipe-preset").disabled = !software;
  const presetLabel = $("recipe-preset").closest?.("label");
  if (presetLabel) presetLabel.hidden = !software;
  $("recipe-quality").disabled = bitrate;
  $("recipe-quality").max = software ? "51" : "100";
  $("recipe-quality").min = bitrate ? "0" : "1";
  if (bitrate) $("recipe-quality").value = "0";
  else if (!software && Number($("recipe-quality").value) === 0)
    $("recipe-quality").value = "65";
}
$("recipe-encoder").addEventListener("change", () => {
  const p = recipeFromControls();
  const previous = state.recipeDraft?.video?.codec;
  const software = p.video.codec === "libx265";
  if (p.video.codec !== previous) {
    if (software) {
      for (const key of [
        "prioritize_speed",
        "spatial_aq",
        "realtime",
        "average_bitrate_kbps",
        "max_bitrate_kbps",
        "constant_bitrate",
        "qmin",
        "qmax",
        "gop_size",
        "b_frames",
        "closed_gop",
        "power_efficient",
        "max_ref_frames",
      ])
        delete p.video[key];
      p.video.preset = $("recipe-preset").value || "medium";
      p.video.quality = 23;
    } else {
      delete p.video.preset;
      delete p.video.tune;
      p.video.quality = 65;
    }
    if (p.optimization?.search) {
      p.optimization.search.quality_values = software
        ? [20, 23, 26]
        : [55, 65, 75];
      p.optimization.search.max_candidates = 3;
      delete p.optimization.search.bitrate_values;
    }
    setRecipeControls(p);
    notify(
      "Encoder changed. Incompatible settings and sample search values were reset; review before saving.",
    );
  } else recipeControls();
});
function recipeFromControls() {
  if (state.recipeAdvancedErrors?.size)
    throw new Error([...state.recipeAdvancedErrors.values()][0]);
  const p = JSON.parse(JSON.stringify(state.recipeDraft || {}));
  p.container = $("recipe-container").value;
  p.video ||= {};
  p.video.codec = $("recipe-encoder").value;
  p.video.quality =
    p.video.codec === "hevc_videotoolbox" && p.video.average_bitrate_kbps > 0
      ? 0
      : Number($("recipe-quality").value);
  if (p.video.codec === "libx265") p.video.preset = $("recipe-preset").value;
  p.audio ||= {};
  p.audio.mode = $("recipe-audio").value;
  p.preserve ||= {};
  for (const key of ["metadata", "chapters", "attachments"])
    p.preserve[key] = $("recipe-" + key).checked;
  return p;
}
// Defaults describe available controls; merely opening the dialog does not
// insert optional values into an existing profile.
function advancedProfileDefaults(profile) {
  const vt = profile.video?.codec === "hevc_videotoolbox";
  const bitrate = vt && profile.video?.average_bitrate_kbps > 0;
  const video = { profile: "", pixel_format: "" };
  if (vt) {
    Object.assign(video, {
      average_bitrate_kbps: null,
      max_bitrate_kbps: null,
      constant_bitrate: null,
      prioritize_speed: null,
      spatial_aq: null,
      realtime: null,
      qmin: null,
      qmax: null,
      gop_size: null,
      b_frames: null,
      closed_gop: null,
      power_efficient: null,
      max_ref_frames: null,
    });
  } else video.tune = "";
  return {
    video,
    preserve: { metadata: true, chapters: true, attachments: true },
    optimization: {
      enabled: false,
      sampling: {
        strategy: "distributed",
        sample_count: 3,
        sample_seconds: 20,
      },
      quality: {
        preferred_metric: "vmaf",
        vmaf: { target: 96, minimum: 95, marginal_tolerance: 0.5 },
        ssim: { target: 0.99, minimum: 0.98, marginal_tolerance: 0.005 },
      },
      search: {
        max_candidates: 3,
        [bitrate ? "bitrate_values" : "quality_values"]: bitrate
          ? [
              Math.round(profile.video.average_bitrate_kbps * 0.8),
              profile.video.average_bitrate_kbps,
              Math.round(profile.video.average_bitrate_kbps * 1.2),
            ]
          : vt
            ? [55, 65, 75]
            : [20, 23, 26],
      },
    },
  };
}
function mergeAdvancedDefaults(defaults, profile) {
  const result = JSON.parse(JSON.stringify(profile || {}));
  for (const [key, value] of Object.entries(defaults)) {
    if (value && typeof value === "object" && !Array.isArray(value))
      result[key] = mergeAdvancedDefaults(value, result[key]);
    else if (!(key in result)) result[key] = value;
  }
  return result;
}
function updatePreservationNote() {
  const note = document.querySelector?.(".preservation-note");
  if (!note) return;
  const kept = ["metadata", "chapters", "attachments"].filter(
    (key) => $("recipe-" + key).checked,
  );
  note.textContent =
    kept.length === 3
      ? "Metadata, chapters and attachments are preserved."
      : `Unsupported policy: ${["metadata", "chapters", "attachments"].filter((key) => !kept.includes(key)).join(", ")} must be enabled before saving.`;
}
function renderAdvancedProfile(profile) {
  const host = $("recipe-extra-fields");
  host.replaceChildren();
  state.recipeAdvancedErrors = new Map();
  const defaults = advancedProfileDefaults(profile);
  const view = mergeAdvancedDefaults(defaults, profile);
  const represented = new Set([
    "container",
    "video.codec",
    "video.quality",
    "video.preset",
    "audio.mode",
  ]);
  const booleans = new Set([
    "constant_bitrate",
    "prioritize_speed",
    "spatial_aq",
    "realtime",
    "closed_gop",
    "power_efficient",
  ]);
  const bounds = {
    average_bitrate_kbps: [1, 1000000],
    max_bitrate_kbps: [1, 1000000],
    qmin: [0, 69],
    qmax: [0, 69],
    gop_size: [1, 100000],
    b_frames: [0, 1],
    max_ref_frames: [1, 16],
    sample_count: [1, 20],
    sample_seconds: [0.1, 120],
    max_candidates: [1, 20],
  };
  const choices = {
    "video.profile": ["", "main", "main10"],
    "video.pixel_format": ["", "yuv420p", "p010le"],
    "video.tune": [
      "",
      "animation",
      "grain",
      "fastdecode",
      "zerolatency",
      "psnr",
      "ssim",
    ],
    "optimization.sampling.strategy": [
      "distributed",
      "uniform",
      "relative_positions",
    ],
    "optimization.quality.preferred_metric": ["vmaf", "ssim"],
  };
  const setValue = (path, value) => {
    const keys = path.split(".");
    let target = state.recipeDraft;
    for (const part of keys.slice(0, -1)) target = target[part] ||= {};
    if (value === undefined) delete target[keys.at(-1)];
    else target[keys.at(-1)] = value;
    if (path.startsWith("preserve.")) {
      $("recipe-" + keys.at(-1)).checked = value !== false;
      updatePreservationNote();
    }
  };
  const visit = (obj, path, parent) => {
    for (const [key, value] of Object.entries(obj)) {
      const current = path ? `${path}.${key}` : key;
      if (represented.has(current)) continue;
      const caption = key
        .replaceAll("_", " ")
        .replace(/^./, (c) => c.toUpperCase());
      if (value && typeof value === "object" && !Array.isArray(value)) {
        const group = node("fieldset");
        group.append(node("legend", caption));
        if (
          current === "video" &&
          profile.video?.codec === "hevc_videotoolbox"
        ) {
          const label = node("label", "Rate control");
          const select = document.createElement("select");
          select.dataset.profileField = "video.rate_control";
          option(select, "quality", "Quality");
          option(select, "bitrate", "Bitrate");
          select.value =
            profile.video.average_bitrate_kbps > 0 ? "bitrate" : "quality";
          select.addEventListener("change", () => {
            const p = state.recipeDraft;
            if (select.value === "bitrate") {
              p.video.average_bitrate_kbps ||= 3500;
              p.video.quality = 0;
              $("recipe-quality").value = "0";
            } else {
              delete p.video.average_bitrate_kbps;
              delete p.video.max_bitrate_kbps;
              delete p.video.constant_bitrate;
              p.video.quality = 65;
              $("recipe-quality").value = "65";
            }
            if (p.optimization?.search) {
              delete p.optimization.search.quality_values;
              delete p.optimization.search.bitrate_values;
              Object.assign(
                p.optimization.search,
                advancedProfileDefaults(p).optimization.search,
              );
            }
            renderAdvancedProfile(p);
            recipeControls();
          });
          label.append(select);
          group.append(label);
        }
        visit(value, current, group);
        if (group.children.length > 1) parent.append(group);
      } else if (
        Array.isArray(value) &&
        value.some((v) => typeof v === "object")
      ) {
        const group = node("fieldset");
        group.append(node("legend", caption));
        visit(value, current, group);
        if (group.children.length > 1) parent.append(group);
      } else {
        const label = node("label", caption);
        const optionalBoolean = booleans.has(key);
        const enumValues = choices[current];
        const input = document.createElement(
          optionalBoolean || enumValues ? "select" : "input",
        );
        input.dataset.profileField = current;
        const list = Array.isArray(value);
        if (optionalBoolean) {
          option(input, "", "Automatic");
          option(input, "true", "On");
          option(input, "false", "Off");
          input.value = value == null ? "" : String(value);
        } else if (enumValues) {
          for (const v of enumValues) option(input, v, v || "Automatic");
          if (value && !enumValues.includes(value)) option(input, value);
          input.value = value ?? "";
        } else {
          input.type =
            typeof value === "boolean"
              ? "checkbox"
              : typeof value === "number" || value === null
                ? "number"
                : "text";
          if (input.type === "checkbox") {
            input.checked = value;
            label.className = "check";
            if (current.startsWith("preserve.")) {
              label.textContent = `${caption} (required)`;
              input.disabled = value === true;
            }
          } else {
            input.value = list ? value.join(", ") : (value ?? "");
            if (value === null) input.placeholder = "Automatic";
            if (input.type === "number") {
              input.step =
                key === "sample_seconds" || current.includes(".quality.")
                  ? "any"
                  : "1";
              if (bounds[key]) [input.min, input.max] = bounds[key].map(String);
            }
          }
        }
        const bitrateOnly =
          current.startsWith("video.") &&
          [
            "average_bitrate_kbps",
            "max_bitrate_kbps",
            "constant_bitrate",
          ].includes(key);
        if (bitrateOnly)
          input.disabled = !(profile.video?.average_bitrate_kbps > 0);
        input.addEventListener(
          optionalBoolean || enumValues ? "change" : "input",
          () => {
            let next;
            if (optionalBoolean)
              next = input.value === "" ? undefined : input.value === "true";
            else if (input.type === "checkbox") {
              if (current.startsWith("preserve.")) {
                input.checked = true;
                input.disabled = true;
              }
              next = input.checked;
            } else if (list) {
              next = input.value.trim()
                ? input.value.split(",").map((v) => Number(v.trim()))
                : [];
              if (
                next.some((v) => !Number.isFinite(v)) ||
                input.value.split(",").some((v) => !v.trim())
              ) {
                state.recipeAdvancedErrors.set(
                  current,
                  `${caption} must be a comma-separated list of numbers.`,
                );
                input.setCustomValidity?.("Enter comma-separated numbers.");
                return;
              }
            } else if (input.type === "number") {
              if (!input.value.trim() && key === "average_bitrate_kbps") {
                state.recipeAdvancedErrors.set(
                  current,
                  "Average bitrate is required in bitrate mode.",
                );
                input.setCustomValidity?.("Enter an average bitrate.");
                return;
              }
              if (!input.value.trim()) next = undefined;
              else {
                next = Number(input.value);
                if (
                  !Number.isFinite(next) ||
                  (bounds[key] &&
                    (next < bounds[key][0] || next > bounds[key][1])) ||
                  (input.step === "1" && !Number.isInteger(next))
                ) {
                  state.recipeAdvancedErrors.set(
                    current,
                    `${caption} is outside its valid range.`,
                  );
                  input.setCustomValidity?.(
                    "Enter a value within the allowed range.",
                  );
                  return;
                }
              }
            } else next = input.value || undefined;
            state.recipeAdvancedErrors.delete(current);
            input.setCustomValidity?.("");
            setValue(current, next);
            if (current === "optimization.enabled" && next) {
              state.recipeDraft.optimization = mergeAdvancedDefaults(
                advancedProfileDefaults(state.recipeDraft).optimization,
                state.recipeDraft.optimization,
              );
            }
            recipeControls();
          },
        );
        label.append(input);
        parent.append(label);
      }
    }
  };
  visit(view, "", host);
  updatePreservationNote();
}
$("reload-profiles").addEventListener("click", () =>
  safe(async () => {
    const r = await tool("recipe_reload");
    if (r.error) throw new Error(r.error);
    await loadRecipes();
    notify("Shared presets refreshed.");
  }),
);
$("rollback-profiles").addEventListener("click", () =>
  safe(async () => {
    if (!confirm("Restore the previous profile bundle?")) return;
    const r = await tool("recipe_rollback");
    if (r.error) throw new Error(r.error);
    await loadRecipes();
    notify("Previous profile bundle restored.");
  }),
);

$("open-path").addEventListener("click", () => $("path-dialog").showModal());
$("close-path").addEventListener("click", () => $("path-dialog").close());
$("recipe-advanced").addEventListener("click", () =>
  $("recipe-settings").showModal(),
);
$("close-recipe-settings").addEventListener("click", () =>
  $("recipe-settings").close(),
);
async function loadBackups(more = false) {
  const result = await tool("transcode_backups", {
    offset: more ? state.backupOffset || 0 : 0,
  });
  $("backup-list").hidden = false;
  if (!more) $("backup-list").replaceChildren();
  for (const copy of result.items || []) {
    const row = node("div", "", "settings-row");
    row.append(
      node(
        "span",
        `${copy.path?.split("/").pop() || copy.action_id} · ${bytes(copy.bytes || 0)}`,
      ),
    );
    if (copy.cleanup_available)
      row.append(
        button("Verify & clean up", async () => {
          if (
            !confirm("Verify the replacement and remove its recovery copies?")
          )
            return;
          await tool("transcode_backups", {
            mode: "clean",
            action_id: copy.action_id,
          });
          await loadBackups();
          await loadJobs();
        }),
      );
    $("backup-list").append(row);
  }
  if (!(result.items || []).length && result.next_offset == null && !more)
    $("backup-list").append(node("p", "No retained recovery copies.", "muted"));
  state.backupOffset = result.next_offset;
  $("backups-more").hidden = result.next_offset == null;
}
$("load-backups").addEventListener("click", () => safe(() => loadBackups()));
$("backups-more").addEventListener("click", () =>
  safe(() => loadBackups(true)),
);

let polling = false;
setInterval(async () => {
  if (polling || document.hidden) return;
  polling = true;
  try {
    if (!serverReachable) {
      await safe(reconnect);
      return;
    }
    if ($("workspace").hidden) return;
    if (!state.jobsLoading) await safe(() => loadJobs());
    if ($("job-detail").open) await safe(refreshDetail);
  } finally {
    polling = false;
  }
}, 5000);
if (typeof window !== "undefined") setConnection(serverReachable);
safe(initialize);
