package downloader

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"robot-downloader/helper-go/internal/api"
	"robot-downloader/helper-go/internal/config"
	"robot-downloader/helper-go/internal/jobstate"
	"robot-downloader/helper-go/internal/storage"
)

const defaultUserAgent = "RobotDownloader/0.1"

var ytDLPProgressRE = regexp.MustCompile(`\[download\]\s+([0-9]+(?:\.[0-9]+)?)%\s+of\s+~?([0-9.]+[KMGTP]?i?B)(?:\s+at\s+(.+?))?(?:\s+ETA\s+([^\s]+))?$`)

type Service struct {
	cfg        config.Config
	store      *storage.JobStore
	httpClient *http.Client
	mu         sync.Mutex
	cancels    map[string]context.CancelFunc
}

func debugLog(format string, args ...any) {
	logPath := filepath.Join(os.TempDir(), "robot-downloader-helper.log")
	line := fmt.Sprintf("[%s] %s\n", time.Now().Format(time.RFC3339), fmt.Sprintf(format, args...))
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.WriteString(line)
}

func headerKeys(headers map[string]string) []string {
	keys := make([]string, 0, len(headers))
	for key := range headers {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

type probeResult struct {
	FinalURL           string
	Filename           string
	TotalBytes         int64
	AcceptRanges       bool
	ETag               string
	ContentType        string
	ContentDisposition string
	Extractor          string
	RequestHeaders     map[string]string
}

type ytDLPProbe struct {
	URL         string            `json:"url"`
	WebpageURL  string            `json:"webpage_url"`
	Title       string            `json:"title"`
	Ext         string            `json:"ext"`
	Protocol    string            `json:"protocol"`
	HTTPHeaders map[string]string `json:"http_headers"`
}

func NewService(cfg config.Config) *Service {
	return &Service{
		cfg:        cfg,
		store:      storage.NewJobStore(cfg.JobsDir),
		httpClient: &http.Client{Timeout: 0},
		cancels:    make(map[string]context.CancelFunc),
	}
}

func (s *Service) Enqueue(ctx context.Context, req api.EnqueueRequest) (jobstate.Job, error) {
	jobID, err := newJobID()
	if err != nil {
		return jobstate.Job{}, err
	}

	options := normalizeOptions(req.Options)
	mergedHeaders := enrichHeadersWithContext(req.URL, req.Context.PageURL, req.Context.Headers)
	chunksDir := filepath.Join(s.cfg.ChunksDir, jobID)
	if err := ensureDir(chunksDir); err != nil {
		return jobstate.Job{}, err
	}

	filename := chooseFilename(req.Filename, "", req.URL)
	now := time.Now().UTC()
	job := jobstate.Job{
		JobID:           jobID,
		URL:             req.URL,
		OriginalURL:     req.URL,
		Filename:        filename,
		Status:          "queued",
		CreatedAt:       now,
		ModifiedAt:      now,
		OutputPath:      filepath.Join(s.cfg.DownloadsDir, filename),
		ChunksDir:       chunksDir,
		MaxConnections:  options.MaxConnections,
		ChunkSizeBytes:  options.ChunkSizeBytes,
		RetryCount:      options.RetryCount,
		YouTubeQuality:  options.YouTubeQuality,
		DownloadedBytes: 0,
		RequestHeaders:  sanitizeHeaders(mergedHeaders),
		OriginalHeaders: sanitizeHeaders(mergedHeaders),
		OriginalPageURL: req.Context.PageURL,
		Chunks:          []jobstate.ChunkState{{Index: 0, Start: 0, End: -1, Completed: false, BytesSaved: 0}},
	}

	if err := s.store.Save(job); err != nil {
		return jobstate.Job{}, err
	}

	go s.startQueuedJob(ctx, req, job)

	return job, nil
}

func (s *Service) Download(ctx context.Context, job *jobstate.Job) error {
	if job.Extractor == "yt-dlp" {
		return s.downloadWithYTDLP(ctx, job)
	}
	if isStreamManifestURL(job.URL, job.ContentType) {
		return s.downloadStream(ctx, job)
	}

	if len(job.Chunks) == 0 {
		job.Chunks = planChunks(job.TotalBytes, job.ChunkSizeBytes, job.MaxConnections, job.AcceptRanges)
	}
	if len(job.Chunks) == 0 {
		return errors.New("job has no chunk plan")
	}

	if err := s.syncJobWithDisk(job); err != nil {
		return err
	}
	if allChunksCompleted(job.Chunks) {
		if err := s.mergeChunks(job); err != nil {
			return err
		}
		job.Status = "completed"
		job.DownloadedBytes = sumDownloadedBytes(job.Chunks)
		job.ModifiedAt = time.Now().UTC()
		job.LastError = ""
		return s.store.Save(*job)
	}

	job.Status = "downloading"
	job.ModifiedAt = time.Now().UTC()
	job.LastError = ""
	if err := s.store.Save(*job); err != nil {
		return err
	}

	sem := make(chan struct{}, max(1, job.MaxConnections))
	errCh := make(chan error, len(job.Chunks))
	var wg sync.WaitGroup
	var mu sync.Mutex

	for index := range job.Chunks {
		if job.Chunks[index].Completed {
			continue
		}

		wg.Add(1)
		go func(chunkIndex int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			bytesSaved, err := s.downloadChunkWithRetry(ctx, *job, chunkIndex)
			if err != nil {
				errCh <- fmt.Errorf("chunk %d: %w", chunkIndex, err)
				return
			}

			mu.Lock()
			job.Chunks[chunkIndex].BytesSaved = bytesSaved
			job.Chunks[chunkIndex].Completed = isChunkComplete(job.Chunks[chunkIndex])
			job.DownloadedBytes = sumDownloadedBytes(job.Chunks)
			job.ModifiedAt = time.Now().UTC()
			_ = s.store.Save(*job)
			mu.Unlock()
		}(index)
	}

	wg.Wait()
	close(errCh)

	for err := range errCh {
		if err != nil {
			job.Status = "failed"
			job.LastError = err.Error()
			job.ModifiedAt = time.Now().UTC()
			_ = s.store.Save(*job)
			return err
		}
	}

	if err := s.mergeChunks(job); err != nil {
		job.Status = "failed"
		job.LastError = err.Error()
		job.ModifiedAt = time.Now().UTC()
		_ = s.store.Save(*job)
		return err
	}

	job.Status = "completed"
	job.DownloadedBytes = sumDownloadedBytes(job.Chunks)
	job.ModifiedAt = time.Now().UTC()
	job.LastError = ""
	return s.store.Save(*job)
}

func (s *Service) List() ([]jobstate.Job, error) {
	jobs, err := s.store.List()
	if err != nil {
		return nil, err
	}
	for i := range jobs {
		if updated, changed := s.refreshLiveJobState(jobs[i]); changed {
			jobs[i] = updated
			_ = s.store.Save(updated)
		}
	}
	return jobs, nil
}

func (s *Service) OpenLocation(jobID string) error {
	job, err := s.store.Get(jobID)
	if err != nil {
		return err
	}

	targetPath := job.OutputPath
	if strings.TrimSpace(targetPath) == "" {
		return errors.New("job has no output path")
	}

	openPath := filepath.Dir(targetPath)
	if _, err := os.Stat(targetPath); err == nil {
		openPath = filepath.Dir(targetPath)
	} else if _, err := os.Stat(job.ChunksDir); err == nil {
		openPath = job.ChunksDir
	}

	return openPathInDesktop(openPath)
}

func openPathInDesktop(path string) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return errors.New("path is empty")
	}

	if err := startDetachedCommand("gio", "open", path); err == nil {
		return nil
	}
	return startDetachedCommand("xdg-open", path)
}

