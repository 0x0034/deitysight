package agent

import (
	"context"
	"errors"
	"net"
	"net/url"
	"os"
	"path"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

type S3Config struct {
	Enabled         bool          `yaml:"enabled"`
	Endpoint        string        `yaml:"endpoint"`
	Region          string        `yaml:"region"`
	Bucket          string        `yaml:"bucket"`
	Prefix          string        `yaml:"prefix"`
	ForcePathStyle  bool          `yaml:"force_path_style"`
	AccessKeyID     string        `yaml:"access_key_id" json:"-"`
	SecretAccessKey string        `yaml:"secret_access_key" json:"-"`
	SessionToken    string        `yaml:"session_token" json:"-"`
	PresignTTL      time.Duration `yaml:"presign_ttl"`
	UploadTimeout   time.Duration `yaml:"upload_timeout"`
	RetryInitial    time.Duration `yaml:"retry_initial"`
	RetryMax        time.Duration `yaml:"retry_max"`
}
type S3Target struct {
	Endpoint       string `json:"endpoint"`
	Region         string `json:"region"`
	Bucket         string `json:"bucket"`
	Prefix         string `json:"prefix"`
	ForcePathStyle bool   `json:"force_path_style"`
}
type S3Result struct {
	S3Target
	State         string     `json:"state"`
	Key           string     `json:"key,omitempty"`
	SHA256        string     `json:"sha256,omitempty"`
	Size          int64      `json:"size,omitempty"`
	Attempts      int        `json:"attempts"`
	UploadID      string     `json:"upload_id,omitempty"`
	UploadedAt    *time.Time `json:"uploaded_at,omitempty"`
	NextAttemptAt *time.Time `json:"next_attempt_at,omitempty"`
	LastErrorCode string     `json:"last_error_code,omitempty"`
	// These fields are response-only and are cleared by persist.
	Paused       bool       `json:"paused"`
	URL          string     `json:"url,omitempty"`
	URLExpiresAt *time.Time `json:"url_expires_at,omitempty"`
	URLErrorCode string     `json:"url_error_code,omitempty"`
}

func (s *S3Result) clone() *S3Result {
	if s == nil {
		return nil
	}
	v := *s
	for _, p := range []**time.Time{&v.UploadedAt, &v.NextAttemptAt, &v.URLExpiresAt} {
		if *p != nil {
			copy := **p
			*p = &copy
		}
	}
	return &v
}
func (c S3Config) target() S3Target {
	return S3Target{strings.TrimSuffix(c.Endpoint, "/"), c.Region, c.Bucket, c.Prefix, c.ForcePathStyle}
}

var s3BucketPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)
var s3RegionPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

func (t S3Target) valid() bool {
	u, e := url.Parse(t.Endpoint)
	if e != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawPath != "" || (u.Path != "" && u.Path != "/") {
		return false
	}
	if !s3BucketPattern.MatchString(t.Bucket) || strings.Contains(t.Bucket, "..") || strings.Contains(t.Bucket, ".-") || strings.Contains(t.Bucket, "-.") || net.ParseIP(t.Bucket) != nil || !s3RegionPattern.MatchString(t.Region) {
		return false
	}
	p := t.Prefix
	if len(p) > 512 || !utf8.ValidString(p) || strings.ContainsAny(p, "\\?#") || strings.IndexFunc(p, unicode.IsControl) >= 0 {
		return false
	}
	if p != "" && (path.Clean(p) != p || strings.HasPrefix(p, "/") || p == "." || p == ".." || strings.HasPrefix(p, "../")) {
		return false
	}
	return true
}
func (c S3Config) validate() error {
	if !c.target().valid() {
		return errors.New("invalid S3 endpoint, region, bucket or prefix")
	}
	for _, s := range []string{c.AccessKeyID, c.SecretAccessKey} {
		if s == "" || strings.Contains(s, "REPLACE_WITH") || strings.TrimSpace(s) != s || strings.IndexFunc(s, unicode.IsControl) >= 0 {
			return errors.New("configure valid S3 credentials")
		}
	}
	if strings.TrimSpace(c.SessionToken) != c.SessionToken || strings.IndexFunc(c.SessionToken, unicode.IsControl) >= 0 {
		return errors.New("invalid S3 session token")
	}
	if c.PresignTTL < time.Second || c.PresignTTL > 7*24*time.Hour || c.PresignTTL%time.Second != 0 || c.UploadTimeout <= 0 || c.RetryInitial <= 0 || c.RetryMax < c.RetryInitial {
		return errors.New("invalid S3 timeouts or retry policy")
	}
	return nil
}
func (s *S3Result) valid(t Task, agentID string) bool {
	if !s.S3Target.valid() {
		return false
	}
	switch s.State {
	case "waiting_result", "pending", "uploading", "retry_wait", "uploaded", "expired", "unavailable":
	default:
		return false
	}
	if s.Attempts < 0 || s.Size < 0 {
		return false
	}
	if (s.State == "pending" || s.State == "uploading" || s.State == "retry_wait" || s.State == "uploaded") && s.Key == "" {
		return false
	}
	if s.Key != "" {
		if len(s.SHA256) != 64 || strings.IndexFunc(s.SHA256, func(r rune) bool { return !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') }) >= 0 {
			return false
		}
		if s.Key != path.Join(s.Prefix, agentID, t.TaskID, s.SHA256+".tar.gz") {
			return false
		}
		if s.Size != t.Result.Size || s.SHA256 != t.Result.SHA256 {
			return false
		}
	}
	return s.State != "uploaded" || (s.Key != "" && s.UploadedAt != nil)
}

var errObjectMissing = errors.New("remote object missing")
var errObjectConflict = errors.New("remote object conflict")
var errRemoteIntegrity = errors.New("remote integrity mismatch")

type remoteObject struct {
	Size                    int64
	SHA256, AgentID, TaskID string
}
type uploadObject struct {
	Key, AgentID, TaskID, SHA256 string
	Size                         int64
	UploadID                     string
}

func (o uploadObject) matches(r remoteObject) bool {
	return r.Size == o.Size && r.SHA256 == o.SHA256 && r.AgentID == o.AgentID && r.TaskID == o.TaskID
}

type remoteStore interface {
	Head(context.Context, string) (remoteObject, error)
	Put(context.Context, uploadObject, *os.File, func(string) error) error
	Abort(context.Context, string, string) error
	Presign(context.Context, string, time.Duration) (string, error)
}
