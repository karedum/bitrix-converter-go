package command

import (
	"bitrix-converter/internal/config"
	"bitrix-converter/internal/lib/fileuploader"
	"bytes"
	"fmt"
	"github.com/go-playground/validator/v10"
	"strings"

	"log/slog"
	"os"
	"os/exec"
	"path"
	"path/filepath"
)

const (
	maxWidth     = "1280"
	videoCommand = "ffmpeg"
	videoMp4Arg  = "-loglevel warning -i %s -c:v libx264 -r 25 -vf scale=w='min(min(" + maxWidth + "\\,trunc(" + maxWidth + "/max(a/1.7778\\,1.7778/a)/2)*2)\\,trunc(iw/2)*2):h=-2' -strict -2 -preset fast -pix_fmt yuv420p -codec:a aac -f mp4 %s"
	videoJpgArg  = "-loglevel warning -i %s -an -ss 00:00:00 -vf scale=w='min(min(" + maxWidth + "\\,trunc(" + maxWidth + "/max(a/1.7778\\,1.7778/a)/2)*2)\\,trunc(iw/2)*2):h=-2' -vframes: 1 -r 1 -y %s"
	videoDir     = "video"
)

type VideoCommand struct {
	*BaseCommand
}

func NewVideoCommand(task ConvertTask, log *slog.Logger, uploader fileuploader.FileUploader, cfg config.ConvertConfig) *VideoCommand {
	bs := BaseCommand{
		uploader: uploader,
		task:     task,
		log:      log,
		cfg:      cfg,
		files:    make(map[string]string),
	}
	vd := &VideoCommand{
		BaseCommand: &bs,
	}
	bs.Command = vd
	return vd
}

func (v *VideoCommand) validate() error {
	validate := validator.New()

	rules := map[string]string{
		"BackUrl": "required",
		"File":    "required",
		"Formats": "required,min=1,dive,required,oneof=mp4 jpg",
	}
	validate.RegisterStructValidationMapRules(rules, v.task)

	return validate.Struct(v.task)

}

func (v *VideoCommand) transform(format string, filePath string) (string, error) {
	fileInfo, err := os.Stat(filePath)
	if err != nil {
		return "", fmt.Errorf("error get file info [%s]: [%w]", filePath, err)
	}
	directory := v.SuccessDir()
	err = os.MkdirAll(directory, 0755)
	if err != nil {
		return "", fmt.Errorf("error creating directory [%s]: [%w]", directory, err)
	}
	file := filepath.Join(directory, fileInfo.Name()+"."+format)
	cmd := &exec.Cmd{}
	var args []string
	switch format {
	case "mp4":
		args = strings.Fields(fmt.Sprintf(videoMp4Arg, filePath, file))
	case "jpg":
		args = strings.Fields(fmt.Sprintf(videoJpgArg, filePath, file))
	default:
		return "", fmt.Errorf("unknown format [%s]", format)
	}
	cmd = exec.Command(videoCommand, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err = cmd.Run(); err != nil {
		return "", fmt.Errorf("error ffmpeg command. file %s: [%w]", filePath, err)
	}

	return file, nil
}

func (v *VideoCommand) preConvert(format string, filePath string) (bool, error) {
	return false, nil
}

func (v *VideoCommand) MaxSize() int64 {
	return v.cfg.MaxVideoSize
}

func (v *VideoCommand) SuccessDir() string {
	return path.Join(v.cfg.SuccessDir, videoDir)
}

func (v *VideoCommand) DownloadDir() string {
	return path.Join(v.cfg.DownloadDir, videoDir)
}
