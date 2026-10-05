package fileuploader

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/avast/retry-go/v4"
	"github.com/google/go-querystring/query"
	"golang.org/x/sync/errgroup"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	retryAttempts    = 3
	retryDelay       = 1 * time.Second
	completeTimeout  = 30 * time.Second
	maxResponseSize  = 1 << 20
	defaultChunkSize = 1 << 20
)

var ErrFileTooBig = errors.New("file is too big")

// FileUploader не хранит состояния задачи, поэтому один экземпляр можно использовать из нескольких горутин.
type FileUploader struct {
	url    string
	client *http.Client
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
	Success any `json:"success"`
	Error   any `json:"error"`
}

// NewHTTPClient создаёт клиент, который нужно создать один раз и переиспользовать:
// общий Transport держит пул keep-alive соединений и закрывает простаивающие.
func NewHTTPClient(timeout time.Duration) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = (&net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
	}).DialContext
	transport.MaxIdleConnsPerHost = 10
	transport.IdleConnTimeout = 90 * time.Second

	return &http.Client{
		Timeout:   timeout,
		Transport: transport,
	}
}

func New(url string, client *http.Client) *FileUploader {
	return &FileUploader{
		url:    url,
		client: client,
	}
}

func urlEncode(str string) (string, error) {
	u, err := url.Parse(str)
	if err != nil {
		return "", err
	}
	return u.String(), nil
}

func retryDo(ctx context.Context, fn func() error) error {
	return retry.Do(
		fn,
		retry.Context(ctx),
		retry.Attempts(retryAttempts),
		retry.Delay(retryDelay),
		retry.DelayType(retry.FixedDelay),
		retry.LastErrorOnly(true),
		retry.RetryIf(func(err error) bool {
			return !errors.Is(err, ErrFileTooBig)
		}),
	)
}

// drainClose дочитывает и закрывает тело ответа, чтобы соединение вернулось в пул.
func drainClose(body io.ReadCloser) {
	_, _ = io.Copy(io.Discard, io.LimitReader(body, maxResponseSize))
	_ = body.Close()
}

// Download скачивает файл с повторными попытками.
func (f *FileUploader) Download(ctx context.Context, url string, filePath string, maxSize int64) error {
	return retryDo(ctx, func() error {
		return f.download(ctx, url, filePath, maxSize)
	})
}

func (f *FileUploader) download(ctx context.Context, url string, filePath string, maxSize int64) error {
	url = fixInvalidUrlEscapes(url)

	res, err := f.head(ctx, url)

	if err != nil {
		return fmt.Errorf("error head request: [%w]", err)
	}

	if res.StatusCode != http.StatusOK {

		url, err = urlEncode(url)

		if err != nil {
			return fmt.Errorf("url encoding failed: [%w]", err)
		}

		res, err = f.head(ctx, url)

		if err != nil {
			return fmt.Errorf("error head request url encoding [%s]: [%w]", url, err)
		}

	}

	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("wrong http-status [%s] head request", res.Status)
	}

	if res.Header.Get("Content-Type") == "" {
		return errors.New("content-type header in head request is empty")
	}

	fileSize, err := strconv.ParseInt(res.Header.Get("Content-Length"), 10, 64)

	if err != nil || fileSize == 0 {
		fileSize = maxSize
	}

	if fileSize > maxSize {
		return fmt.Errorf("%w [%d]", ErrFileTooBig, fileSize)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("error create new GET request: [%w]", err)
	}

	if res.Header.Get("Accept-Ranges") == "bytes" {
		req.Header.Add("Range", "bytes=0-"+strconv.FormatInt(fileSize, 10))
	}

	resp, err := f.client.Do(req)

	if err != nil {
		return fmt.Errorf("error downloading file: [%w]", err)
	}

	defer drainClose(resp.Body)

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		return fmt.Errorf("wrong http-status [%s] get request", resp.Status)
	}

	return saveBody(filePath, resp.Body, maxSize)
}

func (f *FileUploader) head(ctx context.Context, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
	if err != nil {
		return nil, err
	}
	res, err := f.client.Do(req)
	if err != nil {
		return nil, err
	}
	// у HEAD нет тела, но закрыть его обязательно, иначе соединение не вернётся в пул
	drainClose(res.Body)
	return res, nil
}

