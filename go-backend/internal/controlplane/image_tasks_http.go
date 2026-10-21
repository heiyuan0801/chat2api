package controlplane

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"
)

func imageTaskHTTPError(err error) error {
	var full *ImageTaskQueueFullError
	if errors.As(err, &full) {
		return &detailedProblem{503, Map{"code": "image_task_queue_full", "error": full.Error()}}
	}
	return err
}

func (s *Server) imageTaskRoute(method, path, model string, fn func(context.Context, http.ResponseWriter, *http.Request, Map, Map) (Map, error)) {
	s.routes = append(s.routes, registeredRoute{Route{method, path, "identity", model}, func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), time.Hour)
		defer cancel()
		defer func() {
			if recover() != nil {
				writeError(w, problem(500, "Internal Server Error"))
			}
		}()
		body, err := decodePublic(w, r, model)
		if err != nil {
			writeError(w, err)
			return
		}
		who, err := s.Auth.Identity(ctx, r.Header.Get("Authorization"))
		if err != nil {
			writeError(w, err)
			return
		}
		value, err := fn(ctx, w, r, body, who)
		if err != nil {
			writeError(w, imageTaskHTTPError(err))
			return
		}
		writeJSON(w, 200, value)
	}})
}
func (s *Server) registerImageTasks() {
	s.imageTaskRoute("GET", "/api/image-tasks", "", func(ctx context.Context, w http.ResponseWriter, r *http.Request, body, who Map) (Map, error) {
		return s.Tasks.List(who, strings.Split(r.URL.Query().Get("ids"), ","))
	})
	s.imageTaskRoute("POST", "/api/image-tasks/generations", "ImageGenerationTaskRequest", func(ctx context.Context, w http.ResponseWriter, r *http.Request, body, who Map) (Map, error) {
		if err := s.checkContent(ctx, body["prompt"]); err != nil {
			call := s.beginPublicCall(ctx, "/api/image-tasks/generations", body, who)
			if call == nil {
				return nil, problem(503, "server is shutting down")
			}
			call.summary = "文生图任务"
			_ = call.Finish(nil, err)
			return nil, err
		}
		body["base_url"] = s.resolveImageBaseURL(r)
		return s.Tasks.SubmitGeneration(who, body)
	})
	s.imageTaskRoute("POST", "/api/image-tasks/edits", "", func(ctx context.Context, w http.ResponseWriter, r *http.Request, body, who Map) (Map, error) {
		reservation, err := s.Tasks.Reserve()
		if err != nil {
			return nil, err
		}
		defer reservation.Rollback()
		payload, images, masks, err := s.parseImageEdit(ctx, w, r)
		if err != nil {
			return nil, err
		}
		if text(payload["client_task_id"]) == "" {
			return nil, problem(400, "client_task_id is required")
		}
		if err = s.checkContent(ctx, payload["prompt"]); err != nil {
			call := s.beginPublicCall(ctx, "/api/image-tasks/edits", payload, who)
			if call == nil {
				return nil, problem(503, "server is shutting down")
			}
			call.summary = "图生图任务"
			_ = call.Finish(nil, err)
			return nil, err
		}
		payload["base_url"] = s.resolveImageBaseURL(r)
		return s.Tasks.SubmitEdit(who, payload, images, masks, reservation)
	})
	s.imageTaskRoute("POST", "/api/image-tasks/{task_id}/resume-poll", "ResumePollRequest", func(ctx context.Context, w http.ResponseWriter, r *http.Request, body, who Map) (Map, error) {
		seconds, _ := number(body["extra_timeout_secs"])
		return s.Tasks.Resume(who, r.PathValue("task_id"), time.Duration(seconds*float64(time.Second)))
	})
}
