package command

import (
	"archive/zip"
	"bitrix-converter/internal/config"
	"bitrix-converter/internal/lib/util"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
)

const (
	libreofficeCommand = "libreoffice"
	documentDir        = "documents"
	imageMagicCommand  = "convert"
)

var (
	// форматы, которые получаются из pdf, а не из исходного файла
	convertFromPdf = []string{
		"jpg",
		"pngAllPages",
	}

	documentValidator = newTaskValidator(map[string]string{
		"BackUrl": "required",
		"File":    "required",
		"Formats": "required,min=1,dive,required,oneof=pdf jpg txt text md5 sha1 crc32 pngAllPages",
	})
)

type DocumentConverter struct {
	cfg    config.ConvertConfig
	runner *Runner
	// profileDir - отдельный профиль libreoffice: два процесса с одним профилем не могут работать одновременно
	profileDir string
}

func NewDocumentConverter(cfg config.ConvertConfig, runner *Runner, workerID string) *DocumentConverter {
	return &DocumentConverter{
		cfg:        cfg,
		runner:     runner,
		profileDir: filepath.Join(os.TempDir(), "libreoffice", workerID),
	}
}

func (d *DocumentConverter) Validate(task ConvertTask) error {
	return documentValidator.Struct(task)
}

func (d *DocumentConverter) Convert(ctx context.Context, src string, formats []string, cleanup *Cleanup) (map[string]string, error) {
	directory := d.SuccessDir()

	if err := os.MkdirAll(directory, 0755); err != nil {
		return nil, fmt.Errorf("error create directory [%s]: [%w]", directory, err)
	}

	files := make(map[string]string, len(formats))

	// pdf нужен и как самостоятельный формат, и как промежуточный для jpg/pngAllPages - получаем его один раз
	var pdf string
	if slices.Contains(formats, "pdf") || slices.ContainsFunc(formats, isFromPdf) {
		if isPdfFile(src) {
			pdf = src
		} else {
			var err error
			if pdf, err = d.libreoffice(ctx, "pdf", src, directory, cleanup); err != nil {
				return nil, err
			}
		}
		if slices.Contains(formats, "pdf") {
			files["pdf"] = pdf
		}
	}

	for _, format := range formats {
		if _, ok := files[format]; ok {
			continue
		}

		var (
			result string
			err    error
		)

		switch format {
		case "jpg":
			result, err = d.libreoffice(ctx, format, pdf, directory, cleanup)
		case "pngAllPages":
			result, err = d.convertToPng(ctx, pdf, cleanup)
		default:
			result, err = d.libreoffice(ctx, format, src, directory, cleanup)
		}

		if err != nil {
			return nil, fmt.Errorf("error transform file to [%s]: [%w]", format, err)
		}
		files[format] = result
	}

	return files, nil
}

func isFromPdf(format string) bool {
	return slices.Contains(convertFromPdf, format)
}

func (d *DocumentConverter) libreoffice(ctx context.Context, format string, src string, directory string, cleanup *Cleanup) (string, error) {
	result := filepath.Join(directory, util.FileNameNotExt(filepath.Base(src))+"."+format)
	cleanup.Add(result)

	err := d.runner.Run(ctx, libreofficeCommand,
		"-env:UserInstallation=file://"+d.profileDir,
		"--convert-to", format,
		"--outdir", directory,
		src,
		"--headless",
		"--display", ":0",
	)
	if err != nil {
		return "", fmt.Errorf("error libreoffice convert file [%s] to [%s]: [%w]", src, format, err)
	}

	return result, nil
}

func (d *DocumentConverter) convertToPng(ctx context.Context, pdf string, cleanup *Cleanup) (string, error) {
	pngFileName := pdf + ".png"

	err := d.runner.Run(ctx, imageMagicCommand, "-density", "150", pdf, "-quality", "90", pngFileName)
	if err != nil {
		return "", fmt.Errorf("error image magic command: [%w]", err)
	}

	pngs := map[string]string{}

	// одностраничный документ - file.png, многостраничный - file-0.png, file-1.png, ...
	if _, err = os.Stat(pngFileName); err == nil {
		pngs["0.png"] = pngFileName
		cleanup.Add(pngFileName)
	} else {
		for counter := 0; ; counter++ {
			pngFileName = pdf + "-" + strconv.Itoa(counter) + ".png"
			if _, err = os.Stat(pngFileName); err != nil {
				break
			}
			cleanup.Add(pngFileName)
			pngs[strconv.Itoa(counter)+".png"] = pngFileName
		}
	}

	zipPath := pdf + "_pngs.zip"
	cleanup.Add(zipPath)

	if err = zipArchive(zipPath, pngs); err != nil {
		return "", fmt.Errorf("error zipping files: [%w]", err)
	}
	return zipPath, nil
}

func zipArchive(zipPath string, pngs map[string]string) (err error) {
	zipFile, err := os.Create(zipPath)
	if err != nil {
		return fmt.Errorf("error creating zip file [%s]: [%w]", zipPath, err)
	}
	defer func() {
		if closeErr := zipFile.Close(); closeErr != nil && err == nil {
			err = fmt.Errorf("error closing zip file [%s]: [%w]", zipPath, closeErr)
		}
	}()

	zipWriter := zip.NewWriter(zipFile)

	for name, filePath := range pngs {
		if err = addFileToZip(zipWriter, name, filePath); err != nil {
			return err
		}
	}

	if err = zipWriter.Close(); err != nil {
		return fmt.Errorf("error finishing zip file [%s]: [%w]", zipPath, err)
	}
	return nil
}

func addFileToZip(zipWriter *zip.Writer, name string, filePath string) error {
	file, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("error opening file [%s]: [%w]", filePath, err)
	}
	defer file.Close()

	w, err := zipWriter.Create(name)
	if err != nil {
		return fmt.Errorf("error creating zip writer [%s]: [%w]", name, err)
	}

	if _, err = io.Copy(w, file); err != nil {
		return fmt.Errorf("error copying file to zip writer [%s]: [%w]", filePath, err)
	}
	return nil
}

func isPdfFile(filePath string) bool {
	f, err := os.Open(filePath)
	if err != nil {
		return false
	}
	defer f.Close()

	buffer := make([]byte, 512)

	n, err := io.ReadFull(f, buffer)
	if n == 0 && err != nil {
		return false
	}

	return http.DetectContentType(buffer[:n]) == "application/pdf"
}

func (d *DocumentConverter) MaxSize() int64 {
	return d.cfg.MaxDocumentSize
}

func (d *DocumentConverter) SuccessDir() string {
	return path.Join(d.cfg.SuccessDir, documentDir)
}

func (d *DocumentConverter) DownloadDir() string {
	return path.Join(d.cfg.DownloadDir, documentDir)
}
