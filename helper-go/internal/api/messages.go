package api

import "time"

type EnqueueRequest struct {
	Type     string          `json:"type"`
	URL      string          `json:"url"`
	Filename string          `json:"filename,omitempty"`
	Options  DownloadOptions `json:"options"`
	Context  RequestContext  `json:"context,omitempty"`
}

type RequestContext struct {
	Headers map[string]string `json:"headers,omitempty"`
	PageURL string            `json:"pageUrl,omitempty"`
}

type DownloadOptions struct {
	MaxConnections int    `json:"maxConnections"`
	ChunkSizeBytes int64  `json:"chunkSizeBytes"`
	RetryCount     int    `json:"retryCount"`
	YouTubeQuality string `json:"youtubeQuality,omitempty"`
}

type OpenLocationRequest struct {
	Type  string `json:"type"`
	JobID string `json:"jobId"`
}

type Response struct {
	OK      bool   `json:"ok"`
	Type    string `json:"type"`
	JobID   string `json:"jobId,omitempty"`
	Status  string `json:"status,omitempty"`
	Message string `json:"message,omitempty"`
}

type JobSummary struct {
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
}