// saveBody пишет не больше maxSize байт, чтобы слишком большой файл не заполнил диск.
func saveBody(filePath string, body io.Reader, maxSize int64) (err error) {
	file, err := os.Create(filePath)

	if err != nil {
		return fmt.Errorf("error creating file [%s]: [%w]", filePath, err)
	}

	defer func() {
		if closeErr := file.Close(); closeErr != nil && err == nil {
			err = fmt.Errorf("error closing file [%s]: [%w]", filePath, closeErr)
		}
	}()

	written, err := io.Copy(file, io.LimitReader(body, maxSize+1))
	if err != nil {
		return fmt.Errorf("error copy file [%s]: [%w]", filePath, err)
	}

	if written > maxSize {
		return fmt.Errorf("%w: downloaded more than [%d] bytes", ErrFileTooBig, maxSize)
	}

	return nil
}

// UploadFiles параллельно (не больше limit одновременно) загружает файлы в Битрикс.
// files: формат -> путь к файлу. Возвращает формат -> имя загруженного файла на стороне Битрикс.
func (f *FileUploader) UploadFiles(ctx context.Context, files map[string]string, limit int) (map[string]string, error) {
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(max(limit, 1))

	var mu sync.Mutex
	uploaded := make(map[string]string, len(files))

	for key, file := range files {
		g.Go(func() error {
			uploadInfo, err := f.getUploadInfo(gctx, file, key)

			if err != nil {
				return fmt.Errorf("error creating file [%s]: [%w]", file, err)
			}

			if err = f.uploadFile(gctx, file, uploadInfo); err != nil {
				return fmt.Errorf("error upload file [%s]: [%w]", file, err)
			}

			mu.Lock()
			uploaded[key] = uploadInfo.Name
			mu.Unlock()
			return nil
		})
	}

	if err := g.Wait(); err != nil {
		return nil, err
	}
	return uploaded, nil
}

func (f *FileUploader) uploadFile(ctx context.Context, filePath string, uploadInfo *uploadInfoResp) error {
	file, err := os.Open(filePath)

	if err != nil {
		return fmt.Errorf("error open file [%s]: [%w]", filePath, err)
	}

	defer file.Close()

	fileInfo, err := file.Stat()

	if err != nil {
		return fmt.Errorf("error get file stat [%s]: [%w]", filePath, err)
	}

	chunkSize := uploadInfo.ChunkSize

	if chunkSize <= 0 {
		chunkSize = defaultChunkSize
	}

	fileSize := fileInfo.Size()

	parts := max((fileSize+chunkSize-1)/chunkSize, 1)

	buffer := make([]byte, min(chunkSize, fileSize))

	var offset int64

	for i := int64(1); i <= parts; i++ {

		chunk := buffer[:min(chunkSize, fileSize-offset)]

		if _, err = io.ReadFull(file, chunk); err != nil {
			return fmt.Errorf("error read file [%s]: [%w]", filePath, err)
		}

		offset += int64(len(chunk))

		body, contentType, err := buildChunkForm(file.Name(), chunk, uploadInfo, i == parts, fileSize)

		if err != nil {
			return err
		}

		resBody, err := f.post(ctx, func(ctx context.Context) (*http.Request, error) {
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.url, bytes.NewReader(body))
			if err != nil {
				return nil, err
			}
			req.Header.Set("Content-Type", contentType)
			return req, nil
		})

		if err != nil {
			return fmt.Errorf("error upload file to url [%s]: [%w]", f.url, err)
		}

		if err = checkResponse(resBody); err != nil {
			return fmt.Errorf("error when uploading file to url [%s]: [%w]", f.url, err)
		}
	}

	return nil
}

func buildChunkForm(fileName string, chunk []byte, uploadInfo *uploadInfoResp, lastPart bool, fileSize int64) ([]byte, string, error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)

	fw, err := w.CreateFormFile("file", fileName)

	if err != nil {
		return nil, "", fmt.Errorf("error create form file [%s]: [%w]", fileName, err)
	}

	if _, err = fw.Write(chunk); err != nil {
		return nil, "", fmt.Errorf("error copy file to writer [%s]: [%w]", fileName, err)
	}

	isLastPart := "n"
	if lastPart {
		isLastPart = "y"
	}

	fields := [][2]string{
		{"file_name", uploadInfo.Name},
		{"last_part", isLastPart},
		{"file_size", strconv.FormatInt(fileSize, 10)},
	}

	if uploadInfo.Bucket > 0 {
		fields = append(fields, [2]string{"bucket", strconv.Itoa(uploadInfo.Bucket)})
	}

	for _, field := range fields {
		if err = w.WriteField(field[0], field[1]); err != nil {
			return nil, "", fmt.Errorf("error write %s in form [%s]: [%w]", field[0], field[1], err)
		}
	}

	if err = w.Close(); err != nil {
		return nil, "", fmt.Errorf("error close form file: [%w]", err)
	}

	return buf.Bytes(), w.FormDataContentType(), nil
}

