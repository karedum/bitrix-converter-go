package command

import (
	"bitrix-converter/internal/lib/fileuploader"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type fakeConverter struct {
	dir     string
	convErr error
	created []string
}

func (f *fakeConverter) Validate(ConvertTask) error { return nil }
func (f *fakeConverter) MaxSize() int64             { return 1 << 20 }
func (f *fakeConverter) DownloadDir() string        { return f.dir }

func (f *fakeConverter) Convert(_ context.Context, src string, formats []string, cleanup *Cleanup) (map[string]string, error) {
	files := map[string]string{}
	for _, format := range formats {
		out := src + "." + format
		cleanup.Add(out)
		f.created = append(f.created, out)
		if err := os.WriteFile(out, []byte(format), 0644); err != nil {
			return nil, err
		}
		files[format] = out
	}
	return files, f.convErr
}

func newFakeServer(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/file":
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte("source"))
		case r.FormValue("upload") == "where":
			_, _ = w.Write([]byte(`{"name":"n","chunk_size":1024}`))
		default:
			_, _ = w.Write([]byte(`{"success":"y"}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestExecuteRemovesTemporaryFiles(t *testing.T) {
	for _, convErr := range []error{nil, errors.New("convert failed")} {
		srv := newFakeServer(t)
		client := fileuploader.NewHTTPClient(5 * time.Second)
		conv := &fakeConverter{dir: t.TempDir(), convErr: convErr}

		task := ConvertTask{File: srv.URL + "/file", BackUrl: srv.URL + "/back", Formats: []string{"pdf", "jpg"}}

		err := Execute(context.Background(), conv, task, fileuploader.New(task.BackUrl, client), 2)
		if (err != nil) != (convErr != nil) {
			t.Fatalf("Execute error = %v, want error %v", err, convErr)
		}

		entries, _ := os.ReadDir(conv.dir)
		if len(entries) != 0 {
			t.Errorf("temporary files left in %s: %d", conv.dir, len(entries))
		}
		for _, f := range conv.created {
			if _, err := os.Stat(f); !os.IsNotExist(err) {
				t.Errorf("converted file not removed: %s", filepath.Base(f))
			}
		}
		client.CloseIdleConnections()
	}
}
