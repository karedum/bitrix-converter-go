package command

import (
	"bitrix-converter/internal/lib/fileuploader"
	"context"
	"fmt"
	"github.com/go-playground/validator/v10"
	"os"
	"strconv"
	"sync"
)

const (
	DocumentCommand = "Bitrix\\TransformerController\\Document"
	VideoCommand    = "Bitrix\\TransformerController\\Video"
)

// Converter - конвертер конкретного типа файлов (документы, видео).
type Converter interface {
	Validate(task ConvertTask) error
	MaxSize() int64
	DownloadDir() string
	// Convert конвертирует src во все formats. Возвращает формат -> путь к результату.
	// Все созданные файлы должны быть добавлены в cleanup.
	Convert(ctx context.Context, src string, formats []string, cleanup *Cleanup) (map[string]string, error)
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

// Execute скачивает файл, конвертирует его, загружает результаты в Битрикс и удаляет временные файлы.
func Execute(ctx context.Context, conv Converter, task ConvertTask, uploader *fileuploader.FileUploader, uploadConcurrency int) error {
	if err := conv.Validate(task); err != nil {
		return fmt.Errorf("failed validate transform task: [%w]", err)
	}

	cleanup := &Cleanup{}
	defer cleanup.Remove()

	src, err := createTmpFile(conv.DownloadDir(), task.FileId)
	if err != nil {
		return err
	}
	cleanup.Add(src)

	if err = uploader.Download(ctx, task.File, src, conv.MaxSize()); err != nil {
		return fmt.Errorf("error download file [%s]: [%w]", task.File, err)
	}

	files, err := conv.Convert(ctx, src, task.Formats, cleanup)
	if err != nil {
		return fmt.Errorf("error transform file [%s]: [%w]", task.File, err)
	}

	uploaded, err := uploader.UploadFiles(ctx, files, uploadConcurrency)
	if err != nil {
		return fmt.Errorf("error uploading files: [%w]", err)
	}

	if err = uploader.Complete(ctx, uploaded); err != nil {
		return fmt.Errorf("failed complete: [%w]", err)
	}
	return nil
}

// createTmpFile создаёт файл с уникальным именем без расширения:
// из его имени libreoffice/ffmpeg строят имена результатов.
func createTmpFile(directory string, fileId int) (string, error) {
	if err := os.MkdirAll(directory, 0755); err != nil {
		return "", fmt.Errorf("error creating directory [%s]: [%w]", directory, err)
	}

	file, err := os.CreateTemp(directory, "original"+strconv.Itoa(fileId)+"-*")
	if err != nil {
		return "", fmt.Errorf("error creating tmp file in [%s]: [%w]", directory, err)
	}
	_ = file.Close()
	return file.Name(), nil
}

func newTaskValidator(rules map[string]string) *validator.Validate {
	v := validator.New()
	v.RegisterStructValidationMapRules(rules, ConvertTask{})
	return v
}

// Cleanup - список временных файлов, которые нужно удалить после выполнения задачи.
type Cleanup struct {
	mu    sync.Mutex
	files []string
}

func (c *Cleanup) Add(files ...string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.files = append(c.files, files...)
}

func (c *Cleanup) Remove() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, file := range c.files {
		_ = os.Remove(file)
	}
	c.files = nil
}
