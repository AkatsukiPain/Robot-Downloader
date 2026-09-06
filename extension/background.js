const DEFAULT_SETTINGS = {
  helperName: "robot.downloader",
  autoIntercept: false,
  maxConnections: 8,
  chunkSizeMb: 8,
  retryCount: 3,
  youtubeQuality: "highest",
  theme: "light",
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
    theme: input.theme === "dark" ? "dark" : "light",
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

async function notify(title, message) {
  return browser.notifications.create({
    type: "basic",
    iconUrl: browser.runtime.getURL("assets/icon.svg"),
    title,
    message,
  });
}

let nativePortPromise = null;
let helperRequestChain = Promise.resolve();
let lastHoveredMedia = null;
const recentMediaByTab = new Map();
const recentYouTubePlaybackByTab = new Map();
const HELPER_REQUEST_TIMEOUT_MS = 15000;

function sanitizeCapturedHeaders(headerList = []) {
  const out = {};
  for (const header of headerList) {
    const name = String(header?.name || "").trim();
    const value = String(header?.value || "").trim();
    if (!name || !value) continue;
    if (/^cookie$/i.test(name)) continue;
    if (value.length > 4096) continue;
    out[name] = value;
  }
  return out;
}

async function connectToHelper() {
  const settings = await getSettings();

  if (nativePortPromise) {
    return nativePortPromise;
  }

  nativePortPromise = Promise.resolve().then(() => {
    const port = browser.runtime.connectNative(settings.helperName);
    port.onDisconnect.addListener(() => {
      console.warn("[Robot Downloader] native helper disconnected", browser.runtime.lastError);
      nativePortPromise = null;
    });
    return port;
  });

  return nativePortPromise;
}

async function sendToHelper(payload) {
  const runRequest = async () => {
    try {
      const port = await connectToHelper();
      return await new Promise((resolve, reject) => {
        let settled = false;
        let timeoutId = null;

        const cleanup = () => {
          if (timeoutId) {
            clearTimeout(timeoutId);
            timeoutId = null;
          }
          port.onMessage.removeListener(onMessage);
          port.onDisconnect.removeListener(onDisconnect);
        };

        const onMessage = (response) => {
          if (settled) return;
          settled = true;
          cleanup();
          console.log("[Robot Downloader] helper response", response);
          resolve(response);
        };

        const onDisconnect = () => {
          if (settled) return;
          settled = true;
          cleanup();
          nativePortPromise = null;
          reject(new Error(browser.runtime.lastError?.message || "Native helper disconnected"));
        };

        timeoutId = setTimeout(() => {
          if (settled) return;
          settled = true;
          cleanup();
          try {
            port.disconnect();
          } catch (_) {
            // ignore
          }
          nativePortPromise = null;
          reject(new Error("Native helper timed out. Reload the extension and try again."));
        }, HELPER_REQUEST_TIMEOUT_MS);

        port.onMessage.addListener(onMessage);
        port.onDisconnect.addListener(onDisconnect);
        port.postMessage(payload);
      });
    } catch (error) {
      console.error("[Robot Downloader] native helper error", error);
      await notify(
        "Robot Downloader",
        "Native helper not available yet. Install/register the Go helper to enable real downloads."
      );
      throw error;
    }
  };

  const request = helperRequestChain.then(runRequest, runRequest);
  helperRequestChain = request.catch(() => undefined);
  return request;
}

function getUserAgent() {
  try {
    return navigator.userAgent;
  } catch (_) {
    return "Mozilla/5.0";
  }
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
    if (!cookies.length) {
      return "";
    }
    return cookies.map((cookie) => `${cookie.name}=${cookie.value}`).join("; ");
  } catch (error) {
    console.warn("[Robot Downloader] failed to collect cookies", error);
    return "";
  }
}

async function getActivePageContext() {
  try {
    const tabs = await browser.tabs.query({ active: true, currentWindow: true });
    const tab = tabs[0];
    if (!tab?.url) return { pageURL: "", origin: "" };
    const parsed = new URL(tab.url);
    return { pageURL: tab.url, origin: parsed.origin };
  } catch (_) {
    return { pageURL: "", origin: "" };
  }
}

async function findRefererForURL(url) {
  const active = await getActivePageContext();
  if (active.pageURL) {
    return active.pageURL;
  }
  try {
    const target = new URL(url);
    return `${target.origin}/`;
  } catch (_) {
    return "";
  }
}

