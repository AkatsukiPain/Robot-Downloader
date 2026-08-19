package storage

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"robot-downloader/helper-go/internal/jobstate"
)

type JobStore struct {
	jobsDir string
}

func NewJobStore(jobsDir string) *JobStore {
	return &JobStore{jobsDir: jobsDir}
}

func (s *JobStore) Save(job jobstate.Job) error {
	path := filepath.Join(s.jobsDir, fmt.Sprintf("%s.json", job.JobID))
	data, err := json.MarshalIndent(job, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func (s *JobStore) Load(jobID string) (jobstate.Job, error) {
	path := filepath.Join(s.jobsDir, fmt.Sprintf("%s.json", jobID))
	data, err := os.ReadFile(path)
	if err != nil {
		return jobstate.Job{}, err
	}

	var job jobstate.Job
	if err := json.Unmarshal(data, &job); err != nil {
		return jobstate.Job{}, err
	}
	return job, nil
}

func (s *JobStore) Get(jobID string) (jobstate.Job, error) {
	return s.Load(jobID)
}

func (s *JobStore) List() ([]jobstate.Job, error) {
	entries, err := os.ReadDir(s.jobsDir)
	if err != nil {
		return nil, err
	}

	jobs := make([]jobstate.Job, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}

		data, err := os.ReadFile(filepath.Join(s.jobsDir, entry.Name()))
		if err != nil {
			return nil, err
		}

		var job jobstate.Job
		if err := json.Unmarshal(data, &job); err != nil {
			return nil, err
		}
		jobs = append(jobs, job)
	}

	sort.Slice(jobs, func(i, j int) bool {
		return jobs[i].CreatedAt.After(jobs[j].CreatedAt)
	})
	return jobs, nil
}
