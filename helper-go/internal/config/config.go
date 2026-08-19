package config

import (
	"os"
	"path/filepath"
)

type Config struct {
	RootDir      string
	JobsDir      string
	ChunksDir    string
	DownloadsDir string
}

func Load() (Config, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Config{}, err
	}

	root := filepath.Join(home, ".robot-downloader")
	return Config{
		RootDir:      root,
		JobsDir:      filepath.Join(root, "jobs"),
		ChunksDir:    filepath.Join(root, "chunks"),
		DownloadsDir: filepath.Join(root, "downloads"),
	}, nil
}

func (c Config) Ensure() error {
	for _, dir := range []string{c.RootDir, c.JobsDir, c.ChunksDir, c.DownloadsDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return nil
}