async function buildRequestContext(url) {
  const [{ pageURL, origin }, cookieHeader, referer] = await Promise.all([
    getActivePageContext(),
    collectCookiesForURL(url),
    findRefererForURL(url),
  ]);

  const headers = {
    "User-Agent": getUserAgent(),
    Accept: "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8",
    "Accept-Language": getAcceptLanguage(),
    "Cache-Control": "no-cache",
    Pragma: "no-cache",
    DNT: "1",
    Connection: "keep-alive",
    "Upgrade-Insecure-Requests": "1",
  };

  if (cookieHeader) headers.Cookie = cookieHeader;
  if (referer) headers.Referer = referer;
  if (origin) headers.Origin = origin;

  return {
    headers,
    pageURL,
  };
}

async function resolvePreferredDownloadTarget(url) {
  const candidate = String(url || "");
  const active = await getActivePageContext();
  if (shouldPreferPageVideoURL(candidate, active.pageURL || "")) {
    return active.pageURL;
  }
  return candidate;
}

async function buildEnqueuePayload(url, filename, overrides = {}) {
  const settings = await getSettings();
  const resolvedURL = await resolvePreferredDownloadTarget(url);
  const context = await buildRequestContext(resolvedURL);
  const youtubeQuality = String(overrides.youtubeQuality || settings.youtubeQuality || "highest").trim().toLowerCase();

  if (overrides.youtubePlayback?.originUrl && !context.pageURL) {
    context.pageURL = overrides.youtubePlayback.originUrl;
  }
  if (overrides.youtubePlayback?.url) {
    context.headers["X-Robot-YouTube-Playback-URL"] = overrides.youtubePlayback.url;
  }
  if (overrides.youtubePlayback?.method) {
    context.headers["X-Robot-YouTube-Playback-Method"] = overrides.youtubePlayback.method;
  }
  if (overrides.youtubePlayback?.documentUrl) {
    context.headers["X-Robot-YouTube-Playback-Document"] = overrides.youtubePlayback.documentUrl;
  }
  if (overrides.youtubePlayback?.requestHeaders) {
    context.headers["X-Robot-YouTube-Playback-Request-Headers"] = JSON.stringify(overrides.youtubePlayback.requestHeaders);
  }
  if (overrides.youtubePlayback?.responseHeaders) {
    context.headers["X-Robot-YouTube-Playback-Response-Headers"] = JSON.stringify(overrides.youtubePlayback.responseHeaders);
  }

  return {
    type: "enqueue",
    url: resolvedURL,
    filename: filename || null,
    options: {
      maxConnections: settings.maxConnections,
      chunkSizeBytes: settings.chunkSizeMb * 1024 * 1024,
      retryCount: settings.retryCount,
      youtubeQuality,
    },
    context,
  };
}

async function queueDownloadFromLink(url, filename, overrides = {}) {
  const payload = await buildEnqueuePayload(url, filename, overrides);
  const response = await sendToHelper(payload);
  if (response?.ok === false) {
    throw new Error(response.message || "Download request was rejected by the helper.");
  }
  return response;
}

async function listJobs() {
  return sendToHelper({ type: "list" });
}

async function openJobLocation(jobId) {
  return sendToHelper({ type: "open-location", jobId });
}

async function resumeJob(jobId) {
  return sendToHelper({ type: "resume-job", jobId });
}

async function cancelJob(jobId) {
  return sendToHelper({ type: "cancel-job", jobId });
}

async function removeJob(jobId) {
  return sendToHelper({ type: "remove-job", jobId });
}

async function getHelperHealth() {
  try {
    const jobs = await listJobs();
    return {
      ok: true,
      helperName: (await getSettings()).helperName,
      jobCount: Array.isArray(jobs) ? jobs.length : 0,
    };
  } catch (error) {
    return {
      ok: false,
      helperName: (await getSettings()).helperName,
      error: String(error),
    };
  }
}

browser.runtime.onInstalled.addListener(() => {
  browser.contextMenus.create({
    id: "robot-download-with-helper",
    title: "Download with Robot Downloader",
    contexts: ["link", "video", "audio", "all"],
  });
});