func startDetachedCommand(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	devNull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer devNull.Close()
	cmd.Stdin = devNull
	cmd.Stdout = devNull
	cmd.Stderr = devNull
	return cmd.Start()
}

func (s *Service) startQueuedJob(parentCtx context.Context, req api.EnqueueRequest, job jobstate.Job) {
	ctx, cancel := context.WithCancel(context.Background())
	s.mu.Lock()
	s.cancels[job.JobID] = cancel
	s.mu.Unlock()
	defer func() {
		cancel()
		s.mu.Lock()
		delete(s.cancels, job.JobID)
		s.mu.Unlock()
	}()

	mergedHeaders := enrichHeadersWithContext(req.URL, req.Context.PageURL, req.Context.Headers)
	job.Status = "probing"
	job.ModifiedAt = time.Now().UTC()
	job.LastError = ""
	job.OriginalHeaders = sanitizeHeaders(mergedHeaders)
	if strings.TrimSpace(req.Context.PageURL) != "" {
		job.OriginalPageURL = req.Context.PageURL
	}
	if strings.TrimSpace(req.URL) != "" {
		job.OriginalURL = req.URL
	}
	_ = s.store.Save(job)
	debugLog("startQueuedJob job=%s requestedURL=%s referer=%s headerKeys=%v", job.JobID, req.URL, mergedHeaders["Referer"], headerKeys(mergedHeaders))
	probe, err := s.probeURL(parentCtx, req.URL, req.Filename, mergedHeaders)
	debugLog("probe result job=%s finalURL=%s extractor=%s contentType=%s acceptRanges=%v requestHeaderKeys=%v err=%v", job.JobID, probe.FinalURL, probe.Extractor, probe.ContentType, probe.AcceptRanges, headerKeys(probe.RequestHeaders), err)
	if err != nil {
		job.Status = "failed"
		job.LastError = err.Error()
		job.ModifiedAt = time.Now().UTC()
		_ = s.store.Save(job)
		return
	}

	job.URL = probe.FinalURL
	job.Filename = ensureStreamFriendlyFilename(probe.FinalURL, probe.Filename, probe.ContentType)
	job.Status = initialStatusForProbe(probe)
	if probe.Extractor == "yt-dlp" {
		job.Status = "extracting"
	}
	job.ModifiedAt = time.Now().UTC()
	job.OutputPath = filepath.Join(s.cfg.DownloadsDir, job.Filename)
	job.TotalBytes = probe.TotalBytes
	job.AcceptRanges = probe.AcceptRanges
	job.ContentType = probe.ContentType
	job.ContentDisposition = probe.ContentDisposition
	job.ETag = probe.ETag
	job.RequestHeaders = sanitizeHeaders(mergedHeaders)
	if len(probe.RequestHeaders) > 0 {
		job.RequestHeaders = sanitizeHeaders(probe.RequestHeaders)
	}
	job.StreamKind = detectStreamKind(probe.FinalURL, probe.ContentType)
	job.Extractor = probe.Extractor
	job.ExtractorSourceURL = preferredExtractorURL(req.URL, mergedHeaders)
	job.Chunks = planChunks(probe.TotalBytes, job.ChunkSizeBytes, job.MaxConnections, probe.AcceptRanges)
	job.LastError = ""
	if err := s.store.Save(job); err != nil {
		return
	}

	if err := s.Download(ctx, &job); err != nil {
		if errors.Is(err, context.Canceled) {
			job.Status = "failed"
			job.LastError = "Canceled by user"
			job.ModifiedAt = time.Now().UTC()
			_ = s.store.Save(job)
			return
		}
		job.Status = "failed"
		job.LastError = err.Error()
		job.ModifiedAt = time.Now().UTC()
		_ = s.store.Save(job)
	}
}

func (s *Service) CanResume(job jobstate.Job) (bool, string) {
	if job.Status != "failed" {
		return false, "Only failed jobs can be resumed"
	}
	if strings.EqualFold(strings.TrimSpace(job.LastError), "Canceled by user") {
		return true, ""
	}
	if len(job.Chunks) == 0 {
		return false, "No saved chunk plan"
	}
	if !job.AcceptRanges && job.DownloadedBytes > 0 {
		return false, "Server does not support range resume"
	}
	return true, ""
}

func (s *Service) ResumeJob(ctx context.Context, jobID string) (jobstate.Job, error) {
	job, err := s.store.Get(jobID)
	if err != nil {
		return jobstate.Job{}, err
	}
	if ok, reason := s.CanResume(job); !ok {
		return job, errors.New(reason)
	}

	job.Status = "queued"
	job.LastError = ""
	job.ModifiedAt = time.Now().UTC()
	if err := s.store.Save(job); err != nil {
		return job, err
	}

	reqURL := job.URL
	reqHeaders := job.RequestHeaders
	pageURL := ""
	if len(job.OriginalHeaders) > 0 {
		reqHeaders = job.OriginalHeaders
	}
	if strings.TrimSpace(job.OriginalPageURL) != "" {
		pageURL = job.OriginalPageURL
	}
	if strings.TrimSpace(job.OriginalURL) != "" {
		reqURL = job.OriginalURL
	}
	go s.startQueuedJob(ctx, api.EnqueueRequest{URL: reqURL, Filename: job.Filename, Options: api.DownloadOptions{MaxConnections: job.MaxConnections, ChunkSizeBytes: job.ChunkSizeBytes, RetryCount: job.RetryCount, YouTubeQuality: job.YouTubeQuality}, Context: api.RequestContext{Headers: reqHeaders, PageURL: pageURL}}, job)

	return job, nil
}

