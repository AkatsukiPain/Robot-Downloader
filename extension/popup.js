const state = {
  jobs: [],
  autoRefresh: true,
  autoRefreshTimer: null,
  openingLocationFor: null,
  resumingJobFor: null,
  cancelingJobFor: null,
  removingJobFor: null,
  visibleJobLimit: 5,
  progressSnapshots: {},
  theme: "light",
};

function formatBytes(value) {
  if (!value) return "unknown size";
  const units = ["B", "KB", "MB", "GB", "TB"];
  let size = value;
  let unit = 0;
  while (size >= 1024 && unit < units.length - 1) {
    size /= 1024;
    unit += 1;
  }
  return `${size.toFixed(size >= 10 || unit === 0 ? 0 : 1)} ${units[unit]}`;
}

function formatSpeed(bytesPerSecond) {
  if (!bytesPerSecond || bytesPerSecond <= 0) return "—";
  return `${formatBytes(bytesPerSecond)}/s`;
}

function formatDuration(seconds) {
  if (!seconds || seconds <= 0 || !Number.isFinite(seconds)) return "—";
  const totalSeconds = Math.max(1, Math.round(seconds));
  const hours = Math.floor(totalSeconds / 3600);
  const minutes = Math.floor((totalSeconds % 3600) / 60);
  const secs = totalSeconds % 60;

  if (hours > 0) return `${hours}h ${minutes}m`;
  if (minutes > 0) return `${minutes}m ${secs}s`;
  return `${secs}s`;
}

function formatDate(value) {
  if (!value) return "—";
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return "—";
  return new Intl.DateTimeFormat(undefined, {
    month: "short",
    day: "numeric",
    hour: "2-digit",
    minute: "2-digit",
  }).format(date);
}

function escapeHtml(value) {
  return String(value)
    .replaceAll("&", "&amp;")
    .replaceAll("<", "&lt;")
    .replaceAll(">", "&gt;")
    .replaceAll('"', "&quot;")
    .replaceAll("'", "&#39;");
}

function applyTheme(theme) {
  state.theme = theme === "light" ? "light" : "dark";
  document.documentElement.setAttribute("data-theme", state.theme);
  const button = document.getElementById("themeToggleButton");
  if (button) {
    button.textContent = state.theme === "light" ? "Dark theme" : "Light theme";
  }
}

function setSettingsMessage(message, isError = false) {
  const el = document.getElementById("settingsMessage");
  el.textContent = message;
  el.classList.toggle("muted", !isError);
  el.style.color = isError ? "#fca5a5" : "";
}

function setJobsInfo(message, isError = false) {
  const el = document.getElementById("jobsInfoMessage");
  el.textContent = message;
  el.classList.toggle("muted", !isError);
  el.style.color = isError ? "#fca5a5" : "";
}

function fillSettingsForm(settings) {
  document.getElementById("helperNameInput").value = settings.helperName || "robot.downloader";
  document.getElementById("maxConnectionsInput").value = settings.maxConnections ?? 8;
  document.getElementById("chunkSizeInput").value = settings.chunkSizeMb ?? 8;
  document.getElementById("retryCountInput").value = settings.retryCount ?? 3;
  document.getElementById("autoInterceptInput").checked = Boolean(settings.autoIntercept);
  applyTheme(settings.theme || "light");
}

function renderSummary(jobs) {
  const counts = jobs.reduce((acc, job) => {
    acc.total += 1;
    if (job.status === "completed") acc.completed += 1;
    else if (job.status === "failed") acc.failed += 1;
    else if (job.status === "downloading") acc.active += 1;
    return acc;
  }, { total: 0, active: 0, completed: 0, failed: 0 });

  document.getElementById("summaryTotal").textContent = String(counts.total);
  document.getElementById("summaryActive").textContent = String(counts.active);
  document.getElementById("summaryCompleted").textContent = String(counts.completed);
  document.getElementById("summaryFailed").textContent = String(counts.failed);
}

function getFilteredJobs() {
  const query = document.getElementById("searchInput").value.trim().toLowerCase();
  const statusFilter = document.getElementById("statusFilterInput").value;

  return state.jobs.filter((job) => {
    const matchesStatus = statusFilter === "all" || job.status === statusFilter;
    if (!matchesStatus) return false;
    if (!query) return true;

    const haystack = [job.filename, job.url, job.jobId, job.status]
      .filter(Boolean)
      .join(" ")
      .toLowerCase();
    return haystack.includes(query);
  }).slice(0, state.visibleJobLimit);
}

function canOpenLocation(job) {
  return ["completed", "downloading", "failed", "planned", "queued"].includes(job.status || "");
}

