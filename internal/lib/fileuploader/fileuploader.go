package fileuploader

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/avast/retry-go"
	"github.com/google/go-querystring/query"
	"io"
	"math"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"time"
	"strings"
)

type FileUploader struct {
	url           string
	files         map[string]string
	uploadedFiles map[string]string
	filesToDelete []string
}

type uploadInfoResp struct {
	Bucket    int
	Name      string
	ChunkSize int64 `json:"chunk_size"`
}

type uploadInfoRequest struct {
	FileId   string `url:"file_id"`
	FileSize int64  `url:"file_size"`
	Upload   string `url:"upload"`
}

type response struct {
	success *string
	error   *string
}

func New(url string) *FileUploader {
	return &FileUploader{
		url:           url,
		files:         make(map[string]string),
		uploadedFiles: make(map[string]string),
		filesToDelete: make([]string, 0),
	}
}

func (f *FileUploader) SetFiles(files map[string]string) {
	f.files = files
}

func (f *FileUploader) Files() map[string]string {
	return f.files
}

func (f *FileUploader) urlEncode(str string) (string, error) {
	u, err := url.Parse(str)
	if err != nil {
		return "", err
	}
	return u.String(), nil
}

func (f *FileUploader) Download(url string, filePath string, maxSize int64) error {
	client := &http.Client{
		Timeout: time.Minute * 5,
		Transport: &http.Transport{
			DialContext: (&net.Dialer{
				Timeout: 10 * time.Second,
			}).DialContext,
		},
	}

    url = f.fixInvalidUrlEscapes(url);

	res, err := client.Head(url)

	if err != nil {
		return fmt.Errorf("error head request: [%w]", err)
	}

	if res.StatusCode != http.StatusOK {

		url, err = f.urlEncode(url)

		if err != nil {
			return fmt.Errorf("url encoding failed: [%w]", err)
		}

		res, err = client.Head(url)

		if err != nil {
			return fmt.Errorf("error head request url encoding [%s]: [%w]", url, err)
		}

	}

	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("wrong http-status [%s] head request: [%w]", res.Status, err)
	}

	if res.Header.Get("Content-Type") == "" {
		return errors.New("content-type header in head request is empty")
	}

	contentLen := res.Header.Get("Content-Length")
	fileSize, err := strconv.ParseInt(contentLen, 10, 64)

	if err != nil {
		fileSize = 0
	}

	if fileSize == 0 {
		fileSize = maxSize
	}

	if fileSize > maxSize {
		return fmt.Errorf("file is too big [%d]", fileSize)
	}

	isBytesRanges := res.Header.Get("Accept-Ranges") == "bytes"

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return fmt.Errorf("error create new GET request: [%w]", err)
	}

	if isBytesRanges {
		req.Header.Add("Range", "bytes=0-"+strconv.FormatInt(fileSize, 10))
	}

	resp, err := client.Do(req)

	if err != nil {
		return fmt.Errorf("error downloading file: [%w]", err)
	}

	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		return fmt.Errorf("wrong http-status [%s] get request: [%w]", resp.Status, err)
	}

	file, err := os.Create(filePath)

	if err != nil {
		return fmt.Errorf("error creating file [%s]: [%w]", filePath, err)
	}

	if _, err = io.Copy(file, resp.Body); err != nil {
		return fmt.Errorf("error copy file [%s]: [%w]", filePath, err)
	}

	fileInfo, err := os.Stat(filePath)

	if err != nil {
		return fmt.Errorf("error getting file info [%s]: [%w]", filePath, err)
	}

	realFileSize := fileInfo.Size()

	defer file.Close()

	if realFileSize > maxSize {
		return fmt.Errorf("downloaded file is too big [%d]: [%w]", realFileSize, err)
	}

	return nil
}

func (f *FileUploader) DeleteFiles() {
	for _, file := range f.filesToDelete {
		_ = os.Remove(file)
	}
}

func (f *FileUploader) AddFileToDelete(file string) {
	f.filesToDelete = append(f.filesToDelete, file)
}