func (s *Service) CancelJob(jobID string) (jobstate.Job, error) {
	job, err := s.store.Get(jobID)
	if err != nil {
		return jobstate.Job{}, err
	}

	s.mu.Lock()
	cancel, ok := s.cancels[jobID]
	s.mu.Unlock()

	if !ok {
		return job, errors.New("job is not currently running")
	}

	cancel()
	job.Status = "failed"
	job.LastError = "Canceled by user"
	job.ModifiedAt = time.Now().UTC()
	if err := s.store.Save(job); err != nil {
		return job, err
	}
	return job, nil
}

func (s *Service) RemoveJob(jobID string) error {
	job, err := s.store.Get(jobID)
	if err != nil {
		return err
	}

	s.mu.Lock()
	cancel, ok := s.cancels[jobID]
	s.mu.Unlock()
	if ok {
		cancel()
	}

	if strings.TrimSpace(job.OutputPath) != "" {
		if err := os.Remove(job.OutputPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if strings.TrimSpace(job.ChunksDir) != "" {
		if err := os.RemoveAll(job.ChunksDir); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	jobFile := filepath.Join(s.cfg.JobsDir, fmt.Sprintf("%s.json", jobID))
	if err := os.Remove(jobFile); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (s *Service) ResumeIncomplete(ctx context.Context) error {
	jobs, err := s.store.List()
	if err != nil {
		return err
	}
	for i := range jobs {
		job := jobs[i]
		if job.Status == "completed" {
			continue
		}
		reqURL := job.URL
		reqHeaders := job.RequestHeaders
		pageURL := ""
		if len(job.OriginalHeaders) > 0 {
			reqHeaders = job.OriginalHeaders
		}
		if strings.TrimSpace(job.OriginalPageURL) != "" {
			pageURL = job.OriginalPageURL
		}
		if strings.TrimSpace(job.OriginalURL) != "" {
			reqURL = job.OriginalURL
		}
		go s.startQueuedJob(ctx, api.EnqueueRequest{URL: reqURL, Filename: job.Filename, Options: api.DownloadOptions{MaxConnections: job.MaxConnections, ChunkSizeBytes: job.ChunkSizeBytes, RetryCount: job.RetryCount, YouTubeQuality: job.YouTubeQuality}, Context: api.RequestContext{Headers: reqHeaders, PageURL: pageURL}}, job)
	}
	return nil
}

func (s *Service) probeURL(ctx context.Context, rawURL, requestedFilename string, headers map[string]string) (probeResult, error) {
	result := probeResult{}

	extractorURL := preferredExtractorURL(rawURL, headers)
	if shouldUseExtractor(extractorURL) {
		extractorProbe, err := s.probeWithExtractor(ctx, extractorURL, requestedFilename, headers)
		if err != nil {
			return probeResult{}, err
		}
		return extractorProbe, nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodHead, rawURL, nil)
	if err != nil {
		return result, err
	}
	applyHeaders(req, headers)

	resp, err := s.httpClient.Do(req)
	if err != nil || resp.StatusCode >= 400 || resp.StatusCode == http.StatusMethodNotAllowed {
		if resp != nil && resp.Body != nil {
			resp.Body.Close()
		}
		return s.probeWithRangeGet(ctx, rawURL, requestedFilename, headers)
	}
	defer resp.Body.Close()

	return buildProbeResult(resp, rawURL, requestedFilename), nil
}

func (s *Service) probeWithRangeGet(ctx context.Context, rawURL, requestedFilename string, headers map[string]string) (probeResult, error) {
	result := probeResult{}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return result, err
	}
	applyHeaders(req, headers)
	req.Header.Set("Range", "bytes=0-0")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return result, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return result, fmt.Errorf("probe request failed with status %s", resp.Status)
	}

	probe := buildProbeResult(resp, rawURL, requestedFilename)
	if contentRange := resp.Header.Get("Content-Range"); contentRange != "" {
		if total, ok := parseTotalFromContentRange(contentRange); ok {
			probe.TotalBytes = total
			probe.AcceptRanges = true
		}
	}
	return probe, nil
}

func buildProbeResult(resp *http.Response, rawURL, requestedFilename string) probeResult {
	finalURL := rawURL
	if resp.Request != nil && resp.Request.URL != nil {
		finalURL = resp.Request.URL.String()
	}

	contentDisposition := resp.Header.Get("Content-Disposition")
	filename := chooseFilename(requestedFilename, contentDisposition, finalURL)

	return probeResult{
		FinalURL:           finalURL,
		Filename:           filename,
		TotalBytes:         parseContentLength(resp.Header.Get("Content-Length")),
		AcceptRanges:       strings.Contains(strings.ToLower(resp.Header.Get("Accept-Ranges")), "bytes") || resp.StatusCode == http.StatusPartialContent,
		ETag:               resp.Header.Get("ETag"),
		ContentType:        resp.Header.Get("Content-Type"),
		ContentDisposition: contentDisposition,
	}
}

func (s *Service) probeWithExtractor(ctx context.Context, rawURL, requestedFilename string, headers map[string]string) (probeResult, error) {
	if !shouldUseExtractor(rawURL) {
		return probeResult{}, errors.New("extractor not needed")
	}

	payload, err := runYTDLP(ctx, rawURL, headers)
	if err != nil {
		return probeResult{}, err
	}

	finalURL := strings.TrimSpace(payload.URL)
	if finalURL == "" {
		return probeResult{}, errors.New("yt-dlp did not return a media URL")
	}

	filename := chooseFilename(guessExtractorFilename(payload), "", finalURL)
	if strings.TrimSpace(requestedFilename) != "" {
		filename = chooseFilename(requestedFilename, "", finalURL)
	}

	mergedHeaders := sanitizeHeaders(payload.HTTPHeaders)
	if mergedHeaders == nil {
		mergedHeaders = make(map[string]string)
	}
	if ua := strings.TrimSpace(headers["User-Agent"]); ua != "" {
		mergedHeaders["User-Agent"] = ua
	}
	if ua := strings.TrimSpace(headers["user-agent"]); ua != "" {
		mergedHeaders["User-Agent"] = ua
	}
	if payload.WebpageURL != "" {
		mergedHeaders["Referer"] = payload.WebpageURL
	}
	if strings.TrimSpace(mergedHeaders["Accept"]) == "" {
		mergedHeaders["Accept"] = "*/*"
	}

	return probeResult{
		FinalURL:       finalURL,
		Filename:       filename,
		AcceptRanges:   true,
		ContentType:    guessContentTypeFromExtractor(payload),
		Extractor:      "yt-dlp",
		RequestHeaders: mergedHeaders,
	}, nil
}

func shouldUseExtractor(rawURL string) bool {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	host := strings.ToLower(parsed.Hostname())
	return host == "youtu.be" || host == "youtube.com" || strings.HasSuffix(host, ".youtube.com")
}

func preferredExtractorURL(rawURL string, headers map[string]string) string {
	referer := strings.TrimSpace(headers["Referer"])
	if referer == "" {
		referer = strings.TrimSpace(headers["referer"])
	}
	if referer == "" {
		return rawURL
	}

	parsedRaw, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	if !strings.HasSuffix(strings.ToLower(parsedRaw.Hostname()), ".googlevideo.com") {
		return rawURL
	}
	if shouldUseExtractor(referer) {
		return referer
	}
	return rawURL
}

func runYTDLP(ctx context.Context, rawURL string, headers map[string]string) (ytDLPProbe, error) {
	payload := ytDLPProbe{}
	baseArgs := []string{"-J", "--no-playlist", "--js-runtimes", "node", "--remote-components", "ejs:github", "-f", "best[protocol!=mhtml]/best"}
	baseWithCookies := append([]string{}, baseArgs...)
	baseWithCookies = append(baseWithCookies, "--cookies-from-browser", "firefox")
	debugLog("yt-dlp probe rawURL=%s incomingHeaderKeys=%v", rawURL, headerKeys(headers))
	for key, value := range sanitizeHeaders(headers) {
		switch {
		case strings.EqualFold(key, "User-Agent"):
			baseArgs = append(baseArgs, "--user-agent", value)
			baseWithCookies = append(baseWithCookies, "--user-agent", value)
		case strings.EqualFold(key, "Referer"):
			baseArgs = append(baseArgs, "--referer", value)
			baseWithCookies = append(baseWithCookies, "--referer", value)
		case strings.EqualFold(key, "Cookie"):
			continue
		case strings.EqualFold(key, "Origin"):
			continue
		case len(value) > 2048:
			continue
		default:
			baseArgs = append(baseArgs, "--add-header", fmt.Sprintf("%s:%s", key, value))
			baseWithCookies = append(baseWithCookies, "--add-header", fmt.Sprintf("%s:%s", key, value))
		}
	}

	attempts := [][]string{
		append(append([]string{}, baseWithCookies...), rawURL),
		append(append([]string{}, baseWithCookies...), "--extractor-args", "youtube:player_client=web", rawURL),
		append(append([]string{}, baseWithCookies...), "--extractor-args", "youtube:player_client=tv_embedded", rawURL),
		append(append([]string{}, baseArgs...), "--extractor-args", "youtube:player_client=android", rawURL),
	}

	var lastErr string
	for index, args := range attempts {
		cmd := exec.CommandContext(ctx, "yt-dlp", args...)
		var stdout bytes.Buffer
		var stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			message := strings.TrimSpace(stderr.String())
			if message == "" {
				message = err.Error()
			}
			if errors.Is(err, exec.ErrNotFound) {
				return payload, errors.New("YouTube download support requires yt-dlp, but it is not installed")
			}
			lastErr = message
			debugLog("yt-dlp probe failed rawURL=%s attempt=%d error=%s", rawURL, index+1, message)
			continue
		}
		if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
			lastErr = fmt.Sprintf("failed to decode yt-dlp output: %v", err)
			debugLog("yt-dlp probe decode failed rawURL=%s attempt=%d error=%v", rawURL, index+1, err)
			continue
		}
		debugLog("yt-dlp probe success rawURL=%s attempt=%d webpageURL=%s mediaURL=%s httpHeaderKeys=%v", rawURL, index+1, payload.WebpageURL, payload.URL, headerKeys(payload.HTTPHeaders))
		return payload, nil
	}

	if lastErr == "" {
		lastErr = "unknown yt-dlp probe failure"
	}
	return payload, fmt.Errorf("Could not extract a downloadable stream from this YouTube page: %s", lastErr)
}