// Complete сообщает Битриксу, что все файлы загружены.
func (f *FileUploader) Complete(ctx context.Context, uploaded map[string]string) error {
	queryValues := url.Values{}
	queryValues.Add("finish", "y")

	for k, file := range uploaded {
		queryValues.Add("result[files]["+k+"]", file)
	}

	ctx, cancel := context.WithTimeout(ctx, completeTimeout)
	defer cancel()

	body, err := f.postForm(ctx, queryValues)

	if err != nil {
		return fmt.Errorf("error send complete request to url [%s]: [%w]", f.url, err)
	}

	if err = checkResponse(body); err != nil {
		return fmt.Errorf("error complete request to url [%s]: [%w]", f.url, err)
	}

	return nil
}

func (f *FileUploader) getUploadInfo(ctx context.Context, file string, key string) (*uploadInfoResp, error) {
	fileInfo, err := os.Stat(file)

	if err != nil {
		return nil, fmt.Errorf("error get file info [%s]: [%w]", file, err)
	}

	v, err := query.Values(uploadInfoRequest{
		FileId:   key,
		FileSize: fileInfo.Size(),
		Upload:   "where",
	})

	if err != nil {
		return nil, fmt.Errorf("error convert struct request to query: [%w]", err)
	}

	body, err := f.postForm(ctx, v)

	if err != nil {
		return nil, fmt.Errorf("error get upload info from [%s]: [%w]", f.url, err)
	}

	var uploadInfoRes uploadInfoResp

	if err = json.Unmarshal(body, &uploadInfoRes); err != nil {
		return nil, fmt.Errorf("error unmarshal upload info request to url [%s]: [%w]", f.url, err)
	}

	return &uploadInfoRes, nil
}

func (f *FileUploader) postForm(ctx context.Context, values url.Values) ([]byte, error) {
	encoded := values.Encode()
	return f.post(ctx, func(ctx context.Context) (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.url, strings.NewReader(encoded))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		return req, nil
	})
}

// post выполняет запрос с повторами. newReq вызывается на каждую попытку,
// чтобы у каждой попытки было своё, ещё не прочитанное тело запроса.
func (f *FileUploader) post(ctx context.Context, newReq func(ctx context.Context) (*http.Request, error)) ([]byte, error) {
	var body []byte

	err := retryDo(ctx, func() error {
		req, err := newReq(ctx)
		if err != nil {
			return retry.Unrecoverable(err)
		}

		res, err := f.client.Do(req)
		if err != nil {
			return err
		}
		defer drainClose(res.Body)

		if res.StatusCode != http.StatusOK {
			return fmt.Errorf("bad status [%s]", res.Status)
		}

		body, err = io.ReadAll(io.LimitReader(res.Body, maxResponseSize))
		return err
	})

	return body, err
}

func checkResponse(body []byte) error {
	var res response

	if err := json.Unmarshal(body, &res); err != nil {
		return fmt.Errorf("error unmarshal response: [%w]", err)
	}

	if isSet(res.Error) {
		return fmt.Errorf("bitrix returned error: [%v]", res.Error)
	}
	return nil
}

func isSet(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case string:
		return t != ""
	case float64:
		return t != 0
	case []any:
		return len(t) > 0
	case map[string]any:
		return len(t) > 0
	default:
		return true
	}
}

func fixInvalidUrlEscapes(u string) string {
	var sb strings.Builder
	for i := 0; i < len(u); i++ {
		if u[i] == '%' {
			if i+2 < len(u) && isHex(u[i+1]) && isHex(u[i+2]) {
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

func isHex(b byte) bool {
	return (b >= '0' && b <= '9') ||
		(b >= 'a' && b <= 'f') ||
		(b >= 'A' && b <= 'F')
}
