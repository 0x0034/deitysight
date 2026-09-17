package agent

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"time"
)

const Version = "0.1.0"

type Request struct {
	RequestID     string `json:"request_id"`
	WindowSeconds *int64 `json:"window_seconds,omitempty"`
	StepSeconds   *int64 `json:"step_seconds,omitempty"`
}
type Result struct {
	Available bool   `json:"available"`
	Expired   bool   `json:"expired"`
	URL       string `json:"url,omitempty"`
	SHA256    string `json:"sha256,omitempty"`
	Size      int64  `json:"size,omitempty"`
}
type ErrorCount struct {
	Code  string `json:"code"`
	Count int    `json:"count"`
}
type Task struct {
	Host              map[string]any `json:"host"`
	TaskID            string         `json:"task_id"`
	RequestID         string         `json:"request_id"`
	State             string         `json:"state"`
	Phase             string         `json:"phase"`
	WindowSeconds     int64          `json:"window_seconds"`
	StepSeconds       int64          `json:"step_seconds"`
	PlannedPoints     int            `json:"planned_points"`
	SampledPoints     int            `json:"sampled_points"`
	MissedPoints      int            `json:"missed_points"`
	SourceRecords     int64          `json:"source_records"`
	ReceivedAt        time.Time      `json:"received_at"`
	StartedAt         *time.Time     `json:"started_at"`
	LastSampleAt      *time.Time     `json:"last_sample_at"`
	EndedAt           *time.Time     `json:"ended_at"`
	ResultExpiresAt   *time.Time     `json:"result_expires_at"`
	TaskExpiresAt     *time.Time     `json:"task_expires_at"`
	Result            Result         `json:"result"`
	Errors            []ErrorCount   `json:"errors"`
	BackgroundEnabled bool           `json:"background_enabled"`
	HistoryStartedAt  *time.Time     `json:"history_started_at"`
	HistoryEndedAt    *time.Time     `json:"history_ended_at"`
	HistoryRecords    int64          `json:"history_records"`
	HistoryReason     string         `json:"history_reason,omitempty"`
}

func (t Task) clone() Task {
	t.Errors = append([]ErrorCount{}, t.Errors...)
	host := map[string]any{}
	for k, v := range t.Host {
		host[k] = v
	}
	t.Host = host
	for _, p := range []**time.Time{&t.StartedAt, &t.LastSampleAt, &t.EndedAt, &t.ResultExpiresAt, &t.TaskExpiresAt, &t.HistoryStartedAt, &t.HistoryEndedAt} {
		if *p != nil {
			v := **p
			*p = &v
		}
	}
	return t
}
func (t *Task) addError(code string) {
	for i := range t.Errors {
		if t.Errors[i].Code == code {
			t.Errors[i].Count++
			return
		}
	}
	if len(t.Errors) < 64 {
		t.Errors = append(t.Errors, ErrorCount{code, 1})
	}
}

type Object struct {
	PID             int    `json:"pid,omitempty"`
	TID             int    `json:"tid,omitempty"`
	StartTime       string `json:"start_time_ticks,omitempty"`
	ThreadStartTime string `json:"thread_start_time_ticks,omitempty"`
	Cgroup          string `json:"cgroup,omitempty"`
	ContainerID     string `json:"container_id,omitempty"`
}
type Record struct {
	BootID        string    `json:"boot_id,omitempty"`
	SchemaVersion int       `json:"schema_version"`
	SampleID      string    `json:"sample_id"`
	Kind          string    `json:"kind"`
	Source        string    `json:"source"`
	Scope         string    `json:"scope,omitempty"`
	StartedAt     time.Time `json:"started_at"`
	FinishedAt    time.Time `json:"finished_at"`
	OffsetNS      int64     `json:"offset_ns"`
	Object        *Object   `json:"object,omitempty"`
	Encoding      string    `json:"content_encoding,omitempty"`
	Content       string    `json:"content,omitempty"`
	Complete      bool      `json:"complete"`
	Code          string    `json:"error_code,omitempty"`
}
type Collector interface {
	Collect(context.Context, func(Record) error) error
	Metadata() map[string]any
}
type APIError struct {
	Status        int
	Code, Message string
}

func (e *APIError) Error() string                     { return e.Message }
func apiError(status int, code, message string) error { return &APIError{status, code, message} }
func uuid() string {
	var b [16]byte
	if _, e := rand.Read(b[:]); e != nil {
		panic(e)
	}
	b[6] = (b[6] & 15) | 64
	b[8] = (b[8] & 63) | 128
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:])
}
func validID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
		} else if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
func jsonBytes(v any) ([]byte, error) { b, e := json.Marshal(v); return append(b, '\n'), e }