func guessExtractorFilename(payload ytDLPProbe) string {
	title := sanitizeFilename(strings.TrimSpace(payload.Title))
	ext := strings.TrimSpace(payload.Ext)
	if title == "" {
		title = fmt.Sprintf("video-%d", time.Now().UTC().Unix())
	}
	if ext != "" && !strings.HasSuffix(strings.ToLower(title), "."+strings.ToLower(ext)) {
		return title + "." + ext
	}
	return title
}

func guessContentTypeFromExtractor(payload ytDLPProbe) string {
	switch strings.ToLower(strings.TrimSpace(payload.Ext)) {
	case "mp4", "m4v":
		return "video/mp4"
	case "webm":
		return "video/webm"
	case "mp3":
		return "audio/mpeg"
	case "m4a":
		return "audio/mp4"
	default:
		return ""
	}
}

func (s *Service) downloadChunkWithRetry(ctx context.Context, job jobstate.Job, chunkIndex int) (int64, error) {
	var lastErr error
	attempts := max(1, job.RetryCount+1)
	for attempt := 1; attempt <= attempts; attempt++ {
		bytesSaved, err := s.downloadChunk(ctx, job, chunkIndex)
		if err == nil {
			return bytesSaved, nil
		}
		lastErr = err
	}
	return 0, lastErr
}

