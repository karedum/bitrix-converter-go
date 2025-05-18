package convert

import (
	resp "bitrix-converter/internal/lib/api/response"
	"bitrix-converter/internal/lib/command"
	"bitrix-converter/internal/lib/logger/sl"
	"bitrix-converter/internal/lib/rabbitmq"
	"context"
	"encoding/json"
	"fmt"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/render"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
)

func New(ctx context.Context, log *slog.Logger, rabbit *rabbitmq.Rabbit) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {

		const op = "handlers.convert.New"

		reqId := middleware.GetReqID(r.Context())

		log = log.With(
			slog.String("op", op),
			slog.String("request_id", reqId),
		)

		err := r.ParseForm()

		if err != nil {
			log.Error("failed to decode request body", sl.Err(err))
			render.JSON(w, r, resp.Error("failed to decode request body", 152))
		}

		task, err := prepareOptions(r.Form, reqId)

		if err != nil {
			log.Error("failed to prepare options", sl.Err(err))
			render.JSON(w, r, resp.Error("failed to parse task", 0))
			return
		}

		if task.Queue == "" {
			task.Queue = rabbit.DefaultQueue()
			log.Warn("not found queue. Set default", slog.String("default_queue", task.Queue))
		}

		taskMsg, err := json.Marshal(task)

		if err != nil {
			log.Error("Error parse request", "error", err.Error())

			render.JSON(w, r, resp.Error("Error parse request", 0))

			return
		}

		err = rabbit.Publish(task.Queue, taskMsg)
		if err != nil {
			log.Error("error publish task", slog.String("queue", task.Queue), sl.Err(err))
			render.JSON(w, r, resp.Error("error publish task", 0))
		}
		render.JSON(w, r, resp.Success())
	}
}

func parseFormats(form url.Values) ([]string, error) {
	var result []string

	for key, values := range form {
		reg, err := regexp.Compile("params\\[(formats)\\]\\[([a-zA-Z0-9]*)\\]")
		if err != nil {
			return result, fmt.Errorf("error parsing formats: [%w]", err)
		}
		matches := reg.FindStringSubmatch(key)

		if len(matches) > 0 {
			result = append(result, values[0])
		}
	}
	return result, nil
}

func prepareOptions(postForm url.Values, reqId string) (command.ConvertTask, error) {
	task := command.ConvertTask{}
	task.Command = postForm.Get("command")
	task.Queue = postForm.Get("QUEUE")
	task.Id = postForm.Get("params[id]")
	task.BackUrl = postForm.Get("params[back_url]")
	task.File = postForm.Get("params[file]")
	task.RequestID = reqId
	var err error
	if fileId := postForm.Get("params[file_id]"); fileId != "" {
		taskFileId, err := strconv.Atoi(fileId)
		if err != nil {
			return task, fmt.Errorf("error prepareOptions - convert Atoi: %w", err)
		}
		task.FileId = taskFileId
	}

	if fileSize := postForm.Get("params[fileSize]"); fileSize != "" {
		taskFileSize, err := strconv.ParseInt(fileSize, 10, 64)
		if err != nil {
			return task, fmt.Errorf("error prepareOptions - ParseInt: %w", err)
		}
		task.FileSize = taskFileSize
	}

	task.Formats, err = parseFormats(postForm)

	if err != nil {
		return task, err
	}
	return task, nil
}
