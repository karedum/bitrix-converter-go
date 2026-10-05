package command

import (
	"bitrix-converter/internal/config"
	"context"
	"fmt"
	"golang.org/x/sync/errgroup"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sync"
)

const (
	maxWidth     = "1280"
	videoCommand = "ffmpeg"
	videoScale   = "scale=w='min(min(" + maxWidth + "\\,trunc(" + maxWidth + "/max(a/1.7778\\,1.7778/a)/2)*2)\\,trunc(iw/2)*2):h=-2'"
	videoDir     = "video"
)

var videoValidator = newTaskValidator(map[string]string{
	"BackUrl": "required",
	"File":    "required",
	"Formats": "required,min=1,dive,required,oneof=mp4 jpg",
})

type VideoConverter struct {
	cfg    config.ConvertConfig
	runner *Runner
}

func NewVideoConverter(cfg config.ConvertConfig, runner *Runner) *VideoConverter {
	return &VideoConverter{
		cfg:    cfg,
		runner: runner,
	}
}

func (v *VideoConverter) Validate(task ConvertTask) error {
	return videoValidator.Struct(task)
}

// Convert запускает конвертацию во все форматы параллельно (fan-out) и собирает результаты (fan-in).
// Количество одновременно работающих ffmpeg ограничивает семафор Runner.
func (v *VideoConverter) Convert(ctx context.Context, src string, formats []string, cleanup *Cleanup) (map[string]string, error) {
	directory := v.SuccessDir()

	if err := os.MkdirAll(directory, 0755); err != nil {
		return nil, fmt.Errorf("error creating directory [%s]: [%w]", directory, err)
	}

	var mu sync.Mutex
	files := make(map[string]string, len(formats))

	g, gctx := errgroup.WithContext(ctx)

	for _, format := range slices.Compact(slices.Sorted(slices.Values(formats))) {
		g.Go(func() error {
			result, err := v.ffmpeg(gctx, format, src, directory, cleanup)
			if err != nil {
				return err
			}

			mu.Lock()
			files[format] = result
			mu.Unlock()
			return nil
		})
	}

	if err := g.Wait(); err != nil {
		return nil, err
	}
	return files, nil
}

func (v *VideoConverter) ffmpeg(ctx context.Context, format string, src string, directory string, cleanup *Cleanup) (string, error) {
	result := filepath.Join(directory, filepath.Base(src)+"."+format)

	var args []string
	switch format {
	case "mp4":
		args = []string{"-loglevel", "warning", "-i", src, "-c:v", "libx264", "-r", "25", "-vf", videoScale,
			"-strict", "-2", "-preset", "fast", "-pix_fmt", "yuv420p", "-codec:a", "aac", "-f", "mp4", result}
	case "jpg":
		args = []string{"-loglevel", "warning", "-i", src, "-an", "-ss", "00:00:00", "-vf", videoScale,
			"-vframes:", "1", "-r", "1", "-y", result}
	default:
		return "", fmt.Errorf("unknown format [%s]", format)
	}

	cleanup.Add(result)

	if err := v.runner.Run(ctx, videoCommand, args...); err != nil {
		return "", fmt.Errorf("error ffmpeg command. file %s: [%w]", src, err)
	}

	return result, nil
}

func (v *VideoConverter) MaxSize() int64 {
	return v.cfg.MaxVideoSize
}

func (v *VideoConverter) SuccessDir() string {
	return path.Join(v.cfg.SuccessDir, videoDir)
}

func (v *VideoConverter) DownloadDir() string {
	return path.Join(v.cfg.DownloadDir, videoDir)
}