func (s *Service) downloadChunk(ctx context.Context, job jobstate.Job, chunkIndex int) (int64, error) {
	chunk := job.Chunks[chunkIndex]
	chunkPath := filepath.Join(job.ChunksDir, fmt.Sprintf("chunk-%06d.part", chunk.Index))
	expected := chunkLength(chunk)

	existingSize, err := fileSize(chunkPath)
	if err != nil {
		return 0, err
	}
	if expected >= 0 && existingSize > expected {
		if err := os.Remove(chunkPath); err != nil {
			return 0, err
		}
		existingSize = 0
	}
	if expected >= 0 && existingSize == expected {
		return existingSize, nil
	}
	if existingSize > 0 && !job.AcceptRanges {
		if err := os.Remove(chunkPath); err != nil {
			return 0, err
		}
		existingSize = 0
	}

	file, err := os.OpenFile(chunkPath, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return 0, err
	}
	defer file.Close()
	if _, err := file.Seek(existingSize, io.SeekStart); err != nil {
		return 0, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, job.URL, nil)
	if err != nil {
		return 0, err
	}
	applyHeaders(req, job.RequestHeaders)
	debugLog("downloadChunk job=%s chunk=%d url=%s headerKeys=%v range=%s", job.JobID, chunkIndex, job.URL, headerKeys(job.RequestHeaders), req.Header.Get("Range"))

	start := chunk.Start + existingSize
	end := chunk.End
	if job.AcceptRanges && end >= 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))
	}

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return existingSize, err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		debugLog("downloadChunk failure job=%s chunk=%d status=%s responseHeaders=%v", job.JobID, chunkIndex, resp.Status, headerKeys(mapFromHeader(resp.Header)))
		return existingSize, fmt.Errorf("download failed with status %s", resp.Status)
	}
	if job.AcceptRanges && end >= 0 && resp.StatusCode != http.StatusPartialContent {
		return existingSize, fmt.Errorf("server ignored range request with status %s", resp.Status)
	}
	if !job.AcceptRanges && existingSize > 0 {
		return existingSize, errors.New("cannot resume partial download without range support")
	}

	writtenNow, err := io.Copy(file, resp.Body)
	currentTotal := existingSize + writtenNow
	if err != nil {
		return currentTotal, err
	}
	if expected >= 0 && currentTotal != expected {
		return currentTotal, fmt.Errorf("incomplete chunk write: expected %d bytes, got %d", expected, currentTotal)
	}

	return currentTotal, nil
}

func (s *Service) mergeChunks(job *jobstate.Job) error {
	if err := ensureDir(filepath.Dir(job.OutputPath)); err != nil {
		return err
	}

	out, err := os.Create(job.OutputPath)
	if err != nil {
		return err
	}
	defer out.Close()

	for _, chunk := range job.Chunks {
		chunkPath := filepath.Join(job.ChunksDir, fmt.Sprintf("chunk-%06d.part", chunk.Index))
		in, err := os.Open(chunkPath)
		if err != nil {
			return err
		}

		if _, err := io.Copy(out, in); err != nil {
			in.Close()
			return err
		}
		if err := in.Close(); err != nil {
			return err
		}
	}

	return nil
}

func (s *Service) downloadStream(ctx context.Context, job *jobstate.Job) error {
	job.Status = "downloading"
	job.ModifiedAt = time.Now().UTC()
	job.LastError = ""
	if err := s.store.Save(*job); err != nil {
		return err
	}

	if err := ensureDir(filepath.Dir(job.OutputPath)); err != nil {
		return err
	}

	args := []string{"-y"}
	for key, value := range sanitizeHeaders(job.RequestHeaders) {
		args = append(args, "-headers", fmt.Sprintf("%s: %s\r\n", key, value))
	}
	args = append(args, "-i", job.URL, "-c", "copy", job.OutputPath)

	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	devNull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer devNull.Close()
	cmd.Stdout = devNull
	cmd.Stderr = devNull
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ffmpeg failed: %w", err)
	}

	job.Status = "merging"
	job.ModifiedAt = time.Now().UTC()
	job.LastError = ""
	_ = s.store.Save(*job)

	if updated, _ := s.refreshLiveJobState(*job); true {
		job.DownloadedBytes = updated.DownloadedBytes
		job.TotalBytes = updated.TotalBytes
	}
	job.Status = "completed"
	job.ModifiedAt = time.Now().UTC()
	job.LastError = ""
	return s.store.Save(*job)
}

