package convert

import (
	"bitrix-converter/internal/lib/command"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
)

type fakePublisher struct {
	err     error
	queue   string
	message []byte
}

func (p *fakePublisher) Publish(_ context.Context, queue string, message []byte) error {
	p.queue = queue
	p.message = message
	return p.err
}

func (p *fakePublisher) DefaultQueue() string { return "main_preview" }

func doRequest(t *testing.T, publisher Publisher, form url.Values) []map[string]any {
	t.Helper()

	req := httptest.NewRequest(http.MethodPost, "/convert", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()

	New(slog.New(slog.NewTextHandler(io.Discard, nil)), publisher).ServeHTTP(rec, req)

	var responses []map[string]any
	dec := json.NewDecoder(rec.Body)
	for {
		var r map[string]any
		if err := dec.Decode(&r); err != nil {
			break
		}
		responses = append(responses, r)
	}
	return responses
}

func TestHandlerPublishesTask(t *testing.T) {
	publisher := &fakePublisher{}

	form := url.Values{
		"command":            {command.DocumentCommand},
		"params[file]":       {"http://example.com/file.docx"},
		"params[back_url]":   {"http://example.com/back"},
		"params[file_id]":    {"42"},
		"params[formats][0]": {"pdf"},
		"params[formats][1]": {"jpg"},
	}

	responses := doRequest(t, publisher, form)

	if len(responses) != 1 || responses[0]["success"] != true {
		t.Fatalf("want one success response, got %v", responses)
	}
	if publisher.queue != "main_preview" {
		t.Errorf("published to %q, want default queue", publisher.queue)
	}

	var task command.ConvertTask
	if err := json.Unmarshal(publisher.message, &task); err != nil {
		t.Fatal(err)
	}
	slices.Sort(task.Formats)
	if task.FileId != 42 || !slices.Equal(task.Formats, []string{"jpg", "pdf"}) {
		t.Errorf("unexpected task %+v", task)
	}
}

func TestHandlerWritesSingleResponseOnPublishError(t *testing.T) {
	responses := doRequest(t, &fakePublisher{err: errors.New("rabbit down")}, url.Values{"command": {"x"}})

	if len(responses) != 1 {
		t.Fatalf("want exactly one response, got %d: %v", len(responses), responses)
	}
	if responses[0]["success"] != false {
		t.Errorf("want error response, got %v", responses[0])
	}
}

func TestHandlerRejectsInvalidFileId(t *testing.T) {
	publisher := &fakePublisher{}
	responses := doRequest(t, publisher, url.Values{"params[file_id]": {"abc"}})

	if len(responses) != 1 || responses[0]["success"] != false {
		t.Fatalf("want one error response, got %v", responses)
	}
	if publisher.message != nil {
		t.Error("task must not be published")
	}
}