function getResumeLabel(job) {
  if (job.status !== "failed") return "";
  if (job.canResume) return state.resumingJobFor === job.jobId ? "Resuming…" : "Resume";
  return "Unable to resume";
}

function canCancel(job) {
  return ["downloading", "queued", "planned"].includes(job.status || "");
}

function getAverageSpeed(job) {
  if ((job.status || "") !== "downloading") return 0;
  const downloadedBytes = Number(job.downloadedBytes || 0);
  if (downloadedBytes <= 0) return 0;

  const startedAt = new Date(job.createdAt || 0).getTime();
  const updatedAt = new Date(job.modifiedAt || 0).getTime();
  if (!startedAt || !updatedAt || Number.isNaN(startedAt) || Number.isNaN(updatedAt) || updatedAt <= startedAt) {
    return 0;
  }

  const seconds = Math.max(1, Math.round((updatedAt - startedAt) / 1000));
  return downloadedBytes / seconds;
}

function updateProgressSnapshots(jobs) {
  const nextSnapshots = {};
  for (const job of jobs) {
    const previous = state.progressSnapshots[job.jobId];
    const current = {
      downloadedBytes: Number(job.downloadedBytes || 0),
      modifiedAt: new Date(job.modifiedAt || 0).getTime(),
      previous,
    };
    nextSnapshots[job.jobId] = current;
  }
  state.progressSnapshots = nextSnapshots;
}

function getLiveSpeed(job) {
  if ((job.status || "") !== "downloading") return 0;
  const snapshot = state.progressSnapshots[job.jobId];
  const previous = snapshot?.previous;
  if (snapshot && previous) {
    const byteDelta = snapshot.downloadedBytes - previous.downloadedBytes;
    const timeDeltaMs = snapshot.modifiedAt - previous.modifiedAt;
    if (byteDelta > 0 && timeDeltaMs > 0) {
      return byteDelta / (timeDeltaMs / 1000);
    }
  }
  return getAverageSpeed(job);
}

function getRemainingBytes(job) {
  const totalBytes = Number(job.totalBytes || 0);
  const downloadedBytes = Number(job.downloadedBytes || 0);
  if (totalBytes <= 0) return 0;
  return Math.max(0, totalBytes - downloadedBytes);
}

function getEtaSeconds(job, speedBytesPerSecond) {
  const remainingBytes = getRemainingBytes(job);
  if (remainingBytes <= 0 || !speedBytesPerSecond || speedBytesPerSecond <= 0) return 0;
  return remainingBytes / speedBytesPerSecond;
}

