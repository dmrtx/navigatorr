const installButton = document.getElementById("install-app");
const help = document.getElementById("install-help");
let prompt;
const standalone =
  window.matchMedia("(display-mode: standalone)").matches ||
  navigator.standalone === true;
if (!standalone) {
  installButton.hidden = false;
  installButton.addEventListener("click", async () => {
    if (prompt) {
      await prompt.prompt();
      await prompt.userChoice;
      prompt = undefined;
    } else {
      help.hidden = !help.hidden;
    }
  });
}
window.addEventListener("beforeinstallprompt", (event) => {
  event.preventDefault();
  prompt = event;
});
window.addEventListener("appinstalled", () => {
  installButton.hidden = true;
  help.hidden = true;
  prompt = undefined;
});
if ("serviceWorker" in navigator && window.isSecureContext) {
  const showOfflineAvailability = () => {
    const ready = !!navigator.serviceWorker.controller;
    document.documentElement.dataset.pwaReady = String(ready);
    if (ready)
      document.getElementById("install-status").textContent =
        "Inicio disponible sin conexión. Para consultar trabajos y enviar cambios necesitas conexión con Navigatorr.";
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
        "No se pudo preparar el inicio sin conexión. Puedes seguir usando la web.";
    });
} else {
  document.getElementById("install-status").textContent =
    "La instalación PWA necesita HTTPS (o localhost en pruebas).";
}