func (s *Service) downloadWithYTDLP(ctx context.Context, job *jobstate.Job) error {
	job.Status = "extracting"
	job.ModifiedAt = time.Now().UTC()
	job.LastError = ""
	if err := s.store.Save(*job); err != nil {
		return err
	}
	if err := ensureDir(filepath.Dir(job.OutputPath)); err != nil {
		return err
	}

	sourceURL := strings.TrimSpace(job.ExtractorSourceURL)
	if sourceURL == "" {
		sourceURL = strings.TrimSpace(job.RequestHeaders["Referer"])
	}
	if sourceURL == "" {
		sourceURL = job.URL
	}

	baseArgs := []string{"--newline", "--js-runtimes", "node", "--remote-components", "ejs:github", "--downloader", "ffmpeg", "--http-chunk-size", "10M", "--no-part", "--retries", "10", "-o", job.OutputPath}
	baseWithCookies := append([]string{}, baseArgs...)
	baseWithCookies = append(baseWithCookies, "--cookies-from-browser", "firefox")
	preferredFormat := youtubeFormatSelector(job.YouTubeQuality)
	for key, value := range sanitizeHeaders(job.RequestHeaders) {
		switch {
		case strings.EqualFold(key, "User-Agent"):
			baseArgs = append(baseArgs, "--user-agent", value)
			baseWithCookies = append(baseWithCookies, "--user-agent", value)
		case strings.EqualFold(key, "Referer"):
			baseArgs = append(baseArgs, "--referer", value)
			baseWithCookies = append(baseWithCookies, "--referer", value)
		case strings.EqualFold(key, "Cookie"):
			continue
		case strings.EqualFold(key, "Origin"):
			continue
		case len(value) > 2048:
			continue
		default:
			baseArgs = append(baseArgs, "--add-header", fmt.Sprintf("%s:%s", key, value))
			baseWithCookies = append(baseWithCookies, "--add-header", fmt.Sprintf("%s:%s", key, value))
		}
	}

	playlistArgs := []string{"--no-playlist"}
	if shouldUseExtractor(sourceURL) {
		playlistArgs = append(playlistArgs, "--playlist-items", "1")
	}

	attempts := [][]string{
		append(append(append([]string{}, baseWithCookies...), playlistArgs...), "-f", preferredFormat, "--merge-output-format", "mp4", sourceURL),
		append(append(append([]string{}, baseWithCookies...), playlistArgs...), "--extractor-args", "youtube:player_client=web", "-f", preferredFormat, "--merge-output-format", "mp4", sourceURL),
		append(append(append([]string{}, baseWithCookies...), playlistArgs...), "--extractor-args", "youtube:player_client=tv_embedded", "-f", preferredFormat, "--merge-output-format", "mp4", sourceURL),
		append(append(append([]string{}, baseArgs...), playlistArgs...), "--extractor-args", "youtube:player_client=android", "-f", preferredFormat, "--merge-output-format", "mp4", sourceURL),
		append(append(append([]string{}, baseWithCookies...), playlistArgs...), "-f", qualityCappedProgressiveSelector(job.YouTubeQuality), sourceURL),
		append(append(append([]string{}, baseArgs...), playlistArgs...), "--extractor-args", "youtube:player_client=android", "-f", qualityCappedProgressiveSelector(job.YouTubeQuality), sourceURL),
		append(append(append([]string{}, baseWithCookies...), playlistArgs...), "-f", "best", sourceURL),
		append(append(append([]string{}, baseArgs...), playlistArgs...), "-f", "best", sourceURL),
	}

	var lastErr string
	for index, args := range attempts {
		job.Status = "downloading"
		job.ModifiedAt = time.Now().UTC()
		job.LastError = ""
		_ = s.store.Save(*job)
		debugLog("yt-dlp download job=%s attempt=%d sourceURL=%s output=%s args=%v", job.JobID, index+1, sourceURL, job.OutputPath, args)
		_ = os.Remove(job.OutputPath)
		cmd := exec.CommandContext(ctx, "yt-dlp", args...)
		stdoutPipe, err := cmd.StdoutPipe()
		if err != nil {
			return err
		}
		stderrPipe, err := cmd.StderrPipe()
		if err != nil {
			return err
		}
		var stderr bytes.Buffer
		if err := cmd.Start(); err != nil {
			return err
		}
		progressDone := make(chan struct{})
		go func() {
			defer close(progressDone)
			lines := make(chan string, 64)
			var wg sync.WaitGroup
			consume := func(r io.Reader) {
				defer wg.Done()
				reader := bufio.NewReader(r)
				for {
					line, readErr := reader.ReadString('\n')
					if line != "" {
						lines <- line
					}
					if readErr != nil {
						return
					}
				}
			}
			wg.Add(2)
			go consume(stdoutPipe)
			go consume(stderrPipe)
			go func() {
				wg.Wait()
				close(lines)
			}()
			for line := range lines {
				stderr.WriteString(line)
				debugLog("yt-dlp progress job=%s line=%q", job.JobID, strings.TrimSpace(line))
				s.updateYTDLPProgress(job, line)
			}
		}()
		err = cmd.Wait()
		<-progressDone
		if err == nil {
			job.ProgressPercent = 100
			job.ProgressText = "100%"
			job.SpeedText = ""
			job.ETAText = ""
			if info, statErr := os.Stat(job.OutputPath); statErr == nil {
				job.DownloadedBytes = info.Size()
				job.TotalBytes = info.Size()
			}
			job.Status = "completed"
			job.ModifiedAt = time.Now().UTC()
			job.LastError = ""
			return s.store.Save(*job)
		}
		job.ProgressText = ""
		job.SpeedText = ""
		job.ETAText = ""
		job.ProgressPercent = 0
		_ = s.store.Save(*job)
		lastErr = strings.TrimSpace(stderr.String())
		if lastErr == "" {
			lastErr = "yt-dlp execution failed"
		}
		debugLog("yt-dlp download failed job=%s attempt=%d error=%s", job.JobID, index+1, lastErr)
	}

	return fmt.Errorf("yt-dlp download failed: %s", lastErr)
}

func (s *Service) refreshLiveJobState(job jobstate.Job) (jobstate.Job, bool) {
	changed := false
	if job.Status == "downloading" && strings.TrimSpace(job.OutputPath) != "" {
		if info, err := os.Stat(job.OutputPath); err == nil {
			if info.Size() != job.DownloadedBytes {
				job.DownloadedBytes = info.Size()
				if job.Extractor != "yt-dlp" {
					job.TotalBytes = max64(job.TotalBytes, info.Size())
				}
				job.ModifiedAt = time.Now().UTC()
				changed = true
			}
		}
	}
	return job, changed
}

func (s *Service) updateYTDLPProgress(job *jobstate.Job, line string) {
	line = strings.TrimSpace(line)
	if line == "" {
		return
	}
	match := ytDLPProgressRE.FindStringSubmatch(line)
	if len(match) == 0 {
		ffmpegMatch := regexp.MustCompile(`time=([0-9:.]+)`).FindStringSubmatch(line)
		if len(ffmpegMatch) > 1 {
			job.ProgressText = ffmpegMatch[1]
			job.ModifiedAt = time.Now().UTC()
			_ = s.store.Save(*job)
		}
		return
	}
	percent, err := strconv.ParseFloat(match[1], 64)
	if err != nil {
		return
	}
	job.ProgressPercent = percent
	job.ProgressText = strings.TrimSpace(match[1]) + "%"
	if totalBytes, ok := parseHumanBytes(match[2]); ok {
		job.TotalBytes = totalBytes
		job.DownloadedBytes = int64((percent / 100) * float64(totalBytes))
	}
	if len(match) > 3 {
		job.SpeedText = strings.TrimSpace(match[3])
	}
	if len(match) > 4 {
		job.ETAText = strings.TrimSpace(match[4])
	}
	job.ModifiedAt = time.Now().UTC()
	_ = s.store.Save(*job)
}

func parseHumanBytes(value string) (int64, bool) {
	v := strings.TrimSpace(strings.ReplaceAll(value, "~", ""))
	if v == "" {
		return 0, false
	}
	re := regexp.MustCompile(`^([0-9]+(?:\.[0-9]+)?)([KMGTP]?i?B)$`)
	m := re.FindStringSubmatch(v)
	if len(m) != 3 {
		return 0, false
	}
	n, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0, false
	}
	mul := float64(1)
	switch m[2] {
	case "KB":
		mul = 1000
	case "MB":
		mul = 1000 * 1000
	case "GB":
		mul = 1000 * 1000 * 1000
	case "TB":
		mul = 1000 * 1000 * 1000 * 1000
	case "KiB":
		mul = 1024
	case "MiB":
		mul = 1024 * 1024
	case "GiB":
		mul = 1024 * 1024 * 1024
	case "TiB":
		mul = 1024 * 1024 * 1024 * 1024
	case "B":
		mul = 1
	default:
		return 0, false
	}
	return int64(n * mul), true
}

