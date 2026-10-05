package fileuploader

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestFixInvalidUrlEscapes(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"http://a/b%20c", "http://a/b%20c"},
		{"http://a/100%", "http://a/100%25"},
		{"http://a/50%off", "http://a/50%25off"},
		{"http://a/%2", "http://a/%252"},
		{"http://a/%E2%82%AC", "http://a/%E2%82%AC"},
	}
	for _, tt := range tests {
		if got := fixInvalidUrlEscapes(tt.in); got != tt.want {
			t.Errorf("fixInvalidUrlEscapes(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestCheckResponse(t *testing.T) {
	tests := []struct {
		body    string
		wantErr bool
	}{
		{`{"success":"y"}`, false},
		{`{"success":true,"error":false}`, false},
		{`{"error":""}`, false},
		{`{"error":null}`, false},
		{`{"error":"boom"}`, true},
		{`{"error":{"code":1}}`, true},
		{`not json`, true},
	}
	for _, tt := range tests {
		if err := checkResponse([]byte(tt.body)); (err != nil) != tt.wantErr {
			t.Errorf("checkResponse(%s) error = %v, wantErr %v", tt.body, err, tt.wantErr)
		}
	}
}

func TestDownload(t *testing.T) {
	content := []byte("hello world")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("Content-Length", fmt.Sprint(len(content)))
		if r.Method == http.MethodGet {
			_, _ = w.Write(content)
		}
	}))
	defer srv.Close()

	client := NewHTTPClient(5 * time.Second)
	defer client.CloseIdleConnections()

	dst := filepath.Join(t.TempDir(), "file")
	if err := New("", client).Download(context.Background(), srv.URL+"/file", dst, 100); err != nil {
		t.Fatalf("Download: %v", err)
	}

	got, _ := os.ReadFile(dst)
	if !bytes.Equal(got, content) {
		t.Fatalf("downloaded %q, want %q", got, content)
	}
}

func TestDownloadTooBig(t *testing.T) {
	var heads atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		if r.Method == http.MethodHead {
			heads.Add(1)
			w.Header().Set("Content-Length", "1000")
		}
	}))
	defer srv.Close()

	client := NewHTTPClient(5 * time.Second)
	defer client.CloseIdleConnections()

	err := New("", client).Download(context.Background(), srv.URL, filepath.Join(t.TempDir(), "f"), 10)
	if !errors.Is(err, ErrFileTooBig) {
		t.Fatalf("want ErrFileTooBig, got %v", err)
	}
	if heads.Load() != 1 {
		t.Fatalf("too big file must not be retried, got %d HEAD requests", heads.Load())
	}
}

func TestDownloadLimitsBodyWithoutContentLength(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		if r.Method == http.MethodGet {
			// без Content-Length (chunked) сервер отдаёт больше, чем maxSize
			for range 10 {
				_, _ = w.Write(bytes.Repeat([]byte("x"), 10))
				w.(http.Flusher).Flush()
			}
		}
	}))
	defer srv.Close()

	client := NewHTTPClient(5 * time.Second)
	defer client.CloseIdleConnections()

	err := New("", client).Download(context.Background(), srv.URL, filepath.Join(t.TempDir(), "f"), 20)
	if !errors.Is(err, ErrFileTooBig) {
		t.Fatalf("want ErrFileTooBig, got %v", err)
	}
}

// fakeBitrix эмулирует приёмник файлов Битрикс: выдаёт upload info, принимает чанки и complete.
type fakeBitrix struct {
	mu        sync.Mutex
	chunks    map[string]*bytes.Buffer
	lastParts map[string]string
	completed map[string]string
	failFirst atomic.Bool
}

func (b *fakeBitrix) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		// первый чанк отвечает 500, чтобы проверить, что повтор отправляет непустое тело
		if b.failFirst.CompareAndSwap(true, false) {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		file, _, err := r.FormFile("file")
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		data, _ := io.ReadAll(file)
		name := r.FormValue("file_name")

		b.mu.Lock()
		if b.chunks[name] == nil {
			b.chunks[name] = &bytes.Buffer{}
		}
		b.chunks[name].Write(data)
		b.lastParts[name] += r.FormValue("last_part")
		b.mu.Unlock()

		_, _ = w.Write([]byte(`{"success":"y"}`))
		return
	}

	_ = r.ParseForm()
	switch {
	case r.Form.Get("upload") == "where":
		_, _ = fmt.Fprintf(w, `{"name":"uploaded_%s","chunk_size":4}`, r.Form.Get("file_id"))
	case r.Form.Get("finish") == "y":
		b.mu.Lock()
		for k, v := range r.Form {
			b.completed[k] = v[0]
		}
		b.mu.Unlock()
		_, _ = w.Write([]byte(`{"success":"y"}`))
	default:
		http.Error(w, "unexpected request", http.StatusBadRequest)
	}
}

func TestUploadFilesAndComplete(t *testing.T) {
	goroutinesBefore := runtime.NumGoroutine()

	bitrix := &fakeBitrix{
		chunks:    map[string]*bytes.Buffer{},
		lastParts: map[string]string{},
		completed: map[string]string{},
	}
	bitrix.failFirst.Store(true)

	srv := httptest.NewServer(bitrix)

	dir := t.TempDir()
	files := map[string]string{
		"pdf": filepath.Join(dir, "a.pdf"),
		"jpg": filepath.Join(dir, "a.jpg"),
	}
	contents := map[string]string{
		"pdf": "0123456789",
		"jpg": "abc",
	}
	for format, path := range files {
		if err := os.WriteFile(path, []byte(contents[format]), 0644); err != nil {
			t.Fatal(err)
		}
	}

	client := NewHTTPClient(5 * time.Second)
	uploader := New(srv.URL, client)

	uploaded, err := uploader.UploadFiles(context.Background(), files, 2)
	if err != nil {
		t.Fatalf("UploadFiles: %v", err)
	}

	if err = uploader.Complete(context.Background(), uploaded); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	for format, want := range contents {
		name := "uploaded_" + format
		if got := bitrix.chunks[name].String(); got != want {
			t.Errorf("%s: reassembled %q, want %q", format, got, want)
		}
		if got := bitrix.completed["result[files]["+format+"]"]; got != name {
			t.Errorf("%s: completed with %q, want %q", format, got, name)
		}
	}

	// 10 байт по 4 - три чанка, последний с last_part=y
	if got := bitrix.lastParts["uploaded_pdf"]; got != "nny" {
		t.Errorf("pdf last_part sequence = %q, want nny", got)
	}

	client.CloseIdleConnections()
	srv.Close()

	// после закрытия соединений горутины транспорта должны завершиться
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > goroutinesBefore && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if after := runtime.NumGoroutine(); after > goroutinesBefore {
		t.Errorf("goroutine leak: before %d, after %d", goroutinesBefore, after)
	}
}

func TestUploadReturnsBitrixError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"error":"access denied"}`))
	}))
	defer srv.Close()

	client := NewHTTPClient(5 * time.Second)
	defer client.CloseIdleConnections()

	err := New(srv.URL, client).Complete(context.Background(), map[string]string{"pdf": "x"})
	if err == nil || !strings.Contains(err.Error(), "access denied") {
		t.Fatalf("want bitrix error, got %v", err)
	}
}