func (f *FileUploader) UploadFiles() error {
	var client = &http.Client{
		Timeout: time.Minute * 5,
		Transport: &http.Transport{
			DialContext: (&net.Dialer{
				Timeout: 10 * time.Second,
			}).DialContext,
		},
	}
	for i, file := range f.files {
		var uploadInfo = &uploadInfoResp{}

		err := retry.Do(
			func() error {
				var err error
				uploadInfo, err = f.getUploadInfo(file, i)
				return err
			},
			retry.Attempts(3),
			retry.OnRetry(func(n uint, err error) {
				time.Sleep(1 * time.Second)
			}),
		)

		if err != nil {
			return fmt.Errorf("error creating file [%s]: [%w]", file, err)
		}

		f.uploadedFiles[i] = uploadInfo.Name

		err = f.uploadFile(client, file, uploadInfo)

		if err != nil {
			return fmt.Errorf("error upload file [%s]: [%w]", file, err)
		}
	}
	return nil
}

func (f *FileUploader) uploadFile(client *http.Client, filePath string, uploadInfo *uploadInfoResp) error {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)

	file, err := os.Open(filePath)

	if err != nil {
		return fmt.Errorf("error open file [%s]: [%w]", filePath, err)
	}

	fileInfo, err := os.Stat(filePath)

	if err != nil {
		return fmt.Errorf("error get file stat [%s]: [%w]", filePath, err)
	}

	if uploadInfo.ChunkSize <= 0 {
		uploadInfo.ChunkSize = 1
	}

	dataLength := fileInfo.Size()

	parts := int(math.Ceil(float64(dataLength) / float64(uploadInfo.ChunkSize)))

	isLastPart := "n"

	if parts == 0 {
		parts = 1
	}

	buffer := make([]byte, uploadInfo.ChunkSize)

	for i := 1; i <= parts; i++ {

		if i == parts {
			isLastPart = "y"
		}

		bytesRead, err := file.Read(buffer)

		if err != nil {
			if err != io.EOF {
				return fmt.Errorf("read file to the end [%s]: [%w]", filePath, err)
			}
			return fmt.Errorf("error read file [%s]: [%w]", filePath, err)
		}

		fw, err := w.CreateFormFile("file", file.Name())

		if err != nil {
			return fmt.Errorf("error create form file [%s]: [%w]", filePath, err)
		}

		if _, err = fw.Write(buffer[:bytesRead]); err != nil {
			return fmt.Errorf("error copy file to writer [%s]: [%w]", filePath, err)
		}

		err = w.WriteField("file_name", uploadInfo.Name)

		if err != nil {
			return fmt.Errorf("error write file_name in form [%s]: [%w]", uploadInfo.Name, err)
		}

		err = w.WriteField("last_part", isLastPart)

		if err != nil {
			return fmt.Errorf("error write last_part in form [%s]: [%w]", isLastPart, err)
		}

		strFileSize := strconv.FormatInt(fileInfo.Size(), 10)

		err = w.WriteField("file_size", strFileSize)

		if err != nil {
			return fmt.Errorf("error write last_part in form [%s]: [%w]", strFileSize, err)
		}

		strBucket := strconv.Itoa(uploadInfo.Bucket)

		if uploadInfo.Bucket > 0 {
			err = w.WriteField("bucket", strBucket)
			if err != nil {
				return fmt.Errorf("error write bucket in form [%s]: [%w]", strBucket, err)
			}
		}

		err = w.Close()

		if err != nil {
			return fmt.Errorf("error close form file: [%w]", err)
		}

		req, err := http.NewRequest("POST", f.url, &buf)

		if err != nil {
			return fmt.Errorf("error new request upload file to url [%s]: [%w]", f.url, err)
		}

		req.Header.Set("Content-Type", w.FormDataContentType())

		var res = &http.Response{}

		err = retry.Do(
			func() error {
				res, err = client.Do(req)
				if err != nil {
					return fmt.Errorf("error upload file to url [%s]: [%w]", f.url, err)
				}

				if res.StatusCode != http.StatusOK {
					return fmt.Errorf("bad status [%s] upload file to url [%s]: [%w]", res.Status, f.url, err)
				}
				return err
			},
			retry.Attempts(3),
			retry.OnRetry(func(n uint, err error) {
				time.Sleep(1 * time.Second)
			}),
		)

		if err != nil {
			return err
		}

		uploadFileRes := response{}

		body, err := io.ReadAll(res.Body)

		if err != nil {
			return fmt.Errorf("wrong response upload file to url [%s]: [%w]", f.url, err)
		}

		if err = json.Unmarshal(body, &uploadFileRes); err != nil {
			return fmt.Errorf("error unmarshal response upload file to url [%s]: [%w]", f.url, err)
		}

		if uploadFileRes.error != nil {
			return fmt.Errorf("error when uploading file to url [%s] [%s]: [%w]", f.url, uploadFileRes.error, err)
		}

	}

	return nil
}

