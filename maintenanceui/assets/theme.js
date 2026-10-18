// Color theme: "system" follows prefers-color-scheme; "light" and "dark" pin
// html[data-theme]. Loaded as a classic script in <head> so a saved choice
// applies before first paint. Storage may be unavailable (private mode,
// blocked site data); the page then simply follows the system.
(() => {
  const KEY = "navigatorr-theme";
  const CHOICES = ["system", "light", "dark"];
  const BAR = { light: "#244d40", dark: "#121715" };
  const root = document.documentElement;

  function saved() {
    try {
      const value = window.localStorage.getItem(KEY);
      return CHOICES.includes(value) ? value : "system";
    } catch {
      return "system";
    }
  }

  function apply(choice) {
    if (choice === "system") delete root.dataset.theme;
    else root.dataset.theme = choice;
    // Browser chrome: per-scheme metas for "system", the pinned color otherwise.
    for (const meta of document.querySelectorAll('meta[name="theme-color"]')) {
      const scheme = (meta.getAttribute("media") || "").includes("dark")
        ? "dark"
        : "light";
      meta.setAttribute("content", BAR[choice === "system" ? scheme : choice]);
    }
    for (const button of document.querySelectorAll("[data-theme-choice]"))
      button.setAttribute(
        "aria-pressed",
        String(button.dataset.themeChoice === choice),
      );
  }

  function choose(choice) {
    if (!CHOICES.includes(choice)) return;
    try {
      if (choice === "system") window.localStorage.removeItem(KEY);
      else window.localStorage.setItem(KEY, choice);
    } catch {
      // Not persisted; the choice still applies to this page.
    }
    apply(choice);
  }

  apply(saved());
  document.addEventListener("DOMContentLoaded", () => apply(saved()));
  document.addEventListener("click", (event) => {
    const button = event.target.closest?.("[data-theme-choice]");
    if (button) choose(button.dataset.themeChoice);
  });
  // Another tab changed the choice.
  window.addEventListener("storage", (event) => {
    if (event.key === KEY || event.key === null) apply(saved());
  });
})();