function renderJobs(jobs) {
  const container = document.getElementById("jobsContainer");
  const jobCount = document.getElementById("jobCount");
  jobCount.textContent = String(jobs.length);

  if (!jobs.length) {
    container.className = "jobs empty";
    container.textContent = state.jobs.length ? "No jobs match the current filter." : "No jobs yet.";
    return;
  }

  container.className = "jobs";
  container.innerHTML = jobs.map((job) => {
    const totalBytes = Number(job.totalBytes || 0);
    const downloadedBytes = Number(job.downloadedBytes || 0);
    const safeFilename = escapeHtml(job.filename || "Unnamed file");
    const safeURL = escapeHtml(job.url || "");
    const status = escapeHtml(job.status || "unknown");
    const percent = totalBytes > 0 ? Math.max(0, Math.min(100, Math.round((downloadedBytes / totalBytes) * 100))) : 0;
    const progressLabel = totalBytes > 0
      ? `${formatBytes(downloadedBytes)} / ${formatBytes(totalBytes)}`
      : formatBytes(downloadedBytes || 0);

    const canOpen = canOpenLocation(job);
    const isOpening = state.openingLocationFor === job.jobId;
    const resumeLabel = getResumeLabel(job);
    const liveSpeed = getLiveSpeed(job);
    const remainingBytes = getRemainingBytes(job);
    const etaSeconds = getEtaSeconds(job, liveSpeed);

    return `
      <article class="job ${state.removingJobFor === job.jobId ? "job-removing" : ""}">
        <div class="job-header-row">
          <div class="job-header-main">
            <div class="job-name">${safeFilename}</div>
            <span class="pill status-${status}">${status}</span>
          </div>
          <div class="job-actions job-actions-top">
            ${job.status === "failed" ? `
              <button
                type="button"
                class="button secondary job-action-button"
                data-action="resume-job"
                data-job-id="${escapeHtml(job.jobId || "")}"${job.canResume ? "" : " disabled"}
                title="${escapeHtml(job.canResume ? "Resume this failed job" : (job.resumeReason || "This job cannot be resumed"))}"
              >${escapeHtml(resumeLabel)}</button>
            ` : ""}
            ${canCancel(job) ? `
              <button
                type="button"
                class="button secondary job-action-button"
                data-action="cancel-job"
                data-job-id="${escapeHtml(job.jobId || "")}"
              >${state.cancelingJobFor === job.jobId ? "Canceling…" : "Cancel"}</button>
            ` : ""}
            <button
              type="button"
              class="button secondary job-action-button"
              data-action="open-location"
              data-job-id="${escapeHtml(job.jobId || "")}"${canOpen ? "" : " disabled"}
            >${isOpening ? "Opening…" : "Open location"}</button>
            <button
              type="button"
              class="button secondary job-action-button danger-button"
              data-action="remove-job"
              data-job-id="${escapeHtml(job.jobId || "")}"
            >${state.removingJobFor === job.jobId ? "Removing…" : "Remove"}</button>
          </div>
        </div>
        <div class="job-subrow">
          <div class="job-time">Updated ${escapeHtml(formatDate(job.modifiedAt))}</div>
          <div class="job-time">Created ${escapeHtml(formatDate(job.createdAt))}</div>
        </div>
        <div class="job-url">${safeURL}</div>
        <div class="job-progress-row">
          <span class="job-time">${escapeHtml(progressLabel)}</span>
          <span class="job-time">${totalBytes > 0 ? `${percent}%` : "live"}</span>
        </div>
        <div class="job-progress-track" aria-hidden="true">
          <div class="job-progress-bar" style="width: ${percent}%;"></div>
        </div>
        <div class="job-meta">
          <span class="pill">Range support: ${job.acceptRanges ? "Yes" : "No"}</span>
          ${job.status === "downloading" ? `<span class="pill">Speed: ${escapeHtml(formatSpeed(liveSpeed))}</span>` : ""}
          ${job.status === "downloading" && remainingBytes > 0 ? `<span class="pill">Remaining: ${escapeHtml(formatBytes(remainingBytes))}</span>` : ""}
          ${job.status === "downloading" && etaSeconds > 0 ? `<span class="pill">ETA: ${escapeHtml(formatDuration(etaSeconds))}</span>` : ""}
          <span class="pill">Job ID: ${escapeHtml(job.jobId || "-")}</span>
        </div>
      </article>`;
  }).join("");
}

function refreshDerivedViews() {
  renderSummary(state.jobs);
  const filtered = getFilteredJobs();
  renderJobs(filtered);

  if (!state.jobs.length) {
    setJobsInfo("No jobs yet.");
    return;
  }

  const hasQuery = document.getElementById("searchInput").value.trim().length > 0;
  const statusFilter = document.getElementById("statusFilterInput").value;
  const isFiltered = hasQuery || statusFilter !== "all";
  const baseCount = isFiltered
    ? state.jobs.filter((job) => {
        const matchesStatus = statusFilter === "all" || job.status === statusFilter;
        if (!matchesStatus) return false;
        if (!hasQuery) return true;
        const haystack = [job.filename, job.url, job.jobId, job.status]
          .filter(Boolean)
          .join(" ")
          .toLowerCase();
        return haystack.includes(document.getElementById("searchInput").value.trim().toLowerCase());
      }).length
    : state.jobs.length;

  if (baseCount > state.visibleJobLimit) {
    setJobsInfo(`Showing the latest ${filtered.length} of ${baseCount} matching job(s).`);
    return;
  }

  setJobsInfo(
    isFiltered
      ? `Showing ${filtered.length} matching job(s).`
      : `Showing the latest ${filtered.length} job(s).`
  );
}

async function loadJobs() {
  try {
    const response = await browser.runtime.sendMessage({ type: "get-jobs" });
    state.jobs = response.jobs || [];
    updateProgressSnapshots(state.jobs);
    refreshDerivedViews();
  } catch (error) {
    setJobsInfo("Could not load jobs.", true);
  }
}

async function loadSettings() {
  try {
    const response = await browser.runtime.sendMessage({ type: "get-settings" });
    fillSettingsForm(response.settings || {});
    setSettingsMessage("Settings loaded.");
  } catch (error) {
    setSettingsMessage(String(error), true);
  }
}

async function refreshAll() {
  await Promise.all([loadJobs(), loadSettings()]);
}

function stopAutoRefresh() {
  if (state.autoRefreshTimer) {
    clearInterval(state.autoRefreshTimer);
    state.autoRefreshTimer = null;
  }
}