func (f *FileUploader) Complete() error {
	queryValues := url.Values{}
	queryValues.Add("finish", "y")

	for k, file := range f.uploadedFiles {
		queryValues.Add("result[files]["+k+"]", file)
	}
	client := &http.Client{
		Timeout: time.Second * 30,
		Transport: &http.Transport{
			DialContext: (&net.Dialer{
				Timeout: 10 * time.Second,
			}).DialContext,
		},
	}

	var res = &http.Response{}

	err := retry.Do(
		func() error {
			var err error
			res, err = client.PostForm(f.url, queryValues)
			return err
		},
		retry.Attempts(3),
		retry.OnRetry(func(n uint, err error) {
			time.Sleep(1 * time.Second)
		}),
	)

	if err != nil {
		return fmt.Errorf("error send complete request to url [%s]: [%w]", f.url, err)
	}

	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("bad status %s complete request to url [%s]: [%w]", res.Status, f.url, err)
	}

	completeRes := response{}

	body, err := io.ReadAll(res.Body)

	if err != nil {
		return fmt.Errorf("wrong response complete request to url [%s]: [%w]", f.url, err)
	}

	if err = json.Unmarshal(body, &completeRes); err != nil {
		return fmt.Errorf("error unmarshal complete request to url [%s]: [%w]", f.url, err)
	}

	if completeRes.error != nil {
		return fmt.Errorf("error complete request to url [%s] [%s]: [%w]", f.url, completeRes.error, err)
	}

	return nil
}

func (f *FileUploader) getUploadInfo(file string, key string) (*uploadInfoResp, error) {
	fileInfo, err := os.Stat(file)

	if err != nil {
		return nil, fmt.Errorf("error get file info [%s]: [%w]", file, err)
	}

	uploadReq := uploadInfoRequest{
		FileId:   key,
		FileSize: fileInfo.Size(),
		Upload:   "where",
	}

	v, err := query.Values(uploadReq)

	if err != nil {
		return nil, fmt.Errorf("error convert struct request to query: [%w]", err)
	}

	res, err := http.PostForm(f.url, v)

	if err != nil {
		return nil, fmt.Errorf("error get upload info from [%s]: [%w]", f.url, err)
	}

	var uploadInfoRes uploadInfoResp

	body, err := io.ReadAll(res.Body)

	if err != nil {
		return nil, fmt.Errorf("wrong response upload info request to url [%s]: [%w]", f.url, err)
	}

	if err = json.Unmarshal(body, &uploadInfoRes); err != nil {
		return nil, fmt.Errorf("error unmarshal upload info request to url [%s]: [%w]", f.url, err)
	}

	return &uploadInfoRes, nil
}

func (f *FileUploader) fixInvalidUrlEscapes(u string) string {
    var sb strings.Builder
    for i := 0; i < len(u); i++ {
        if u[i] == '%' {
            if i+2 < len(u) && f.isHex(u[i+1]) && f.isHex(u[i+2]) {
                sb.WriteByte(u[i])
            } else {
                sb.WriteString("%25")
            }
        } else {
            sb.WriteByte(u[i])
        }
    }
    return sb.String()
}

func (f *FileUploader) isHex(b byte) bool {
    return (b >= '0' && b <= '9') ||
        (b >= 'a' && b <= 'f') ||
        (b >= 'A' && b <= 'F')
}