func (s *Service) syncJobWithDisk(job *jobstate.Job) error {
	for i := range job.Chunks {
		chunkPath := filepath.Join(job.ChunksDir, fmt.Sprintf("chunk-%06d.part", job.Chunks[i].Index))
		size, err := fileSize(chunkPath)
		if err != nil {
			return err
		}
		expected := chunkLength(job.Chunks[i])
		if expected >= 0 && size > expected {
			if err := os.Remove(chunkPath); err != nil {
				return err
			}
			size = 0
		}
		job.Chunks[i].BytesSaved = size
		job.Chunks[i].Completed = expected >= 0 && size == expected && expected > 0
	}
	job.DownloadedBytes = sumDownloadedBytes(job.Chunks)
	job.ModifiedAt = time.Now().UTC()
	return s.store.Save(*job)
}

func normalizeOptions(opts api.DownloadOptions) api.DownloadOptions {
	if opts.MaxConnections <= 0 {
		opts.MaxConnections = 8
	}
	if opts.ChunkSizeBytes <= 0 {
		opts.ChunkSizeBytes = 8 * 1024 * 1024
	}
	if opts.RetryCount < 0 {
		opts.RetryCount = 0
	}
	switch strings.ToLower(strings.TrimSpace(opts.YouTubeQuality)) {
	case "highest", "best", "1080p", "720p", "480p", "360p":
		opts.YouTubeQuality = strings.ToLower(strings.TrimSpace(opts.YouTubeQuality))
	default:
		opts.YouTubeQuality = "highest"
	}
	return opts
}

func initialStatusForProbe(probe probeResult) string {
	if probe.Extractor == "yt-dlp" {
		return "extracting"
	}
	if isStreamManifestURL(probe.FinalURL, probe.ContentType) {
		return "planned"
	}
	if probe.TotalBytes > 0 {
		return "planned"
	}
	return "queued"
}

func planChunks(totalBytes, chunkSizeBytes int64, maxConnections int, acceptRanges bool) []jobstate.ChunkState {
	if totalBytes <= 0 {
		return []jobstate.ChunkState{{Index: 0, Start: 0, End: -1, Completed: false, BytesSaved: 0}}
	}
	if !acceptRanges || totalBytes <= chunkSizeBytes {
		return []jobstate.ChunkState{{Index: 0, Start: 0, End: totalBytes - 1, Completed: false, BytesSaved: 0}}
	}
	if maxConnections <= 0 {
		maxConnections = 1
	}

	planned := make([]jobstate.ChunkState, 0)
	var start int64
	index := 0
	for start < totalBytes {
		end := start + chunkSizeBytes - 1
		if end >= totalBytes {
			end = totalBytes - 1
		}
		planned = append(planned, jobstate.ChunkState{Index: index, Start: start, End: end, Completed: false, BytesSaved: 0})
		start = end + 1
		index++
	}

	return planned
}

func newJobID() (string, error) {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

func chooseFilename(requested, contentDisposition, rawURL string) string {
	if strings.TrimSpace(requested) != "" {
		return sanitizeFilename(requested)
	}
	if _, params, err := mime.ParseMediaType(contentDisposition); err == nil {
		if name := strings.TrimSpace(params["filename"]); name != "" {
			return sanitizeFilename(name)
		}
		if name := strings.TrimSpace(params["filename*"]); name != "" {
			return sanitizeFilename(strings.TrimPrefix(name, "UTF-8''"))
		}
	}
	return normalizeFilename(rawURL, "")
}

func normalizeFilename(rawURL, requested string) string {
	if strings.TrimSpace(requested) != "" {
		return sanitizeFilename(requested)
	}

	parsed, err := url.Parse(rawURL)
	if err == nil {
		base := filepath.Base(parsed.Path)
		if base != "." && base != "/" && base != "" {
			return sanitizeFilename(base)
		}
	}

	return fmt.Sprintf("download-%d.bin", time.Now().UTC().Unix())
}

func trimForLog(value string, maxLen int) string {
	value = strings.TrimSpace(value)
	if maxLen <= 0 || len(value) <= maxLen {
		return value
	}
	return value[:maxLen] + "..."
}

func youtubeFormatSelector(quality string) string {
	switch strings.ToLower(strings.TrimSpace(quality)) {
	case "360p":
		return "bestvideo[height<=360][vcodec!=none]+bestaudio[acodec!=none]/best[height<=360]"
	case "480p":
		return "bestvideo[height<=480][vcodec!=none]+bestaudio[acodec!=none]/best[height<=480]"
	case "720p":
		return "bestvideo[height<=720][vcodec!=none]+bestaudio[acodec!=none]/best[height<=720]"
	case "1080p":
		return "bestvideo[height<=1080][vcodec!=none]+bestaudio[acodec!=none]/best[height<=1080]"
	case "best", "highest", "":
		return "bestvideo[vcodec!=none]+bestaudio[acodec!=none]/best"
	default:
		return "bestvideo[vcodec!=none]+bestaudio[acodec!=none]/best"
	}
}

func qualityCappedProgressiveSelector(quality string) string {
	switch strings.ToLower(strings.TrimSpace(quality)) {
	case "360p":
		return "best[height<=360][ext=mp4]/best[height<=360]/best"
	case "480p":
		return "best[height<=480][ext=mp4]/best[height<=480]/best"
	case "720p":
		return "best[height<=720][ext=mp4]/best[height<=720]/best"
	case "1080p":
		return "best[height<=1080][ext=mp4]/best[height<=1080]/best"
	case "best", "highest", "":
		return "best[ext=mp4]/best"
	default:
		return "best[ext=mp4]/best"
	}
}

func detectStreamKind(rawURL, contentType string) string {
	lowerURL := strings.ToLower(rawURL)
	lowerType := strings.ToLower(contentType)
	if strings.Contains(lowerURL, ".m3u8") || strings.Contains(lowerType, "application/vnd.apple.mpegurl") || strings.Contains(lowerType, "application/x-mpegurl") {
		return "hls"
	}
	if strings.Contains(lowerURL, ".mpd") || strings.Contains(lowerType, "dash+xml") {
		return "dash"
	}
	return ""
}

func isStreamManifestURL(rawURL, contentType string) bool {
	return detectStreamKind(rawURL, contentType) != ""
}

func ensureStreamFriendlyFilename(rawURL, filename, contentType string) string {
	name := sanitizeFilename(filename)
	if !isStreamManifestURL(rawURL, contentType) {
		return name
	}
	lower := strings.ToLower(name)
	if strings.HasSuffix(lower, ".m3u8") || strings.HasSuffix(lower, ".mpd") || !strings.Contains(filepath.Base(lower), ".") {
		base := strings.TrimSuffix(strings.TrimSuffix(name, filepath.Ext(name)), ".")
		if strings.TrimSpace(base) == "" {
			base = fmt.Sprintf("video-%d", time.Now().UTC().Unix())
		}
		return sanitizeFilename(base + ".mp4")
	}
	return name
}

func sanitizeFilename(name string) string {
	name = strings.TrimSpace(name)
	name = strings.ReplaceAll(name, "/", "_")
	name = strings.ReplaceAll(name, "\\", "_")
	if name == "" || name == "." || name == ".." {
		return fmt.Sprintf("download-%d.bin", time.Now().UTC().Unix())
	}
	return name
}

func parseContentLength(raw string) int64 {
	if strings.TrimSpace(raw) == "" {
		return 0
	}
	size, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || size < 0 {
		return 0
	}
	return size
}

func parseTotalFromContentRange(raw string) (int64, bool) {
	slash := strings.LastIndex(raw, "/")
	if slash == -1 || slash == len(raw)-1 {
		return 0, false
	}
	total, err := strconv.ParseInt(strings.TrimSpace(raw[slash+1:]), 10, 64)
	if err != nil || total <= 0 {
		return 0, false
	}
	return total, true
}

func ensureDir(path string) error {
	return os.MkdirAll(path, 0o755)
}

func sumDownloadedBytes(chunks []jobstate.ChunkState) int64 {
	var total int64
	for _, chunk := range chunks {
		total += chunk.BytesSaved
	}
	return total
}

func chunkLength(chunk jobstate.ChunkState) int64 {
	if chunk.End < chunk.Start {
		return -1
	}
	return chunk.End - chunk.Start + 1
}

func isChunkComplete(chunk jobstate.ChunkState) bool {
	length := chunkLength(chunk)
	return length >= 0 && chunk.BytesSaved == length && length > 0
}

func allChunksCompleted(chunks []jobstate.ChunkState) bool {
	if len(chunks) == 0 {
		return false
	}
	for _, chunk := range chunks {
		if !isChunkComplete(chunk) {
			return false
		}
	}
	return true
}

func fileSize(path string) (int64, error) {
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}
	return info.Size(), nil
}