function deriveFilenameFromContext(info = {}) {
  const candidates = [info.linkText, info.selectionText, info.srcUrl, info.linkUrl].filter(Boolean);
  for (const candidate of candidates) {
    try {
      const parsed = new URL(candidate);
      const base = parsed.pathname.split("/").filter(Boolean).pop();
      if (base) return decodeURIComponent(base);
    } catch (_) {
      const trimmed = String(candidate).trim();
      if (trimmed && !trimmed.startsWith("http://") && !trimmed.startsWith("https://")) {
        return trimmed;
      }
    }
  }
  return null;
}

function isProbablyDirectMediaURL(url = "") {
  const lowered = String(url).toLowerCase();
  return /\.(mp4|m4v|webm|mkv|mov|mp3|m4a|aac|flac|wav|ogg|m3u8|mpd)(\?|$)/.test(lowered);
}

function isSupportedPageVideoURL(url = "") {
  try {
    const parsed = new URL(url);
    const host = parsed.hostname.toLowerCase();
    if (host === "youtu.be") {
      return true;
    }
    if (host === "youtube.com" || host.endsWith(".youtube.com")) {
      return parsed.pathname === "/watch";
    }
    return false;
  } catch (_) {
    return false;
  }
}

function getUnsupportedURLMessage(url = "") {
  try {
    const parsed = new URL(url);
    const host = parsed.hostname.toLowerCase();
    if (host) {
      return `This site is not supported yet (${host}). Right now Robot Downloader works with direct media links, HLS/DASH manifests, and YouTube video pages.`;
    }
  } catch (_) {
    // ignore
  }
  return "This link is not supported yet. Right now Robot Downloader works with direct media links, HLS/DASH manifests, and YouTube video pages.";
}

function shouldPreferPageVideoURL(targetURL = "", pageURL = "") {
  if (!isSupportedPageVideoURL(pageURL)) return false;
  try {
    const parsed = new URL(targetURL);
    const host = parsed.hostname.toLowerCase();
    return host === "youtu.be"
      || host === "youtube.com"
      || host.endsWith(".youtube.com")
      || host.endsWith(".googlevideo.com");
  } catch (_) {
    return !targetURL || String(targetURL).startsWith("blob:");
  }
}

function rememberMediaRequest(tabId, url) {
  if (typeof tabId !== "number" || tabId < 0 || !url) return;
  recentMediaByTab.set(tabId, {
    url,
    seenAt: Date.now(),
  });
}

function rememberYouTubePlaybackRequest(tabId, details) {
  if (typeof tabId !== "number" || tabId < 0 || !details?.url) return;
  const previous = recentYouTubePlaybackByTab.get(tabId) || {};
  recentYouTubePlaybackByTab.set(tabId, {
    ...previous,
    url: details.url,
    method: details.method || previous.method || "GET",
    originUrl: details.originUrl || previous.originUrl || "",
    documentUrl: details.documentUrl || previous.documentUrl || "",
    requestBody: details.requestBody || previous.requestBody || null,
    requestHeaders: details.requestHeaders || previous.requestHeaders || {},
    responseHeaders: details.responseHeaders || previous.responseHeaders || {},
    seenAt: Date.now(),
  });
}

function getRecentYouTubePlayback(tabId) {
  const record = recentYouTubePlaybackByTab.get(tabId);
  if (!record) return null;
  if (Date.now() - record.seenAt > 10 * 60 * 1000) {
    recentYouTubePlaybackByTab.delete(tabId);
    return null;
  }
  return record;
}

function getRememberedMedia(tabId) {
  const record = recentMediaByTab.get(tabId);
  if (!record) return null;
  if (Date.now() - record.seenAt > 10 * 60 * 1000) {
    recentMediaByTab.delete(tabId);
    return null;
  }
  return record.url;
}

