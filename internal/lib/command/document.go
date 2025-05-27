package command

import (
	"archive/zip"
	"bitrix-converter/internal/config"
	"bitrix-converter/internal/lib/fileuploader"
	"bitrix-converter/internal/lib/util"
	"fmt"
	"github.com/go-playground/validator/v10"

	"io"
	"net/http"
	"slices"
	"strings"

	"log/slog"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
)

const (
	libreofficeCommand = "libreoffice"
	libreofficeArg     = "-env:UserInstallation=file://%s --convert-to %s --outdir %s %s --headless --display :0"
	documentDir        = "documents"
	imageMagicCommand  = "convert"
	imageMagicArg      = "-density 150 %s -quality 90 %s"
)

var (
	convertFromPdf = []string{
		"jpg",
		"pngAllPages",
	}
)

type DocumentCommand struct {
	*BaseCommand
	uniqId string
}

func NewDocumentCommand(task ConvertTask, log *slog.Logger, uploader fileuploader.FileUploader, cfg config.ConvertConfig, uniqId string) *DocumentCommand {
	bs := BaseCommand{
		uploader: uploader,
		task:     task,
		log:      log,
		cfg:      cfg,
		files:    make(map[string]string),
	}
	doc := &DocumentCommand{
		BaseCommand: &bs,
		uniqId:      uniqId,
	}
	bs.Command = doc
	return doc
}

func (d *DocumentCommand) validate() error {

	validate := validator.New()

	rules := map[string]string{
		"BackUrl": "required",
		"File":    "required",
		"Formats": "required,min=1,dive,required,oneof=pdf jpg txt text md5 sha1 crc32 pngAllPages",
	}
	validate.RegisterStructValidationMapRules(rules, d.task)

	return validate.Struct(d.task)

}

func (d *DocumentCommand) transform(format string, filePath string) (string, error) {
	fileInfo, err := os.Stat(filePath)

	if err != nil {
		return "", fmt.Errorf("error get file info [%s]: [%w]", filePath, err)
	}

	directory := d.SuccessDir()

	err = os.MkdirAll(directory, 0755)

	if err != nil {
		return "", fmt.Errorf("error create directory [%s]: [%w]", directory, err)
	}

	randTmpDir := filepath.Join(os.TempDir(), "libreoffice", d.uniqId)

	args := strings.Fields(fmt.Sprintf(libreofficeArg, randTmpDir, format, directory, filePath))

	cmd := exec.Command(libreofficeCommand, args...)

	if err = cmd.Run(); err != nil {
		return "", fmt.Errorf("error libreoffice command file [%s]: [%w]", filePath, err)
	}

	return filepath.Join(directory, util.FileNameNotExt(fileInfo.Name())+"."+format), nil
}

func (d *DocumentCommand) preConvert(format string, filePath string) (bool, error) {
	needConvert := slices.Contains(convertFromPdf, format)
	if needConvert {
		pdf := d.existPdfFile()
		var err error
		if pdf == "" {
			pdf, err = d.transform("pdf", filePath)
			if err != nil {
				return false, fmt.Errorf("error transform file [%s] to [%s]: [%w]", d.task.File, format, err)
			}
			d.uploader.AddFileToDelete(pdf)
			needPdf := slices.Contains(d.task.Formats, "pdf")
			if needPdf {
				d.files["pdf"] = pdf
			}
		}

		switch format {
		case "jpg":
			jpg, err := d.transform(format, pdf)
			d.uploader.AddFileToDelete(jpg)
			if err != nil {
				return false, fmt.Errorf("error transform file [%s] to [%s]: [%w]", d.task.File, format, err)
			}
			d.files[format] = jpg
			return true, nil
		case "pngAllPages":
			zipPath, err := d.convertToPng(pdf)

			if err != nil {
				return false, fmt.Errorf("error transform file to pngAllPages [%s]: [%w]", pdf, err)
			}

			d.files["pngAllPages"] = zipPath
			return true, nil
		}
	}
	return false, nil
}

func (d *DocumentCommand) convertToPng(pdf string) (string, error) {
	pngFileName := pdf + ".png"
	pngs := map[string]string{}

	args := strings.Fields(fmt.Sprintf(imageMagicArg, pdf, pngFileName))

	cmd := exec.Command(imageMagicCommand, args...)
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("error image magic command: [%w]", err)
	}

	_, err := os.Stat(pngFileName)
	if err == nil {
		pngs["0.png"] = pngFileName
		d.uploader.AddFileToDelete(pngFileName)
	} else {
		counter := 0
		pngFileName = pdf + "-" + strconv.Itoa(counter) + ".png"

		for {
			_, err := os.Stat(pngFileName)
			if err != nil {
				break
			}
			d.uploader.AddFileToDelete(pngFileName)
			pngs[strconv.Itoa(counter)+".png"] = pngFileName
			counter++
			pngFileName = pdf + "-" + strconv.Itoa(counter) + ".png"
		}
	}

	zipPath := pdf + "_pngs.zip"
	d.uploader.AddFileToDelete(zipPath)
	err = d.zipArchive(zipPath, pngs)
	if err != nil {
		return "", fmt.Errorf("error zipping files: [%w]", err)
	}
	return zipPath, nil
}

func (d *DocumentCommand) zipArchive(zipPath string, pngs map[string]string) error {

	zipFile, err := os.Create(zipPath)
	if err != nil {
		return fmt.Errorf("error creating zip file [%s]: [%w]", zipPath, err)
	}
	defer zipFile.Close()

	zipWriter := zip.NewWriter(zipFile)
	defer zipWriter.Close()
	for name, filePath := range pngs {
		file, err := os.Open(filePath)
		if err != nil {
			return fmt.Errorf("error opening file [%s]: [%w]", filePath, err)
		}
		defer file.Close()

		w, err := zipWriter.Create(name)
		if err != nil {
			return fmt.Errorf("error creating zip writer [%s]: [%w]", name, err)
		}

		_, err = io.Copy(w, file)
		if err != nil {
			return fmt.Errorf("error copying file to zip writer [%s]: [%w]", filePath, err)
		}
	}
	return nil
}

func (d *DocumentCommand) isPdfFile() bool {
	f, err := os.Open(d.file)
	if err != nil {
		return false
	}
	defer f.Close()
	buffer := make([]byte, 512)

	_, err = f.Read(buffer)
	if err != nil {
		return false
	}

	contentType := http.DetectContentType(buffer)

	return contentType == "application/pdf"
}

func (d *DocumentCommand) existPdfFile() string {
	if d.isPdfFile() {
		return d.file
	}
	convertedFiles := d.files

	if file, ok := convertedFiles["pdf"]; ok {
		return file
	}
	return ""
}

func (d *DocumentCommand) MaxSize() int64 {
	return d.cfg.MaxDocumentSize
}

func (d *DocumentCommand) SuccessDir() string {
	return path.Join(d.cfg.SuccessDir, documentDir)
}

func (d *DocumentCommand) DownloadDir() string {
	return path.Join(d.cfg.DownloadDir, documentDir)
}
