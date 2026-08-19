(() => {
  const BUTTON_ID = "robot-downloader-video-button";
  let activeVideo = null;
  let hideTimer = null;
  let lastHoverPayload = null;

  function sendHoverPayload(payload) {
    lastHoverPayload = payload;
    browser.runtime.sendMessage({ type: "set-hovered-media", payload }).catch(() => {});
  }

  function clearHoverPayload() {
    lastHoverPayload = null;
    browser.runtime.sendMessage({ type: "clear-hovered-media" }).catch(() => {});
  }

  function getOrCreateButton() {
    let button = document.getElementById(BUTTON_ID);
    if (button) return button;

    button = document.createElement("button");
    button.id = BUTTON_ID;
    button.type = "button";
    button.textContent = "Download with Robot Downloader";
    Object.assign(button.style, {
      position: "fixed",
      zIndex: "2147483647",
      display: "none",
      padding: "8px 12px",
      borderRadius: "999px",
      border: "1px solid rgba(255,255,255,0.18)",
      background: "rgba(37,99,235,0.95)",
      color: "#fff",
      fontSize: "12px",
      fontWeight: "600",
      cursor: "pointer",
      boxShadow: "0 10px 25px rgba(15,23,42,0.35)",
      backdropFilter: "blur(6px)",
    });

    button.addEventListener("mouseenter", () => {
      if (hideTimer) {
        clearTimeout(hideTimer);
        hideTimer = null;
      }
    });

    button.addEventListener("mouseleave", scheduleHide);
    button.addEventListener("click", async (event) => {
      event.preventDefault();
      event.stopPropagation();
      if (!activeVideo) return;

      const payload = getVideoPayload(activeVideo);
      const effectiveURL = payload.url || payload.sourceURL || (isKnownPageBackedVideoHost(location.href) ? location.href : "");
      if (!effectiveURL) {
        button.textContent = "Direct video URL not available";
        setTimeout(() => {
          button.textContent = "Download with Robot Downloader";
        }, 1800);
        return;
      }

      button.disabled = true;
      button.textContent = "Queueing…";
      try {
        const response = await browser.runtime.sendMessage({
          type: "enqueue-job",
          url: effectiveURL,
          filename: payload.filename,
          pageURL: payload.pageURL,
        });
        button.textContent = response?.message || (response?.ok === false ? "Need direct media URL" : "Queued");
      } catch (error) {
        button.textContent = "Queue failed";
      } finally {
        setTimeout(() => {
          button.disabled = false;
          button.textContent = "Download with Robot Downloader";
        }, 1600);
      }
    });

    document.documentElement.appendChild(button);
    return button;
  }

  function guessFilename(url) {
    try {
      const parsed = new URL(url, location.href);
      const last = parsed.pathname.split("/").filter(Boolean).pop();
      if (last) return decodeURIComponent(last);
    } catch (_) {
      // ignore
    }

    const title = (document.title || "video").trim().replace(/[\\/:*?"<>|]+/g, "_");
    return `${title || "video"}.mp4`;
  }

  function getVideoPayload(video) {
    const url = video.currentSrc || video.src || "";
    const sourceURL = Array.from(video.querySelectorAll("source"))
      .map((node) => node.src)
      .find(Boolean) || "";
    return {
      url,
      sourceURL,
      filename: guessFilename(sourceURL || url),
      pageURL: location.href,
      frameURL: location.href,
    };
  }

  function isKnownPageBackedVideoHost(url) {
    try {
      const host = new URL(url, location.href).hostname.toLowerCase();
      return host === "youtube.com" || host.endsWith(".youtube.com") || host === "youtu.be";
    } catch (_) {
      return false;
    }
  }

  function positionButton(video) {
    const button = getOrCreateButton();
    const rect = video.getBoundingClientRect();
    const top = Math.max(8, rect.top + 8);
    const left = Math.max(8, rect.right - button.offsetWidth - 8);
    button.style.top = `${top}px`;
    button.style.left = `${left}px`;
    button.style.display = rect.width > 120 && rect.height > 80 ? "block" : "none";
  }

  function showForVideo(video) {
    activeVideo = video;
    sendHoverPayload(getVideoPayload(video));
    const button = getOrCreateButton();
    button.style.display = "block";
    positionButton(video);
  }

  function hideButton() {
    const button = document.getElementById(BUTTON_ID);
    if (!button) return;
    button.style.display = "none";
    activeVideo = null;
    clearHoverPayload();
  }

  function scheduleHide() {
    if (hideTimer) clearTimeout(hideTimer);
    hideTimer = setTimeout(hideButton, 220);
  }

  function attach(video) {
    if (!(video instanceof HTMLVideoElement) || video.dataset.robotDownloaderBound === "1") return;
    video.dataset.robotDownloaderBound = "1";
    video.addEventListener("mouseenter", () => showForVideo(video));
    video.addEventListener("mousemove", () => {
      if (activeVideo === video) {
        sendHoverPayload(getVideoPayload(video));
        positionButton(video);
      }
    });
    video.addEventListener("mouseleave", scheduleHide);
  }

  function scan() {
    document.querySelectorAll("video").forEach(attach);
  }

  document.addEventListener("contextmenu", (event) => {
    const video = event.target?.closest?.("video");
    if (video) {
      showForVideo(video);
    }
  }, true);

  const observer = new MutationObserver(() => scan());
  observer.observe(document.documentElement, { childList: true, subtree: true });

  window.addEventListener("scroll", () => {
    if (activeVideo) positionButton(activeVideo);
  }, true);
  window.addEventListener("resize", () => {
    if (activeVideo) positionButton(activeVideo);
  });
  window.addEventListener("blur", scheduleHide);

  scan();
})();
