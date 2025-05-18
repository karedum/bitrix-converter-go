package command

import (
	"bitrix-converter/internal/config"
	"bitrix-converter/internal/lib/fileuploader"
	"fmt"
	"github.com/avast/retry-go"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

type Command interface {
	Execute() error
	validate() error
	transform(format string, filePath string) (string, error)
	MaxSize() int64
	ConvertDir() string
	DownloadDir() string
	preConvert(format string, filePath string) (bool, error)
}

type BaseCommand struct {
	Command
	uploader fileuploader.FileUploader
	log      *slog.Logger
	task     ConvertTask
	cfg      config.ConvertConfig
	file     string
	files    map[string]string
}

type ConvertTask struct {
	Id        string
	FileId    int
	File      string
	FileSize  int64
	Formats   []string
	BackUrl   string `form:"back_url"`
	Command   string
	Queue     string
	RequestID string
}

func (bs *BaseCommand) genTmpFilePath(directory string) string {
	fileName := "original" + strconv.Itoa(bs.task.FileId) + strconv.Itoa(time.Now().Nanosecond())

	return filepath.Join(directory, fileName)

}

func (bs *BaseCommand) Execute() error {

	if err := bs.validate(); err != nil {
		return fmt.Errorf("failed validate transform task: [%w]", err)
	}

	directory := bs.DownloadDir()

	err := os.MkdirAll(directory, 0755)

	if err != nil {
		return fmt.Errorf("error creating directory [%s]: [%w]", directory, err)
	}

	filePath := bs.genTmpFilePath(directory)

	err = retry.Do(
		func() error {
			return bs.uploader.Download(bs.task.File, filePath, bs.MaxSize())
		},
		retry.Attempts(3),
		retry.OnRetry(func(n uint, err error) {
			time.Sleep(1 * time.Second)
		}),
	)

	bs.uploader.AddFileToDelete(filePath)

	defer bs.uploader.DeleteFiles()

	if err != nil {
		return fmt.Errorf("error download file [%s]: [%w]", bs.task.File, err)
	}

	bs.file = filePath

	for _, format := range bs.task.Formats {

		if _, ok := bs.files[format]; ok {
			continue
		}
		pre, err := bs.preConvert(format, filePath)
		if err != nil {
			return err
		}
		if pre {
			continue
		}

		convertedFile, err := bs.transform(format, filePath)
		bs.uploader.AddFileToDelete(convertedFile)
		if err != nil {
			return fmt.Errorf("error transform file [%s] to [%s]: [%w]", bs.task.File, format, err)
		}
		bs.files[format] = convertedFile
	}

	bs.uploader.SetFiles(bs.files)

	err = bs.uploader.UploadFiles()
	if err != nil {
		return fmt.Errorf("error uploading files: [%w]", err)
	}

	err = bs.uploader.Complete()
	if err != nil {
		return fmt.Errorf("failed complete: [%w]", err)
	}
	return nil
}
