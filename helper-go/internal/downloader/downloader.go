package downloader

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
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

type Service struct {
	cfg        config.Config
	store      *storage.JobStore
	httpClient *http.Client
	mu         sync.Mutex
	cancels    map[string]context.CancelFunc
}

type probeResult struct {
	FinalURL           string
	Filename           string
	TotalBytes         int64
	AcceptRanges       bool
	ETag               string
	ContentType        string
	ContentDisposition string
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
		Filename:        filename,
		Status:          "queued",
		CreatedAt:       now,
		ModifiedAt:      now,
		OutputPath:      filepath.Join(s.cfg.DownloadsDir, filename),
		ChunksDir:       chunksDir,
		MaxConnections:  options.MaxConnections,
		ChunkSizeBytes:  options.ChunkSizeBytes,
		RetryCount:      options.RetryCount,
		DownloadedBytes: 0,
		RequestHeaders:  sanitizeHeaders(mergedHeaders),
		Chunks:          []jobstate.ChunkState{{Index: 0, Start: 0, End: -1, Completed: false, BytesSaved: 0}},
	}

	if err := s.store.Save(job); err != nil {
		return jobstate.Job{}, err
	}

	go s.startQueuedJob(ctx, req, job)

	return job, nil
}

func (s *Service) Download(ctx context.Context, job *jobstate.Job) error {
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
	probe, err := s.probeURL(parentCtx, req.URL, req.Filename, mergedHeaders)
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
	job.ModifiedAt = time.Now().UTC()
	job.OutputPath = filepath.Join(s.cfg.DownloadsDir, job.Filename)
	job.TotalBytes = probe.TotalBytes
	job.AcceptRanges = probe.AcceptRanges
	job.ContentType = probe.ContentType
	job.ContentDisposition = probe.ContentDisposition
	job.ETag = probe.ETag
	job.RequestHeaders = sanitizeHeaders(mergedHeaders)
	job.StreamKind = detectStreamKind(probe.FinalURL, probe.ContentType)
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

	go s.startQueuedJob(ctx, api.EnqueueRequest{URL: job.URL, Filename: job.Filename, Options: api.DownloadOptions{MaxConnections: job.MaxConnections, ChunkSizeBytes: job.ChunkSizeBytes, RetryCount: job.RetryCount}, Context: api.RequestContext{Headers: job.RequestHeaders}}, job)

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
		if err := s.Download(ctx, &job); err != nil {
			continue
		}
	}
	return nil
}

func (s *Service) probeURL(ctx context.Context, rawURL, requestedFilename string, headers map[string]string) (probeResult, error) {
	result := probeResult{}

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
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ffmpeg failed: %w", err)
	}

	if updated, _ := s.refreshLiveJobState(*job); true {
		job.DownloadedBytes = updated.DownloadedBytes
		job.TotalBytes = updated.TotalBytes
	}
	job.Status = "completed"
	job.ModifiedAt = time.Now().UTC()
	job.LastError = ""
	return s.store.Save(*job)
}

func (s *Service) refreshLiveJobState(job jobstate.Job) (jobstate.Job, bool) {
	changed := false
	if job.Status == "downloading" && job.StreamKind != "" && strings.TrimSpace(job.OutputPath) != "" {
		if info, err := os.Stat(job.OutputPath); err == nil {
			if info.Size() != job.DownloadedBytes {
				job.DownloadedBytes = info.Size()
				job.TotalBytes = max64(job.TotalBytes, info.Size())
				job.ModifiedAt = time.Now().UTC()
				changed = true
			}
		}
	}
	return job, changed
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
	return opts
}

func initialStatusForProbe(probe probeResult) string {
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
