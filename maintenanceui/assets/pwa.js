const installButton = document.getElementById("install-app");
const help = document.getElementById("install-help");
let prompt;
let installed =
  window.matchMedia("(display-mode: standalone)").matches ||
  navigator.standalone === true;
const ios =
  /iPhone|iPad|iPod/.test(navigator.userAgent) ||
  (navigator.platform === "MacIntel" && navigator.maxTouchPoints > 1);
const mobile = ios || /Android/.test(navigator.userAgent);
function updateInstall() {
  installButton.hidden =
    installed || !window.isSecureContext || (!prompt && !mobile);
  installButton.textContent = prompt ? "Install app" : "Add app";
}
updateInstall();
document.getElementById("install-instructions").textContent = ios
  ? "Share → Add to Home Screen."
  : "Browser menu → Install app or Add to Home Screen.";
installButton.addEventListener("click", async () => {
  if (prompt) {
    const event = prompt;
    prompt = undefined;
    await event.prompt();
    await event.userChoice;
    updateInstall();
  } else if (mobile && !installed) help.showModal();
});
document
  .getElementById("close-install")
  .addEventListener("click", () => help.close());
window.addEventListener("beforeinstallprompt", (event) => {
  event.preventDefault();
  prompt = event;
  updateInstall();
});
window.addEventListener("appinstalled", () => {
  installed = true;
  prompt = undefined;
  help.close();
  updateInstall();
});
if ("serviceWorker" in navigator && window.isSecureContext) {
  const showOfflineAvailability = () => {
    const ready = !!navigator.serviceWorker.controller;
    document.documentElement.dataset.pwaReady = String(ready);
    if (ready)
      document.getElementById("install-status").textContent =
        "The app opens offline. Jobs and changes need a connection.";
  };
  navigator.serviceWorker.addEventListener(
    "controllerchange",
    showOfflineAvailability,
  );
  navigator.serviceWorker
    .register("/sw.js", { scope: "/", updateViaCache: "none" })
    .then(() => navigator.serviceWorker.ready)
    .then(showOfflineAvailability)
    .catch(() => {
      document.getElementById("install-status").textContent =
        "Offline start is unavailable. You can keep using the web app.";
    });
} else
  document.getElementById("install-status").textContent =
    "Installation requires HTTPS.";