browser.contextMenus.onClicked.addListener(async (info, tab) => {
  let targetURL = info.linkUrl || info.srcUrl;
  let filename = deriveFilenameFromContext(info);

  if ((!targetURL || String(targetURL).startsWith("blob:")) && lastHoveredMedia?.tabId === tab?.id) {
    const directURL = [lastHoveredMedia.sourceURL, lastHoveredMedia.url].find((url) => url && !String(url).startsWith("blob:"));
    if (directURL) {
      targetURL = directURL;
      filename = lastHoveredMedia.filename || filename || deriveFilenameFromContext({ srcUrl: directURL });
    }
  }

  if ((!targetURL || String(targetURL).startsWith("blob:")) && tab?.id != null) {
    const remembered = getRememberedMedia(tab.id);
    if (remembered) {
      targetURL = remembered;
      if (!filename) {
        filename = deriveFilenameFromContext({ srcUrl: remembered });
      }
    }
  }

  if (info.menuItemId !== "robot-download-with-helper") {
    return;
  }

  if (tab?.url && shouldPreferPageVideoURL(targetURL, tab.url)) {
    targetURL = tab.url;
    filename = filename || deriveFilenameFromContext({ srcUrl: tab.url });
  }

  if (!targetURL || String(targetURL).startsWith("blob:")) {
    await notify("Robot Downloader", "Could not find a playable media URL yet. Try playing the video for a moment, then try again.");
    return;
  }

  if (!isProbablyDirectMediaURL(targetURL) && !isSupportedPageVideoURL(targetURL)) {
    await notify("Robot Downloader", getUnsupportedURLMessage(targetURL));
    return;
  }

  try {
    const result = await queueDownloadFromLink(targetURL, filename);
    await notify("Robot Downloader", `Queued download job ${result.jobId || "(pending)"}`);
  } catch (error) {
    await notify("Robot Downloader error", String(error));
  }
});

browser.webRequest.onBeforeRequest.addListener((details) => {
  if (details.tabId < 0 || !details.url) return;

  try {
    const parsed = new URL(details.url);
    const host = parsed.hostname.toLowerCase();
    const path = parsed.pathname || "";
    if (host.endsWith('.googlevideo.com') && (path.includes('/videoplayback') || path.includes('/initplayback'))) {
      if (parsed.searchParams.get('sabr') === '1' || details.method === 'POST' || path.includes('/initplayback')) {
        rememberYouTubePlaybackRequest(details.tabId, details);
      }
    }
  } catch (_) {
    // ignore
  }

  if (details.type !== "media" && !isProbablyDirectMediaURL(details.url)) return;
  rememberMediaRequest(details.tabId, details.url);
}, { urls: ["<all_urls>"] }, ["requestBody"]);

browser.webRequest.onBeforeSendHeaders.addListener((details) => {
  if (details.tabId < 0 || !details.url) return;
  try {
    const parsed = new URL(details.url);
    const host = parsed.hostname.toLowerCase();
    const path = parsed.pathname || "";
    if (host.endsWith('.googlevideo.com') && (path.includes('/videoplayback') || path.includes('/initplayback'))) {
      rememberYouTubePlaybackRequest(details.tabId, {
        ...details,
        requestHeaders: sanitizeCapturedHeaders(details.requestHeaders || []),
      });
    }
  } catch (_) {
    // ignore
  }
}, { urls: ["<all_urls>"] }, ["requestHeaders"]);

browser.webRequest.onHeadersReceived.addListener((details) => {
  if (details.tabId < 0 || !details.url) return;
  try {
    const parsed = new URL(details.url);
    const host = parsed.hostname.toLowerCase();
    const path = parsed.pathname || "";
    if (host.endsWith('.googlevideo.com') && (path.includes('/videoplayback') || path.includes('/initplayback'))) {
      rememberYouTubePlaybackRequest(details.tabId, {
        ...details,
        responseHeaders: sanitizeCapturedHeaders(details.responseHeaders || []),
      });
    }
  } catch (_) {
    // ignore
  }
}, { urls: ["<all_urls>"] }, ["responseHeaders"]);

browser.downloads.onCreated.addListener(async (downloadItem) => {
  const settings = await getSettings();
  if (!settings.autoIntercept || !downloadItem.url) {
    return;
  }

  try {
    await browser.downloads.cancel(downloadItem.id);
    const result = await queueDownloadFromLink(downloadItem.url, downloadItem.filename);
    await notify("Robot Downloader", `Intercepted and queued ${result.jobId || downloadItem.filename || downloadItem.url}`);
  } catch (error) {
    console.error("[Robot Downloader] failed to intercept", error);
  }
});