func mapFromHeader(header http.Header) map[string]string {
	if len(header) == 0 {
		return nil
	}
	flat := make(map[string]string, len(header))
	for key, values := range header {
		flat[key] = strings.Join(values, ",")
	}
	return flat
}

func applyHeaders(req *http.Request, headers map[string]string) {
	appliedUA := false
	for key, value := range sanitizeHeaders(headers) {
		if value == "" || isBlockedRequestHeader(key) {
			continue
		}
		req.Header.Set(key, value)
		if strings.EqualFold(key, "User-Agent") {
			appliedUA = true
		}
	}
	if !appliedUA {
		req.Header.Set("User-Agent", defaultUserAgent)
	}
}

func sanitizeHeaders(headers map[string]string) map[string]string {
	if len(headers) == 0 {
		return nil
	}
	clean := make(map[string]string)
	for key, value := range headers {
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if key == "" || value == "" {
			continue
		}
		if strings.HasPrefix(key, "X-Robot-") {
			clean[key] = value
			continue
		}
		canonical := http.CanonicalHeaderKey(key)
		if isBlockedRequestHeader(canonical) {
			continue
		}
		clean[canonical] = value
	}
	return clean
}

func isBlockedRequestHeader(key string) bool {
	switch http.CanonicalHeaderKey(strings.TrimSpace(key)) {
	case "Connection", "Proxy-Connection", "Keep-Alive", "Transfer-Encoding", "Upgrade", "Host", "Content-Length":
		return true
	default:
		return false
	}
}

func enrichHeadersWithContext(downloadURL, pageURL string, headers map[string]string) map[string]string {
	merged := sanitizeHeaders(headers)
	if merged == nil {
		merged = make(map[string]string)
	}
	if playbackURL := strings.TrimSpace(merged["X-Robot-YouTube-Playback-URL"]); playbackURL != "" {
		debugLog("playback hint downloadURL=%s playbackURL=%s method=%s document=%s requestHeaders=%s responseHeaders=%s", downloadURL, playbackURL, merged["X-Robot-YouTube-Playback-Method"], merged["X-Robot-YouTube-Playback-Document"], trimForLog(merged["X-Robot-YouTube-Playback-Request-Headers"], 800), trimForLog(merged["X-Robot-YouTube-Playback-Response-Headers"], 800))
	}

	if pageURL != "" {
		if _, ok := merged["Referer"]; !ok {
			merged["Referer"] = pageURL
		}
		if _, ok := merged["Origin"]; !ok {
			if parsed, err := url.Parse(pageURL); err == nil && parsed.Scheme != "" && parsed.Host != "" {
				merged["Origin"] = parsed.Scheme + "://" + parsed.Host
			}
		}
	}

	if _, ok := merged["Accept"]; !ok {
		merged["Accept"] = "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8"
	}
	if _, ok := merged["Accept-Language"]; !ok {
		merged["Accept-Language"] = "en-US,en;q=0.9"
	}
	if _, ok := merged["Cache-Control"]; !ok {
		merged["Cache-Control"] = "no-cache"
	}
	if _, ok := merged["Pragma"]; !ok {
		merged["Pragma"] = "no-cache"
	}
	if _, ok := merged["Dnt"]; !ok {
		merged["DNT"] = "1"
	}
	if _, ok := merged["Connection"]; !ok {
		merged["Connection"] = "keep-alive"
	}

	_ = downloadURL
	return merged
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
