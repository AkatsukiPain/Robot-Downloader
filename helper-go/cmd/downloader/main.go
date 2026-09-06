package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"time"

	"robot-downloader/helper-go/internal/api"
	"robot-downloader/helper-go/internal/config"
	"robot-downloader/helper-go/internal/downloader"
)

const (
	uiListenAddr            = "127.0.0.1:38519"
	openAppPath             = "/app/"
	apiBasePath             = "/api"
	webAppVersion           = "20260823-1958"
	postDisconnectKeepAlive = 10 * time.Minute
)

type appServer struct {
	cfg     config.Config
	service *downloader.Service
	started time.Time
	baseURL string
	webRoot string
}

type appSettings struct {
	DownloadsDir      string `json:"downloadsDir"`
	HelperName        string `json:"helperName"`
	UIBaseURL         string `json:"uiBaseUrl"`
	NativeHostEnabled bool   `json:"nativeHostEnabled"`
}

type apiJob struct {
	JobID           string    `json:"jobId"`
	URL             string    `json:"url"`
	Filename        string    `json:"filename"`
	Status          string    `json:"status"`
	TotalBytes      int64     `json:"totalBytes,omitempty"`
	DownloadedBytes int64     `json:"downloadedBytes,omitempty"`
	AcceptRanges    bool      `json:"acceptRanges,omitempty"`
	CanResume       bool      `json:"canResume,omitempty"`
	ResumeReason    string    `json:"resumeReason,omitempty"`
	StreamKind      string    `json:"streamKind,omitempty"`
	CreatedAt       time.Time `json:"createdAt"`
	ModifiedAt      time.Time `json:"modifiedAt"`
	OutputPath      string    `json:"outputPath,omitempty"`
	LastError       string    `json:"lastError,omitempty"`
	Extractor       string    `json:"extractor,omitempty"`
	ProgressPercent float64   `json:"progressPercent,omitempty"`
	ProgressText    string    `json:"progressText,omitempty"`
	SpeedText       string    `json:"speedText,omitempty"`
	ETAText         string    `json:"etaText,omitempty"`
}

type jobsEnvelope struct {
	Jobs []apiJob `json:"jobs"`
}