function startAutoRefresh() {
  stopAutoRefresh();
  if (!state.autoRefresh) return;
  state.autoRefreshTimer = setInterval(() => {
    Promise.all([loadJobs()]).catch(() => {});
  }, 1000);
}

document.getElementById("refreshButton").addEventListener("click", () => {
  refreshAll();
});

document.getElementById("themeToggleButton").addEventListener("click", async () => {
  const nextTheme = state.theme === "light" ? "dark" : "light";
  applyTheme(nextTheme);

  try {
    const response = await browser.runtime.sendMessage({
      type: "save-settings",
      settings: {
        helperName: document.getElementById("helperNameInput").value.trim(),
        maxConnections: document.getElementById("maxConnectionsInput").value,
        chunkSizeMb: document.getElementById("chunkSizeInput").value,
        retryCount: document.getElementById("retryCountInput").value,
        autoIntercept: document.getElementById("autoInterceptInput").checked,
        theme: nextTheme,
      },
    });
    fillSettingsForm(response.settings || { theme: nextTheme });
  } catch (error) {
    setJobsInfo(String(error), true);
  }
});

document.getElementById("searchInput").addEventListener("input", () => {
  refreshDerivedViews();
});

document.getElementById("statusFilterInput").addEventListener("change", () => {
  refreshDerivedViews();
});

document.getElementById("autoRefreshInput").addEventListener("change", (event) => {
  state.autoRefresh = Boolean(event.target.checked);
  startAutoRefresh();
  setJobsInfo(state.autoRefresh ? "Auto refresh enabled." : "Auto refresh paused.");
});

document.getElementById("settingsForm").addEventListener("submit", async (event) => {
  event.preventDefault();
  const settings = {
    helperName: document.getElementById("helperNameInput").value.trim(),
    maxConnections: document.getElementById("maxConnectionsInput").value,
    chunkSizeMb: document.getElementById("chunkSizeInput").value,
    retryCount: document.getElementById("retryCountInput").value,
    autoIntercept: document.getElementById("autoInterceptInput").checked,
    theme: state.theme,
  };

  try {
    const response = await browser.runtime.sendMessage({ type: "save-settings", settings });
    fillSettingsForm(response.settings || settings);
    setSettingsMessage("Settings saved.");
  } catch (error) {
    setSettingsMessage(String(error), true);
  }
});

document.getElementById("jobsContainer").addEventListener("click", async (event) => {
  const button = event.target.closest("[data-action]");
  if (!button) return;

  const action = button.dataset.action;
  const jobId = button.dataset.jobId;
  if (!jobId) return;

  if (action === "open-location") {
    state.openingLocationFor = jobId;
    refreshDerivedViews();
    setJobsInfo("Opening file location...");

    try {
      const response = await browser.runtime.sendMessage({ type: "open-job-location", jobId });
      setJobsInfo(response.message || "Opened file location.");
    } catch (error) {
      setJobsInfo(String(error), true);
    } finally {
      state.openingLocationFor = null;
      refreshDerivedViews();
    }
    return;
  }

  if (action === "resume-job") {
    state.resumingJobFor = jobId;
    refreshDerivedViews();
    setJobsInfo("Resuming failed job...");

    try {
      const response = await browser.runtime.sendMessage({ type: "resume-job", jobId });
      setJobsInfo(response.message || "Resuming job.");
      await loadJobs();
    } catch (error) {
      setJobsInfo(String(error), true);
    } finally {
      state.resumingJobFor = null;
      refreshDerivedViews();
    }
    return;
  }

  if (action === "cancel-job") {
    state.cancelingJobFor = jobId;
    refreshDerivedViews();
    setJobsInfo("Canceling job...");

    try {
      const response = await browser.runtime.sendMessage({ type: "cancel-job", jobId });
      setJobsInfo(response.message || "Canceled job.");
      await loadJobs();
    } catch (error) {
      setJobsInfo(String(error), true);
    } finally {
      state.cancelingJobFor = null;
      refreshDerivedViews();
    }
    return;
  }

  if (action === "remove-job") {
    state.removingJobFor = jobId;
    refreshDerivedViews();
    setJobsInfo("Removing job and data...");

    try {
      const response = await browser.runtime.sendMessage({ type: "remove-job", jobId });
      setJobsInfo(response.message || "Removed job and data.");
      await new Promise((resolve) => setTimeout(resolve, 220));
      await loadJobs();
    } catch (error) {
      setJobsInfo(String(error), true);
    } finally {
      state.removingJobFor = null;
      refreshDerivedViews();
    }
  }
});

window.addEventListener("unload", () => {
  stopAutoRefresh();
});

refreshAll();
startAutoRefresh();
