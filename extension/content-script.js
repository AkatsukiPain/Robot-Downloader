(() => {
  const BUTTON_ID = "robot-downloader-video-button";
  const WRAPPER_ID = BUTTON_ID + "-wrapper";
  const QUALITY_ID = BUTTON_ID + "-quality";
  let activeVideo = null;
  let lastHoverPayload = null;

  function sendHoverPayload(payload) {
    lastHoverPayload = payload;
    browser.runtime.sendMessage({ type: "set-hovered-media", payload }).catch(() => {});
  }

  function clearHoverPayload() {
    lastHoverPayload = null;
    browser.runtime.sendMessage({ type: "clear-hovered-media" }).catch(() => {});
  }

  function isYouTube() {
    const host = location.hostname.toLowerCase();
    return host === "youtube.com" || host.endsWith(".youtube.com") || host === "youtu.be";
  }

  function getOrCreateWrapper() {
    let wrapper = document.getElementById(WRAPPER_ID);
    if (wrapper) return wrapper;

    wrapper = document.createElement("div");
    wrapper.id = WRAPPER_ID;
    Object.assign(wrapper.style, {
      position: "fixed",
      zIndex: "2147483647",
      display: "none",
      pointerEvents: "auto",
      userSelect: "none",
      whiteSpace: "nowrap",
    });

    const button = document.createElement("button");
    button.id = BUTTON_ID;
    button.type = "button";
    button.textContent = "Download";
    Object.assign(button.style, {
      padding: "6px 12px",
      borderRadius: "999px 0 0 999px",
      border: "1px solid rgba(255,255,255,0.18)",
      borderRight: "none",
      background: "rgba(37,99,235,0.95)",
      color: "#fff",
      fontSize: "12px",
      fontWeight: "600",
      cursor: "pointer",
      boxShadow: "0 10px 25px rgba(15,23,42,0.35)",
      backdropFilter: "blur(6px)",
      verticalAlign: "top",
      lineHeight: "normal",
      whiteSpace: "nowrap",
    });

    const qualitySelect = document.createElement("select");
    qualitySelect.id = QUALITY_ID;
    const qualityOptions = [
      { value: "highest", label: "Auto (best)" },
      { value: "2160p", label: "Up to 2160p" },
      { value: "1440p", label: "Up to 1440p" },
      { value: "1080p", label: "Up to 1080p" },
      { value: "720p", label: "Up to 720p" },
      { value: "480p", label: "Up to 480p" },
      { value: "360p", label: "Up to 360p" },
    ];
    for (const opt of qualityOptions) {
      const el = document.createElement("option");
      el.value = opt.value;
      el.textContent = opt.label;
      qualitySelect.appendChild(el);
    }
    Object.assign(qualitySelect.style, {
      padding: "6px 4px 6px 8px",
      borderRadius: "0 999px 999px 0",
      border: "1px solid rgba(255,255,255,0.18)",
      background: "rgba(30,64,175,0.9)",
      color: "#fff",
      fontSize: "11px",
      fontWeight: "500",
      cursor: "pointer",
      outline: "none",
      backdropFilter: "blur(6px)",
      boxShadow: "0 10px 25px rgba(15,23,42,0.35)",
      verticalAlign: "top",
      lineHeight: "normal",
      appearance: "none",
      WebkitAppearance: "none",
      MozAppearance: "none",
      textAlignLast: "center",
    });

    wrapper.appendChild(button);
    wrapper.appendChild(qualitySelect);

    const handleQueue = async (event) => {
      event.preventDefault();
      event.stopPropagation();
      event.stopImmediatePropagation();
      if (!activeVideo) return;

      const payload = getVideoPayload(activeVideo);
      const effectiveURL = payload.url || payload.sourceURL || (isKnownPageBackedVideoHost(location.href) ? location.href : "");
      if (!effectiveURL) {
        button.textContent = "No URL";
        setTimeout(() => { button.textContent = "Download"; }, 1800);
        return;
      }

      button.disabled = true;
      button.textContent = "Queueing…";
      try {
        const quality = qualitySelect.value;
        const usePageBackedFilename = isKnownPageBackedVideoHost(location.href);
        const response = await browser.runtime.sendMessage({
          type: "enqueue-job",
          url: effectiveURL,
          filename: usePageBackedFilename ? null : payload.filename,
          pageURL: payload.pageURL,
          youtubeQuality: quality,
        });
        button.textContent = response?.ok ? "Queued" : (response?.message || "Failed");
      } catch (error) {
        button.textContent = "Failed";
      } finally {
        setTimeout(() => {
          button.disabled = false;
          button.textContent = "Download";
        }, 1600);
      }
    };

    button.addEventListener("pointerdown", handleQueue, true);
    button.addEventListener("click", (event) => {
      event.preventDefault();
      event.stopPropagation();
      event.stopImmediatePropagation();
    }, true);

    qualitySelect.addEventListener("pointerdown", (event) => {
      event.stopPropagation();
    }, true);
    qualitySelect.addEventListener("click", (event) => {
      event.stopPropagation();
    }, true);

    (document.body || document.documentElement).appendChild(wrapper);
    return wrapper;
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
      const parsed = new URL(url, location.href);
      const host = parsed.hostname.toLowerCase();
      if (!(host === "youtube.com" || host.endsWith(".youtube.com") || host === "youtu.be")) {
        return false;
      }
      return parsed.pathname === "/watch" || host === "youtu.be";
    } catch (_) {
      return false;
    }
  }

  function positionButton(video) {
    const wrapper = getOrCreateWrapper();
    const rect = video.getBoundingClientRect();
    const top = Math.max(8, rect.top + 8);
    const left = Math.max(8, rect.left + 8);
    wrapper.style.top = `${top}px`;
    wrapper.style.left = `${left}px`;
    wrapper.style.display = rect.width > 120 && rect.height > 80 ? "block" : "none";
  }

  async function showForVideo(video) {
    activeVideo = video;
    sendHoverPayload(getVideoPayload(video));
    const wrapper = getOrCreateWrapper();
    const qualitySelect = document.getElementById(QUALITY_ID);
    if (qualitySelect) {
      try {
        const result = await browser.storage.local.get("settings");
        if (result.settings?.youtubeQuality) {
          qualitySelect.value = result.settings.youtubeQuality;
        }
      } catch (_) {}
    }
    wrapper.style.display = "block";
    positionButton(video);
  }

  function hideButton() {
    const wrapper = document.getElementById(WRAPPER_ID);
    if (!wrapper) return;
    wrapper.style.display = "none";
    activeVideo = null;
    clearHoverPayload();
  }

  // --- YouTube-specific: permanent button in the player controls area ---
  function injectYouTubeButton() {
    if (!isYouTube()) return;
    const existing = document.getElementById("robot-downloader-yt-button");
    if (existing) return;

    const findAndInject = () => {
      let container = document.querySelector(".ytp-right-controls");
      if (!container) {
        container = document.querySelector(".ytp-chrome-top .ytp-buttons");
      }
      if (!container) return false;

      if (document.getElementById("robot-downloader-yt-button")) return true;

      const ytBtn = document.createElement("button");
      ytBtn.id = "robot-downloader-yt-button";
      ytBtn.className = "ytp-button";
      ytBtn.title = "Download with Robot Downloader";
      ytBtn.innerHTML = `<svg height="100%" viewBox="0 0 24 24" fill="white"><path d="M19 9h-4V3H9v6H5l7 7 7-7zM5 18v2h14v-2H5z"/></svg>`;
      Object.assign(ytBtn.style, {
        cursor: "pointer",
        opacity: "0.9",
        width: "48px",
        height: "48px",
        padding: "8px",
      });

      const qualitySelect = document.createElement("select");
      qualitySelect.id = "robot-downloader-yt-quality";
      const qualityOptions = [
        { value: "highest", label: "Auto (best)" },
        { value: "2160p", label: "Up to 2160p" },
        { value: "1440p", label: "Up to 1440p" },
        { value: "1080p", label: "Up to 1080p" },
        { value: "720p", label: "Up to 720p" },
        { value: "480p", label: "Up to 480p" },
        { value: "360p", label: "Up to 360p" },
      ];
      for (const opt of qualityOptions) {
        const el = document.createElement("option");
        el.value = opt.value;
        el.textContent = opt.label;
        qualitySelect.appendChild(el);
      }
      Object.assign(qualitySelect.style, {
        background: "transparent",
        color: "#fff",
        border: "1px solid rgba(255,255,255,0.3)",
        borderRadius: "4px",
        fontSize: "11px",
        padding: "2px 4px",
        cursor: "pointer",
        outline: "none",
        verticalAlign: "middle",
        marginRight: "8px",
      });

      browser.storage.local.get("settings").then((result) => {
        if (result.settings?.youtubeQuality) {
          qualitySelect.value = result.settings.youtubeQuality;
        }
      }).catch(() => {});

      ytBtn.addEventListener("click", async (event) => {
        event.stopPropagation();
        const video = document.querySelector("video.html5-main-video") || document.querySelector("video");
        if (!video) return;

        activeVideo = video;
        const payload = getVideoPayload(video);
        const effectiveURL = location.href;

        ytBtn.style.opacity = "0.5";
        ytBtn.title = "Queueing…";
        try {
          const quality = qualitySelect.value;
          const response = await browser.runtime.sendMessage({
            type: "enqueue-job",
            url: effectiveURL,
            filename: null,
            pageURL: location.href,
            youtubeQuality: quality,
          });
          ytBtn.title = response?.ok ? "Queued!" : (response?.message || "Failed");
        } catch (error) {
          ytBtn.title = "Failed";
        } finally {
          setTimeout(() => {
            ytBtn.style.opacity = "0.9";
            ytBtn.title = "Download with Robot Downloader";
          }, 2000);
        }
      });

      container.prepend(qualitySelect);
      container.prepend(ytBtn);
      return true;
    };

    if (!findAndInject()) {
      let attempts = 0;
      const interval = setInterval(() => {
        attempts++;
        if (findAndInject() || attempts > 20) {
          clearInterval(interval);
        }
      }, 500);
    }
  }

  // --- Always show button on the first/largest video ---
  function attach(video) {
    if (!(video instanceof HTMLVideoElement) || video.dataset.robotDownloaderBound === "1") return;
    video.dataset.robotDownloaderBound = "1";
    // Just mark it, we'll pick the best video to show on
  }

  function pickVideo() {
    const videos = document.querySelectorAll("video");
    // Pick the largest visible video
    let best = null;
    let bestArea = 0;
    for (const v of videos) {
      const r = v.getBoundingClientRect();
      const area = r.width * r.height;
      if (area > bestArea && r.width > 120 && r.height > 80) {
        bestArea = area;
        best = v;
      }
    }
    return best;
  }

  function updateButton() {
    const video = pickVideo();
    if (video) {
      showForVideo(video);
      // Also reposition on scroll/resize
    } else {
      hideButton();
    }
  }

  function scan() {
    document.querySelectorAll("video").forEach(attach);
    injectYouTubeButton();
    updateButton();
  }

  // Re-position on scroll/resize
  window.addEventListener("scroll", () => {
    if (activeVideo) positionButton(activeVideo);
  }, true);
  window.addEventListener("resize", () => {
    if (activeVideo) positionButton(activeVideo);
  });

  // Watch for new videos
  const observer = new MutationObserver(() => {
    scan();
  });
  observer.observe(document.documentElement, { childList: true, subtree: true });

  // Initial scan
  scan();
})();