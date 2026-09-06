const DEFAULT_SETTINGS = {
  helperName: "robot.downloader",
  autoIntercept: false,
  maxConnections: 8,
  chunkSizeMb: 8,
  retryCount: 3,
  youtubeQuality: "highest",
};

function normalizeSettings(input = {}) {
  const rawQuality = String(input.youtubeQuality || DEFAULT_SETTINGS.youtubeQuality).trim().toLowerCase();
  const youtubeQuality = ["highest", "2160p", "1440p", "1080p", "720p", "480p", "360p"].includes(rawQuality)
    ? rawQuality
    : DEFAULT_SETTINGS.youtubeQuality;

  return {
    helperName: String(input.helperName || DEFAULT_SETTINGS.helperName).trim() || DEFAULT_SETTINGS.helperName,
    autoIntercept: Boolean(input.autoIntercept),
    maxConnections: Math.min(32, Math.max(1, Number.parseInt(input.maxConnections, 10) || DEFAULT_SETTINGS.maxConnections)),
    chunkSizeMb: Math.min(256, Math.max(1, Number.parseInt(input.chunkSizeMb, 10) || DEFAULT_SETTINGS.chunkSizeMb)),
    retryCount: Math.min(10, Math.max(0, Number.parseInt(input.retryCount, 10) || DEFAULT_SETTINGS.retryCount)),
    youtubeQuality,
  };
}

async function getSettings() {
  const stored = await browser.storage.local.get("settings");
  return normalizeSettings({ ...DEFAULT_SETTINGS, ...(stored.settings || {}) });
}

async function saveSettings(nextSettings) {
  const settings = normalizeSettings(nextSettings);
  await browser.storage.local.set({ settings });
  return settings;
}

function setMessage(id, message, isError = false) {
  const el = document.getElementById(id);
  el.textContent = message;
  el.style.color = isError ? "#fecaca" : "";
}

function fillSettingsForm(settings) {
  document.getElementById("helperNameInput").value = settings.helperName || DEFAULT_SETTINGS.helperName;
  document.getElementById("maxConnectionsInput").value = settings.maxConnections ?? DEFAULT_SETTINGS.maxConnections;
  document.getElementById("chunkSizeInput").value = settings.chunkSizeMb ?? DEFAULT_SETTINGS.chunkSizeMb;
  document.getElementById("retryCountInput").value = settings.retryCount ?? DEFAULT_SETTINGS.retryCount;
  document.getElementById("youtubeQualityInput").value = settings.youtubeQuality || DEFAULT_SETTINGS.youtubeQuality;
  document.getElementById("autoInterceptInput").checked = Boolean(settings.autoIntercept);
  document.getElementById("quickQualityInput").value = settings.youtubeQuality || DEFAULT_SETTINGS.youtubeQuality;
}

async function collectCookiesForURL(url) {
  try {
    const target = new URL(url);
    const candidates = [url, `${target.origin}/`];
    const seen = new Map();
    for (const candidate of candidates) {
      const cookies = await browser.cookies.getAll({ url: candidate });
      for (const cookie of cookies) {
        seen.set(`${cookie.storeId}:${cookie.domain}:${cookie.path}:${cookie.name}`, cookie);
      }
    }
    const cookies = [...seen.values()];
    return cookies.length ? cookies.map((cookie) => `${cookie.name}=${cookie.value}`).join("; ") : "";
  } catch (_) {
    return "";
  }
}

function getUserAgent() {
  try { return navigator.userAgent; } catch (_) { return "Mozilla/5.0"; }
}

function getAcceptLanguage() {
  try {
    const langs = navigator.languages?.filter(Boolean) || [];
    if (langs.length) return langs.join(",");
    return navigator.language || "en-US,en;q=0.9";
  } catch (_) {
    return "en-US,en;q=0.9";
  }
}

async function getActiveTab() {
  const tabs = await browser.tabs.query({ active: true, currentWindow: true });
  return tabs[0] || null;
}

async function buildRequestContext(url, pageURL = "") {
  const cookieHeader = await collectCookiesForURL(url);
  const headers = {
    "User-Agent": getUserAgent(),
    Accept: "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8",
    "Accept-Language": getAcceptLanguage(),
    "Cache-Control": "no-cache",
    Pragma: "no-cache",
    DNT: "1",
    "Upgrade-Insecure-Requests": "1",
  };
  if (cookieHeader) headers.Cookie = cookieHeader;
  if (pageURL) {
    headers.Referer = pageURL;
    try {
      const parsed = new URL(pageURL);
      headers.Origin = parsed.origin;
    } catch (_) {
      // ignore
    }
  }
  return { headers, pageURL };
}

