package jobstate

import "time"

type ChunkState struct {
	Index      int   `json:"index"`
	Start      int64 `json:"start"`
	End        int64 `json:"end"`
	Completed  bool  `json:"completed"`
	BytesSaved int64 `json:"bytesSaved"`
}

type Job struct {
	JobID              string            `json:"jobId"`
	URL                string            `json:"url"`
	Filename           string            `json:"filename"`
	Status             string            `json:"status"`
	CreatedAt          time.Time         `json:"createdAt"`
	ModifiedAt         time.Time         `json:"modifiedAt"`
	OutputPath         string            `json:"outputPath"`
	ChunksDir          string            `json:"chunksDir"`
	MaxConnections     int               `json:"maxConnections"`
	ChunkSizeBytes     int64             `json:"chunkSizeBytes"`
	RetryCount         int               `json:"retryCount"`
	YouTubeQuality     string            `json:"youtubeQuality,omitempty"`
	TotalBytes         int64             `json:"totalBytes"`
	DownloadedBytes    int64             `json:"downloadedBytes"`
	AcceptRanges       bool              `json:"acceptRanges"`
	ContentType        string            `json:"contentType,omitempty"`
	ContentDisposition string            `json:"contentDisposition,omitempty"`
	ETag               string            `json:"etag,omitempty"`
	RequestHeaders     map[string]string `json:"requestHeaders,omitempty"`
	OriginalHeaders    map[string]string `json:"originalHeaders,omitempty"`
	OriginalPageURL    string            `json:"originalPageUrl,omitempty"`
	OriginalURL        string            `json:"originalUrl,omitempty"`
	Chunks             []ChunkState      `json:"chunks"`
	StreamKind         string            `json:"streamKind,omitempty"`
	Extractor          string            `json:"extractor,omitempty"`
	ExtractorSourceURL string            `json:"extractorSourceUrl,omitempty"`
	ProgressPercent    float64           `json:"progressPercent,omitempty"`
	ProgressText       string            `json:"progressText,omitempty"`
	SpeedText          string            `json:"speedText,omitempty"`
	ETAText            string            `json:"etaText,omitempty"`
	LastError          string            `json:"lastError,omitempty"`
}