browser.runtime.onMessage.addListener((message, sender) => {
  const senderTabId = sender?.tab?.id;

  if (message?.type === "set-hovered-media") {
    lastHoveredMedia = { ...(message.payload || {}), tabId: senderTabId };
    const directURL = [message.payload?.url, message.payload?.sourceURL].find((url) => url && !String(url).startsWith("blob:"));
    if (directURL && senderTabId != null) {
      rememberMediaRequest(senderTabId, directURL);
    }
    return Promise.resolve({ ok: true });
  }

  if (message?.type === "clear-hovered-media") {
    if (lastHoveredMedia?.tabId == null || lastHoveredMedia.tabId === senderTabId) {
      lastHoveredMedia = null;
    }
    return Promise.resolve({ ok: true });
  }

  if (message?.type === "get-jobs") {
    return listJobs().then((jobs) => ({ jobs }));
  }

  if (message?.type === "enqueue-job") {
    let targetURL = message.url;
    let filename = message.filename;
    const senderPageURL = sender?.tab?.url || "";
    const supportedPageURL = isSupportedPageVideoURL(message.pageURL || "")
      ? message.pageURL
      : (isSupportedPageVideoURL(senderPageURL) ? senderPageURL : "");

    if (supportedPageURL) {
      targetURL = supportedPageURL;
      if (!filename) {
        filename = null;
      }
    }

    if ((!targetURL || String(targetURL).startsWith("blob:")) && senderTabId != null) {
      const remembered = getRememberedMedia(senderTabId);
      if (remembered) {
        targetURL = remembered;
        filename = filename || deriveFilenameFromContext({ srcUrl: remembered });
      }
    }

    if ((!targetURL || String(targetURL).startsWith("blob:")) && lastHoveredMedia?.tabId === senderTabId) {
      const directURL = [lastHoveredMedia.sourceURL, lastHoveredMedia.url].find((url) => url && !String(url).startsWith("blob:"));
      if (directURL) {
        targetURL = directURL;
        filename = filename || lastHoveredMedia.filename || deriveFilenameFromContext({ srcUrl: directURL });
      }
    }

    if (shouldPreferPageVideoURL(targetURL, supportedPageURL)) {
      targetURL = supportedPageURL;
      if (!filename) {
        filename = null;
      }
    }

    const recentYouTubePlayback = senderTabId != null ? getRecentYouTubePlayback(senderTabId) : null;

    if (!targetURL || String(targetURL).startsWith("blob:")) {
      return Promise.resolve({
        ok: false,
        message: supportedPageURL
          ? "Found the YouTube page, but could not hand it to the downloader yet. Please reload the extension and try again."
          : "Could not find a playable media URL yet. Play the video for a moment, then try again.",
      });
    }

    if (!isProbablyDirectMediaURL(targetURL) && !isSupportedPageVideoURL(targetURL)) {
      return Promise.resolve({
        ok: false,
        message: getUnsupportedURLMessage(targetURL),
      });
    }

    return queueDownloadFromLink(targetURL, filename, {
      youtubeQuality: message.youtubeQuality,
      youtubePlayback: recentYouTubePlayback,
    }).then((result) => ({
      ok: true,
      jobId: result.jobId,
      status: result.status,
      message: result.message || `Queued ${targetURL}`,
    })).catch((error) => ({
      ok: false,
      message: String(error?.message || error),
    }));
  }

  if (message?.type === "get-settings") {
    return getSettings().then((settings) => ({ settings }));
  }

  if (message?.type === "save-settings") {
    return saveSettings(message.settings).then((settings) => ({ ok: true, settings }));
  }

  if (message?.type === "helper-health") {
    return getHelperHealth();
  }

  if (message?.type === "app-health") {
    return sendToHelper({ type: "app-health" });
  }

  if (message?.type === "open-app") {
    return sendToHelper({ type: "open-app" });
  }

  if (message?.type === "open-job-location") {
    return openJobLocation(message.jobId).then((result) => ({
      ok: true,
      jobId: result.jobId || message.jobId,
      message: result.message || "Opened file location.",
    }));
  }

  if (message?.type === "resume-job") {
    return resumeJob(message.jobId).then((result) => ({
      ok: true,
      jobId: result.jobId || message.jobId,
      status: result.status,
      message: result.message || "Resuming job.",
    }));
  }

  if (message?.type === "cancel-job") {
    return cancelJob(message.jobId).then((result) => ({
      ok: true,
      jobId: result.jobId || message.jobId,
      status: result.status,
      message: result.message || "Canceled job.",
    }));
  }

  if (message?.type === "remove-job") {
    return removeJob(message.jobId).then((result) => ({
      ok: true,
      jobId: result.jobId || message.jobId,
      message: result.message || "Removed job and data.",
    }));
  }

  return false;
});

// Open the helper app when the extension icon is clicked
browser.browserAction.onClicked.addListener(() => {
  sendToHelper({ type: "open-app" }).catch(() => {
    notify("Robot Downloader", "Could not open the downloader app. Is the helper running?");
  });
});