async function sendThroughHelper(message) {
  return browser.runtime.sendMessage(message);
}

async function refreshBridgeStatus() {
  try {
    const response = await sendThroughHelper({ type: "app-health" });
    document.getElementById("bridgeStatus").textContent = response?.ok ? "Connected" : "Unavailable";
    setMessage("statusMessage", response?.ok ? "Downloader app is reachable." : "Downloader app is unavailable.");
  } catch (error) {
    document.getElementById("bridgeStatus").textContent = "Unavailable";
    setMessage("statusMessage", String(error?.message || error), true);
  }
}

async function openDownloaderApp() {
  const response = await sendThroughHelper({ type: "open-app" });
  if (response?.ok === false) throw new Error(response.message || "Could not open downloader app");
  setMessage("statusMessage", response?.message || "Opened downloader app.");
}

async function quickSendURL(url, filename, youtubeQuality) {
  const settings = await getSettings();
  const activeTab = await getActiveTab();
  const context = await buildRequestContext(url, activeTab?.url || "");
  const response = await sendThroughHelper({
    type: "enqueue",
    url,
    filename: filename || null,
    options: {
      maxConnections: settings.maxConnections,
      chunkSizeBytes: settings.chunkSizeMb * 1024 * 1024,
      retryCount: settings.retryCount,
      youtubeQuality: youtubeQuality || settings.youtubeQuality,
    },
    context,
  });
  if (response?.ok === false) throw new Error(response.message || "Queue failed");
  setMessage("statusMessage", response?.message || "Sent to downloader.");
}

function isSupportedPageVideoURL(url = "") {
  try {
    const parsed = new URL(url);
    const host = parsed.hostname.toLowerCase();
    if (host === "youtu.be") return true;
    return (host === "youtube.com" || host.endsWith(".youtube.com")) && parsed.pathname === "/watch";
  } catch (_) {
    return false;
  }
}

async function sendCurrentTab() {
  const tab = await getActiveTab();
  if (!tab?.url) throw new Error("No active tab URL found");
  const url = tab.url;
  if (!/^https?:/i.test(url)) throw new Error("Current tab is not a downloadable HTTP(S) page");
  const filename = isSupportedPageVideoURL(url) ? null : (tab.title || "download").replace(/[\\/:*?"<>|]+/g, "_");
  const quality = document.getElementById("quickQualityInput").value || DEFAULT_SETTINGS.youtubeQuality;
  await quickSendURL(url, filename, quality);
}

document.getElementById("openAppButton").addEventListener("click", () => {
  openDownloaderApp().catch((error) => setMessage("statusMessage", String(error?.message || error), true));
});

document.getElementById("refreshStatusButton").addEventListener("click", () => {
  refreshBridgeStatus();
});

document.getElementById("quickSendForm").addEventListener("submit", (event) => {
  event.preventDefault();
  const url = document.getElementById("quickUrlInput").value.trim();
  const filename = document.getElementById("quickFilenameInput").value.trim();
  const quality = document.getElementById("quickQualityInput").value;
  if (!url) {
    setMessage("statusMessage", "Quick-send URL is required.", true);
    return;
  }
  quickSendURL(url, filename, quality).catch((error) => setMessage("statusMessage", String(error?.message || error), true));
});

document.getElementById("sendCurrentTabButton").addEventListener("click", () => {
  sendCurrentTab().catch((error) => setMessage("statusMessage", String(error?.message || error), true));
});

document.getElementById("settingsForm").addEventListener("submit", async (event) => {
  event.preventDefault();
  const settings = {
    helperName: document.getElementById("helperNameInput").value.trim(),
    maxConnections: document.getElementById("maxConnectionsInput").value,
    chunkSizeMb: document.getElementById("chunkSizeInput").value,
    retryCount: document.getElementById("retryCountInput").value,
    youtubeQuality: document.getElementById("youtubeQualityInput").value,
    autoIntercept: document.getElementById("autoInterceptInput").checked,
  };
  try {
    const saved = await saveSettings(settings);
    fillSettingsForm(saved);
    setMessage("settingsMessage", "Bridge settings saved.");
  } catch (error) {
    setMessage("settingsMessage", String(error?.message || error), true);
  }
});

(async function boot() {
  try {
    const settings = await getSettings();
    fillSettingsForm(settings);
    setMessage("settingsMessage", "Settings loaded.");
  } catch (error) {
    setMessage("settingsMessage", String(error?.message || error), true);
  }
  await refreshBridgeStatus();
})();