type enqueueBody struct {
	URL            string               `json:"url"`
	Filename       string               `json:"filename"`
	YouTubeQuality string               `json:"youtubeQuality"`
	Context        api.RequestContext   `json:"context"`
	Options        *api.DownloadOptions `json:"options,omitempty"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "robot-downloader helper failed: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if err := cfg.Ensure(); err != nil {
		return err
	}

	service := downloader.NewService(cfg)
	_ = service.ResumeIncomplete(context.Background())

	webRoot, err := resolveWebRoot()
	if err != nil {
		return err
	}

	server := &appServer{
		cfg:     cfg,
		service: service,
		started: time.Now(),
		baseURL: "http://" + uiListenAddr,
		webRoot: webRoot,
	}

	httpServer := &http.Server{
		Addr:              uiListenAddr,
		Handler:           server.routes(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	listener, err := net.Listen("tcp", uiListenAddr)
	if err != nil {
		return err
	}
	go func() {
		if err := httpServer.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("robot-downloader UI server error: %v", err)
		}
	}()

	shutdownCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-shutdownCtx.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(ctx)
	}()

	nativeErrCh := make(chan error, 1)
	go func() {
		for {
			raw, err := readNativeMessage(os.Stdin)
			if err != nil {
				if err == io.EOF {
					nativeErrCh <- nil
					return
				}
				nativeErrCh <- err
				return
			}

			msgType, _ := raw["type"].(string)
			response := server.handleNativeMessage(msgType, raw)
			if err := writeNativeMessage(os.Stdout, response); err != nil {
				nativeErrCh <- err
				return
			}
		}
	}()

	select {
	case err := <-nativeErrCh:
		if err != nil {
			return err
		}
		log.Printf("robot-downloader native messaging disconnected; keeping local app alive for %s", postDisconnectKeepAlive)
		timer := time.NewTimer(postDisconnectKeepAlive)
		defer timer.Stop()
		select {
		case <-shutdownCtx.Done():
			return nil
		case <-timer.C:
			return nil
		}
	case <-shutdownCtx.Done():
		return nil
	}
}

func (s *appServer) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealthz)
	mux.Handle("/app/assets/", http.StripPrefix("/app/assets/", http.FileServer(http.Dir(s.webRoot))))
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/app", s.handleIndex)
	mux.HandleFunc(openAppPath, s.handleIndex)
	mux.HandleFunc(apiBasePath+"/health", s.handleAPIHealth)
	mux.HandleFunc(apiBasePath+"/settings", s.handleAPISettings)
	mux.HandleFunc(apiBasePath+"/jobs", s.handleAPIJobs)
	mux.HandleFunc(apiBasePath+"/jobs/", s.handleAPIJobAction)
	return withCORS(withLogging(mux))
}

func (s *appServer) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func (s *appServer) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" && r.URL.Path != "/app" && r.URL.Path != openAppPath {
		http.NotFound(w, r)
		return
	}
	http.ServeFile(w, r, filepath.Join(s.webRoot, "index.html"))
}

func resolveWebRoot() (string, error) {
	candidates := []string{}
	if cwd, err := os.Getwd(); err == nil {
		candidates = append(candidates, filepath.Join(cwd, "webapp"))
	}
	if exePath, err := os.Executable(); err == nil {
		exeDir := filepath.Dir(exePath)
		candidates = append(candidates,
			filepath.Join(exeDir, "webapp"),
			filepath.Join(exeDir, "..", "webapp"),
		)
	}
	for _, candidate := range candidates {
		indexPath := filepath.Join(candidate, "index.html")
		if info, err := os.Stat(indexPath); err == nil && !info.IsDir() {
			resolved, err := filepath.Abs(candidate)
			if err == nil {
				return resolved, nil
			}
			return candidate, nil
		}
	}
	return "", fmt.Errorf("webapp assets not found")
}

func (s *appServer) handleAPIHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":           true,
		"startedAt":    s.started.UTC(),
		"uiBaseUrl":    s.baseURL + openAppPath + "?v=" + webAppVersion,
		"downloadsDir": s.cfg.DownloadsDir,
	})
}

func (s *appServer) handleAPISettings(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	writeJSON(w, http.StatusOK, appSettings{
		DownloadsDir:      s.cfg.DownloadsDir,
		HelperName:        "robot.downloader",
		UIBaseURL:         s.baseURL + openAppPath + "?v=" + webAppVersion,
		NativeHostEnabled: true,
	})
}

func (s *appServer) handleAPIJobs(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		jobs, err := s.listAPIJobs()
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, jobsEnvelope{Jobs: jobs})
	case http.MethodPost:
		var body enqueueBody
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
			writeErrorMessage(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		if strings.TrimSpace(body.URL) == "" {
			writeErrorMessage(w, http.StatusBadRequest, "url is required")
			return
		}
		opts := api.DownloadOptions{MaxConnections: 8, ChunkSizeBytes: 8 * 1024 * 1024, RetryCount: 3, YouTubeQuality: body.YouTubeQuality}
		if body.Options != nil {
			opts = *body.Options
			if opts.YouTubeQuality == "" {
				opts.YouTubeQuality = body.YouTubeQuality
			}
		}
		job, err := s.service.Enqueue(r.Context(), api.EnqueueRequest{
			Type:     "enqueue",
			URL:      body.URL,
			Filename: body.Filename,
			Options:  opts,
			Context:  body.Context,
		})
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]any{
			"ok":      true,
			"jobId":   job.JobID,
			"status":  job.Status,
			"message": fmt.Sprintf("queued %s", job.Filename),
		})
	default:
		methodNotAllowed(w, http.MethodGet, http.MethodPost)
	}
}

func (s *appServer) handleAPIJobAction(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, apiBasePath+"/jobs/")
	path = strings.Trim(path, "/")
	if path == "" {
		http.NotFound(w, r)
		return
	}
	parts := strings.Split(path, "/")
	jobID := parts[0]
	if len(parts) == 1 {
		if r.Method != http.MethodGet {
			methodNotAllowed(w, http.MethodGet)
			return
		}
		jobs, err := s.listAPIJobs()
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		for _, job := range jobs {
			if job.JobID == jobID {
				writeJSON(w, http.StatusOK, job)
				return
			}
		}
		writeErrorMessage(w, http.StatusNotFound, "job not found")
		return
	}

	action := parts[1]
	switch action {
	case "resume":
		if r.Method != http.MethodPost {
			methodNotAllowed(w, http.MethodPost)
			return
		}
		job, err := s.service.ResumeJob(r.Context(), jobID)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "jobId": job.JobID, "status": job.Status})
	case "cancel":
		if r.Method != http.MethodPost {
			methodNotAllowed(w, http.MethodPost)
			return
		}
		job, err := s.service.CancelJob(jobID)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "jobId": job.JobID, "status": job.Status})
	case "remove":
		if r.Method != http.MethodDelete {
			methodNotAllowed(w, http.MethodDelete)
			return
		}
		if err := s.service.RemoveJob(jobID); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "jobId": jobID})
	case "open":
		if r.Method != http.MethodPost {
			methodNotAllowed(w, http.MethodPost)
			return
		}
		if err := s.service.OpenLocation(jobID); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "jobId": jobID})
	default:
		http.NotFound(w, r)
	}
}

func (s *appServer) listAPIJobs() ([]apiJob, error) {
	jobs, err := s.service.List()
	if err != nil {
		return nil, err
	}
	out := make([]apiJob, 0, len(jobs))
	for _, job := range jobs {
		canResume, reason := s.service.CanResume(job)
		displayURL := job.URL
		if strings.TrimSpace(job.ExtractorSourceURL) != "" {
			displayURL = job.ExtractorSourceURL
		}
		out = append(out, apiJob{
			JobID:           job.JobID,
			URL:             displayURL,
			Filename:        job.Filename,
			Status:          job.Status,
			TotalBytes:      job.TotalBytes,
			DownloadedBytes: job.DownloadedBytes,
			AcceptRanges:    job.AcceptRanges,
			CanResume:       canResume,
			ResumeReason:    reason,
			StreamKind:      job.StreamKind,
			CreatedAt:       job.CreatedAt,
			ModifiedAt:      job.ModifiedAt,
			OutputPath:      job.OutputPath,
			LastError:       job.LastError,
			Extractor:       job.Extractor,
			ProgressPercent: job.ProgressPercent,
			ProgressText:    job.ProgressText,
			SpeedText:       job.SpeedText,
			ETAText:         job.ETAText,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

func (s *appServer) handleNativeMessage(msgType string, raw map[string]any) any {
	switch msgType {
	case "enqueue":
		payloadBytes, err := json.Marshal(raw)
		if err != nil {
			return api.Response{OK: false, Type: "enqueue", Message: err.Error()}
		}
		var req api.EnqueueRequest
		if err := json.Unmarshal(payloadBytes, &req); err != nil {
			return api.Response{OK: false, Type: "enqueue", Message: err.Error()}
		}
		job, err := s.service.Enqueue(context.Background(), req)
		if err != nil {
			return api.Response{OK: false, Type: "enqueue", Message: err.Error()}
		}
		return api.Response{OK: true, Type: "enqueue", JobID: job.JobID, Status: job.Status, Message: fmt.Sprintf("queued %s", job.Filename)}
	case "list":
		jobs, err := s.listAPIJobs()
		if err != nil {
			return api.Response{OK: false, Type: "list", Message: err.Error()}
		}
		return jobs
	case "open-location", "resume-job", "cancel-job", "remove-job":
		payloadBytes, err := json.Marshal(raw)
		if err != nil {
			return api.Response{OK: false, Type: msgType, Message: err.Error()}
		}
		var req api.OpenLocationRequest
		if err := json.Unmarshal(payloadBytes, &req); err != nil {
			return api.Response{OK: false, Type: msgType, Message: err.Error()}
		}
		if strings.TrimSpace(req.JobID) == "" {
			return api.Response{OK: false, Type: msgType, Message: "jobId is required"}
		}
		switch msgType {
		case "open-location":
			if err := s.service.OpenLocation(req.JobID); err != nil {
				return api.Response{OK: false, Type: msgType, JobID: req.JobID, Message: err.Error()}
			}
			return api.Response{OK: true, Type: msgType, JobID: req.JobID, Message: "opened file location"}
		case "resume-job":
			job, err := s.service.ResumeJob(context.Background(), req.JobID)
			if err != nil {
				return api.Response{OK: false, Type: msgType, JobID: req.JobID, Message: err.Error()}
			}
			return api.Response{OK: true, Type: msgType, JobID: job.JobID, Status: job.Status, Message: fmt.Sprintf("resuming %s", job.Filename)}
		case "cancel-job":
			job, err := s.service.CancelJob(req.JobID)
			if err != nil {
				return api.Response{OK: false, Type: msgType, JobID: req.JobID, Message: err.Error()}
			}
			return api.Response{OK: true, Type: msgType, JobID: job.JobID, Status: job.Status, Message: fmt.Sprintf("canceled %s", job.Filename)}
		case "remove-job":
			if err := s.service.RemoveJob(req.JobID); err != nil {
				return api.Response{OK: false, Type: msgType, JobID: req.JobID, Message: err.Error()}
			}
			return api.Response{OK: true, Type: msgType, JobID: req.JobID, Message: "removed job and data"}
		}
	case "open-app":
		if err := openBrowser(s.baseURL + openAppPath + "?v=" + webAppVersion); err != nil {
			return api.Response{OK: false, Type: msgType, Message: err.Error()}
		}
		return api.Response{OK: true, Type: msgType, Message: "opened downloader app", Status: s.baseURL + openAppPath + "?v=" + webAppVersion}
	case "app-health":
		return map[string]any{"ok": true, "uiBaseUrl": s.baseURL + openAppPath + "?v=" + webAppVersion}
	default:
		return api.Response{OK: false, Type: msgType, Message: "unsupported message type"}
	}
	return api.Response{OK: false, Type: msgType, Message: "unsupported message type"}
}

func readNativeMessage(r io.Reader) (map[string]any, error) {
	var size uint32
	if err := binary.Read(r, binary.LittleEndian, &size); err != nil {
		return nil, err
	}
	payload := make([]byte, size)
	if _, err := io.ReadFull(r, payload); err != nil {
		return nil, err
	}
	var raw map[string]any
	if err := json.Unmarshal(payload, &raw); err != nil {
		return nil, err
	}
	return raw, nil
}

func writeNativeMessage(w io.Writer, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	size := uint32(len(body))
	if err := binary.Write(w, binary.LittleEndian, size); err != nil {
		return err
	}
	_, err = w.Write(body)
	return err
}

func methodNotAllowed(w http.ResponseWriter, allowed ...string) {
	w.Header().Set("Allow", strings.Join(allowed, ", "))
	writeErrorMessage(w, http.StatusMethodNotAllowed, "method not allowed")
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeErrorMessage(w, status, err.Error())
}

func writeErrorMessage(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{"ok": false, "error": message})
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		w.Header().Set("Access-Control-Allow-Methods", "GET,POST,DELETE,OPTIONS")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") || r.URL.Path == "/healthz" {
			log.Printf("%s %s", r.Method, r.URL.Path)
		}
		next.ServeHTTP(w, r)
	})
}

func openBrowser(target string) error {
	target = strings.TrimSpace(target)
	if target == "" {
		return errors.New("target is empty")
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "linux":
		cmd = exec.Command("xdg-open", target)
	case "darwin":
		cmd = exec.Command("open", target)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", target)
	default:
		return errors.New("unsupported platform for open-app")
	}
	return cmd.Start()
}
