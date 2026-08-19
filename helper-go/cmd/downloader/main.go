package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"robot-downloader/helper-go/internal/api"
	"robot-downloader/helper-go/internal/config"
	"robot-downloader/helper-go/internal/downloader"
)

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

	for {
		raw, err := readNativeMessage(os.Stdin)
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}

		msgType, _ := raw["type"].(string)
		var response any

		switch msgType {
		case "enqueue":
			payloadBytes, err := json.Marshal(raw)
			if err != nil {
				return err
			}
			var req api.EnqueueRequest
			if err := json.Unmarshal(payloadBytes, &req); err != nil {
				return err
			}

			job, err := service.Enqueue(context.Background(), req)
			if err != nil {
				response = api.Response{OK: false, Type: "enqueue", Message: err.Error()}
				break
			}

			response = api.Response{OK: true, Type: "enqueue", JobID: job.JobID, Status: job.Status, Message: fmt.Sprintf("queued %s", job.Filename)}
		case "list":
			jobs, err := service.List()
			if err != nil {
				response = api.Response{OK: false, Type: "list", Message: err.Error()}
				break
			}
			summaries := make([]api.JobSummary, 0, len(jobs))
			for _, job := range jobs {
				canResume, reason := service.CanResume(job)
				summaries = append(summaries, api.JobSummary{
					JobID:           job.JobID,
					URL:             job.URL,
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
				})
			}
			response = summaries
		case "open-location":
			payloadBytes, err := json.Marshal(raw)
			if err != nil {
				return err
			}
			var req api.OpenLocationRequest
			if err := json.Unmarshal(payloadBytes, &req); err != nil {
				return err
			}
			if strings.TrimSpace(req.JobID) == "" {
				response = api.Response{OK: false, Type: "open-location", Message: "jobId is required"}
				break
			}
			if err := service.OpenLocation(req.JobID); err != nil {
				response = api.Response{OK: false, Type: "open-location", JobID: req.JobID, Message: err.Error()}
				break
			}
			response = api.Response{OK: true, Type: "open-location", JobID: req.JobID, Message: "opened file location"}
		case "resume-job":
			payloadBytes, err := json.Marshal(raw)
			if err != nil {
				return err
			}
			var req api.OpenLocationRequest
			if err := json.Unmarshal(payloadBytes, &req); err != nil {
				return err
			}
			if strings.TrimSpace(req.JobID) == "" {
				response = api.Response{OK: false, Type: "resume-job", Message: "jobId is required"}
				break
			}
			job, err := service.ResumeJob(context.Background(), req.JobID)
			if err != nil {
				response = api.Response{OK: false, Type: "resume-job", JobID: req.JobID, Message: err.Error()}
				break
			}
			response = api.Response{OK: true, Type: "resume-job", JobID: job.JobID, Status: job.Status, Message: fmt.Sprintf("resuming %s", job.Filename)}
		case "cancel-job":
			payloadBytes, err := json.Marshal(raw)
			if err != nil {
				return err
			}
			var req api.OpenLocationRequest
			if err := json.Unmarshal(payloadBytes, &req); err != nil {
				return err
			}
			if strings.TrimSpace(req.JobID) == "" {
				response = api.Response{OK: false, Type: "cancel-job", Message: "jobId is required"}
				break
			}
			job, err := service.CancelJob(req.JobID)
			if err != nil {
				response = api.Response{OK: false, Type: "cancel-job", JobID: req.JobID, Message: err.Error()}
				break
			}
			response = api.Response{OK: true, Type: "cancel-job", JobID: job.JobID, Status: job.Status, Message: fmt.Sprintf("canceled %s", job.Filename)}
		case "remove-job":
			payloadBytes, err := json.Marshal(raw)
			if err != nil {
				return err
			}
			var req api.OpenLocationRequest
			if err := json.Unmarshal(payloadBytes, &req); err != nil {
				return err
			}
			if strings.TrimSpace(req.JobID) == "" {
				response = api.Response{OK: false, Type: "remove-job", Message: "jobId is required"}
				break
			}
			if err := service.RemoveJob(req.JobID); err != nil {
				response = api.Response{OK: false, Type: "remove-job", JobID: req.JobID, Message: err.Error()}
				break
			}
			response = api.Response{OK: true, Type: "remove-job", JobID: req.JobID, Message: "removed job and data"}
		default:
			response = api.Response{OK: false, Type: msgType, Message: "unsupported message type"}
		}

		if err := writeNativeMessage(os.Stdout, response); err != nil {
			return err
		}
	}
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
	if _, err := w.Write(body); err != nil {
		return err
	}
	return nil
}
