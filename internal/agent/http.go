package agent

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func writeError(w http.ResponseWriter, err error) {
	var api *APIError
	if !errors.As(err, &api) {
		api = &APIError{500, "internal_error", "internal error"}
	}
	writeJSON(w, api.Status, map[string]any{"error": map[string]string{"code": api.Code, "message": api.Message}})
}
func parseRequest(b []byte) (Request, error) {
	var r Request
	bad := apiError(400, "invalid_request", "invalid request JSON")
	if !utf8.Valid(b) {
		return r, bad
	}
	d := json.NewDecoder(bytes.NewReader(b))
	tok, e := d.Token()
	if e != nil || tok != json.Delim('{') {
		return r, bad
	}
	seen := map[string]bool{}
	for d.More() {
		tok, e = d.Token()
		if e != nil {
			return r, bad
		}
		key, ok := tok.(string)
		if !ok || seen[key] {
			return r, bad
		}
		seen[key] = true
		var raw json.RawMessage
		if d.Decode(&raw) != nil || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return r, bad
		}
		switch key {
		case "scenes":
			if json.Unmarshal(raw, &r.Scenes) != nil {
				return r, bad
			}
		case "include_threads":
			var v bool
			if json.Unmarshal(raw, &v) != nil {
				return r, bad
			}
			r.IncludeThreads = &v
		case "request_id":
			if json.Unmarshal(raw, &r.RequestID) != nil {
				return r, bad
			}
		case "window_seconds", "step_seconds":
			var n int64
			if json.Unmarshal(raw, &n) != nil {
				return r, bad
			}
			if key == "window_seconds" {
				r.WindowSeconds = &n
			} else {
				r.StepSeconds = &n
			}
		default:
			return r, bad
		}
	}
	if tok, e = d.Token(); e != nil || tok != json.Delim('}') {
		return r, bad
	}
	if _, e = d.Token(); e != io.EOF {
		return r, bad
	}
	return r, nil
}
func (a *Agent) Handler() http.Handler {
	expected := sha256.Sum256([]byte("Bearer " + a.cfg.HTTP.Token))
	slots := make(chan struct{}, 32)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		supplied := sha256.Sum256([]byte(r.Header.Get("Authorization")))
		if len(r.Header.Values("Authorization")) != 1 || subtle.ConstantTimeCompare(supplied[:], expected[:]) != 1 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeError(w, apiError(401, "unauthorized", "authentication required"))
			return
		}
		select {
		case slots <- struct{}{}:
			defer func() { <-slots }()
		default:
			writeError(w, apiError(503, "agent_unavailable", "HTTP concurrency limit reached"))
			return
		}
		if r.URL.Path == "/v1/health" && r.Method == http.MethodGet {
			a.health(w)
			return
		}
		if r.URL.Path == "/v1/tasks" && r.Method == http.MethodPost {
			b, e := io.ReadAll(http.MaxBytesReader(w, r.Body, 8192))
			if e != nil {
				var tooLarge *http.MaxBytesError
				if errors.As(e, &tooLarge) {
					writeError(w, apiError(413, "request_too_large", "request exceeds 8 KiB"))
				} else {
					writeError(w, apiError(400, "invalid_request", "cannot read request"))
				}
				return
			}
			req, e := parseRequest(b)
			if e != nil {
				writeError(w, e)
				return
			}
			t, reused, e := a.Submit(req)
			if e != nil {
				writeError(w, e)
				return
			}
			status := 202
			if reused {
				status = 200
			}
			w.Header().Set("Location", "/v1/tasks/"+t.TaskID)
			writeJSON(w, status, struct {
				Task
				Reused bool `json:"reused"`
			}{t, reused})
			return
		}
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/v1/tasks/"), "/")
		if strings.HasPrefix(r.URL.Path, "/v1/tasks/") && validID(parts[0]) && r.Method == http.MethodGet {
			if len(parts) == 1 {
				t, e := a.Get(parts[0])
				if e != nil {
					writeError(w, e)
				} else {
					writeJSON(w, 200, t)
				}
				return
			}
			if len(parts) == 2 && parts[1] == "result" {
				a.download(w, r, parts[0])
				return
			}
		}
		writeError(w, apiError(404, "not_found", "endpoint not found"))
	})
}
func (a *Agent) health(w http.ResponseWriter) {
	a.mu.Lock()
	defer a.mu.Unlock()
	storageOK := a.store.Available() == nil
	stalled := !a.roundDeadline.IsZero() && time.Now().After(a.roundDeadline)
	status := 200
	atopOK := true
	if c, ok := a.collector.(windowCollector); ok {
		atopOK = c.Available() == nil
	}
	if !atopOK {
		status = 503
	}
	if a.degraded || stalled || a.ctx.Err() != nil {
		status = 503
	}
	writeJSON(w, status, map[string]any{"atop_available": atopOK, "agent_id": a.id, "version": Version, "recovery_ok": !a.degraded, "storage_available": storageOK, "collector_stalled": stalled, "active_task_id": a.active, "background_enabled": a.cfg.Background.Enabled, "background_paused": a.paused || !storageOK, "accepting_tasks": status == 200 && storageOK && a.active == ""})
}
func (a *Agent) download(w http.ResponseWriter, r *http.Request, id string) {
	a.mu.Lock()
	t, e := a.getLocked(id)
	if e == nil {
		switch {
		case t.Result.Expired:
			e = apiError(410, "result_expired", "result expired")
		case t.State == "running":
			e = apiError(409, "result_not_ready", "task is still running")
		case !t.Result.Available:
			e = apiError(409, "result_unavailable", "result unavailable")
		}
	}
	if e != nil {
		a.mu.Unlock()
		writeError(w, e)
		return
	}
	f, e := a.store.Lease(taskPath(id, "result.tar.gz"))
	a.mu.Unlock()
	if e != nil {
		writeError(w, apiError(409, "result_unavailable", "result unavailable"))
		return
	}
	defer f.Close()
	n, sum, e := digest(f)
	if e != nil || n != t.Result.Size || sum != t.Result.SHA256 {
		a.mu.Lock()
		current, ok := a.tasks[id]
		if ok {
			current.Result.Available = false
			current.addError("result_corrupt_or_missing")
			a.tasks[id] = current
			_ = a.persist(current)
		}
		a.mu.Unlock()
		writeError(w, apiError(409, "result_unavailable", "result integrity check failed"))
		return
	}
	if _, e = f.Seek(0, io.SeekStart); e != nil {
		writeError(w, apiError(409, "result_unavailable", "cannot read result"))
		return
	}
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+id+`.tar.gz"`)
	w.Header().Set("Content-Length", strconv.FormatInt(t.Result.Size, 10))
	w.Header().Set("ETag", `"`+t.Result.SHA256+`"`)
	_, _ = io.Copy(w, f)
}
