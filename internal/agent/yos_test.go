package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// The fixture deliberately implements the observed YOS differences: no HEAD
// metadata, unconditional PUT, and an XML endpoint for COS download links.
type yosFixture struct {
	mu              sync.Mutex
	objects         map[string][]byte
	puts            int
	corruptAfterPut bool
	signReply       string
}

func startYOSFixture(t *testing.T) (Config, *yosFixture) {
	t.Helper()
	f := &yosFixture{objects: make(map[string][]byte)}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if !strings.HasPrefix(r.URL.Path, "/fixture/qa/") || r.Header.Get("Authorization") != "" {
			t.Error("unexpected namespace or leaked credentials")
			w.WriteHeader(400)
			return
		}
		if r.URL.Query().Get("presign") == "true" {
			if f.signReply != "" {
				_, _ = io.WriteString(w, f.signReply)
				return
			}
			if r.URL.Query().Get("cloud") != "cos" {
				t.Error("wrong cloud")
			}
			expire, _ := strconv.ParseInt(r.URL.Query().Get("expire"), 10, 64)
			link := "https://fixture.cos.ap-beijing.myqcloud.com" + r.URL.Path + "?q-signature=fixture-signature&q-sign-time=" + strconv.FormatInt(time.Now().Unix()-1, 10) + ";" + strconv.FormatInt(expire, 10)
			_, _ = io.WriteString(w, "<Data><Code>OK</Code><Message>")
			_ = xml.EscapeText(w, []byte(link))
			_, _ = io.WriteString(w, "</Message></Data>")
			return
		}
		switch r.Method {
		case "GET", "HEAD":
			b, ok := f.objects[r.URL.Path]
			if !ok {
				w.WriteHeader(404)
				return
			}
			w.Header().Set("Content-Length", strconv.Itoa(len(b)))
			if r.Method == "GET" {
				_, _ = w.Write(b)
			}
		case "PUT":
			if r.ContentLength < 0 {
				t.Error("chunked upload")
			}
			b, err := io.ReadAll(io.LimitReader(r.Body, 2<<20))
			if err != nil {
				t.Error(err)
			}
			f.objects[r.URL.Path] = b
			if f.corruptAfterPut && len(b) > 0 {
				b[0] ^= 1
			}
			f.puts++
		default:
			t.Error("unexpected YOS operation", r.Method)
			w.WriteHeader(405)
		}
	}))
	t.Cleanup(srv.Close)
	c := transferConfig(t)
	c.S3.Provider = "yos"
	c.S3.AllowHTTP = true
	c.S3.Endpoint = srv.URL
	c.S3.Bucket = "fixture/qa"
	c.S3.Region, c.S3.AccessKeyID, c.S3.SecretAccessKey = "", "", ""
	return c, f
}

func TestYOSConfigurationBoundaries(t *testing.T) {
	c := testConfig(t)
	c.S3.Enabled, c.S3.Provider, c.S3.AllowHTTP = true, "yos", true
	c.S3.Endpoint, c.S3.Bucket = "http://yos.example.test", "fixture/qa"
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*S3Config){
		"http_requires_opt_in":    func(c *S3Config) { c.AllowHTTP = false },
		"unknown_provider":        func(c *S3Config) { c.Provider = "other" },
		"namespace_traversal":     func(c *S3Config) { c.Bucket = "fixture/../qa" },
		"namespace_encoded_slash": func(c *S3Config) { c.Bucket = "fixture%2fqa" },
		"namespace_empty_segment": func(c *S3Config) { c.Bucket = "fixture//qa" },
		"namespace_query":         func(c *S3Config) { c.Bucket = "fixture/qa?x=y" },
		"prefix_over_budget":      func(c *S3Config) { c.Prefix = strings.Repeat("a", 64) },
		"virtual_host":            func(c *S3Config) { c.ForcePathStyle = false },
		"credentials":             func(c *S3Config) { c.AccessKeyID = "do-not-send" },
		"session_token":           func(c *S3Config) { c.SessionToken = "do-not-send" },
		"region":                  func(c *S3Config) { c.Region = "us-east-1" },
		"endpoint_query":          func(c *S3Config) { c.Endpoint += "?secret=x" },
	} {
		t.Run(name, func(t *testing.T) {
			changed := c
			mutate(&changed.S3)
			if changed.Validate() == nil {
				t.Fatal("invalid YOS configuration accepted")
			}
		})
	}
	s3 := transferConfig(t)
	before := s3.S3.target()
	s3.S3.Provider = "s3"
	if s3.Validate() != nil || s3.S3.target() != before {
		t.Fatal("explicit S3 provider changed legacy identity")
	}
	s3.S3.AllowHTTP = true
	if s3.Validate() == nil {
		t.Fatal("HTTP exception escaped YOS provider")
	}
	c.S3.AllowHTTP, c.S3.Endpoint = false, "https://yos.example.test"
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestYOSCorruptUploadIsNeverPublishedOrOverwritten(t *testing.T) {
	c, f := startYOSFixture(t)
	f.mu.Lock()
	f.corruptAfterPut = true
	f.mu.Unlock()
	a := transferAgent(t, c, fixtureCollector{}, nil)
	task, _, err := a.Submit(Request{RequestID: "corrupt-yos"})
	if err != nil {
		t.Fatal(err)
	}
	got := waitTransfer(t, a, task.TaskID, "retry_wait")
	if got.Result.S3.URL != "" || !got.Result.Available || got.State != "completed" {
		t.Fatal("corrupt remote changed local task or produced a link")
	}
	if got.Result.S3.LastErrorCode != "s3_integrity_mismatch" && got.Result.S3.LastErrorCode != "s3_object_conflict" {
		t.Fatal(got.Result.S3.LastErrorCode)
	}
	a.Close()
	b := transferAgent(t, c, fixtureCollector{}, nil)
	previousAttempts := got.Result.S3.Attempts
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		got, err = b.Get(task.TaskID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Result.S3.Attempts > previousAttempts && got.Result.S3.State == "retry_wait" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got.Result.S3.URL != "" || got.Result.S3.Attempts <= previousAttempts || got.Result.S3.LastErrorCode != "s3_object_conflict" {
		t.Fatal("restart failed to reject existing corrupt object")
	}
	b.Close()
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.puts != 1 {
		t.Fatal("existing mismatched object overwritten")
	}
}

func TestYOSRemoteProtocolFailures(t *testing.T) {
	data := []byte("synthetic archive")
	sum := sha256.Sum256(data)
	o := uploadObject{Key: "result.tar.gz", Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:])}
	for _, tc := range []struct {
		name   string
		status int
		body   string
		code   string
	}{
		{"missing", 404, "", "s3_request_failed"},
		{"forbidden", 403, "SECRET_PROVIDER_ERROR", "s3_access_denied"},
		{"rate_limit", 429, "", "s3_throttled"},
		{"unavailable", 503, "", "s3_service_unavailable"},
		{"redirect", 307, "", "s3_request_failed"},
		{"same_length_tamper", 200, "Synthetic archive", "s3_object_conflict"},
		{"truncated", 200, "short", "s3_object_conflict"},
		{"oversized", 200, strings.Repeat("x", 100), "s3_object_conflict"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Location", "http://127.0.0.1:1/private")
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer srv.Close()
			client := newYOSClient(S3Config{Endpoint: srv.URL, Bucket: "fixture/qa"}, nil)
			err := client.Check(context.Background(), o)
			if tc.status == 404 {
				if !errors.Is(err, errObjectMissing) {
					t.Fatal("missing not recognized")
				}
				return
			}
			if err == nil || remoteErrorCode(err) != tc.code || strings.Contains(err.Error(), "SECRET") {
				t.Fatal("wrong or unsafe error", remoteErrorCode(err))
			}
		})
	}
	t.Run("cancelled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		client := newYOSClient(S3Config{Endpoint: "http://127.0.0.1:1", Bucket: "fixture/qa"}, nil)
		if err := client.Check(ctx, o); !errors.Is(err, context.Canceled) {
			t.Fatal("cancellation lost")
		}
	})
}

func TestYOSSigningValidatesGatewayReply(t *testing.T) {
	for _, mode := range []string{"valid", "clock_skew", "http", "foreign_host", "wrong_path", "user_info", "port", "fragment", "extended_expiry", "expired", "future_start", "duplicate_signature", "missing_signature", "malformed_xml", "oversized_xml", "provider_error"} {
		t.Run(mode, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if mode == "malformed_xml" {
					_, _ = io.WriteString(w, "<Data>")
					return
				}
				if mode == "oversized_xml" {
					_, _ = io.WriteString(w, strings.Repeat("x", (16<<10)+1))
					return
				}
				if mode == "provider_error" {
					_, _ = io.WriteString(w, "<Data><Code>Error</Code><Message>DO_NOT_LEAK</Message></Data>")
					return
				}
				expire, _ := strconv.ParseInt(r.URL.Query().Get("expire"), 10, 64)
				start := time.Now().Unix() - 1
				switch mode {
				case "extended_expiry":
					expire += 3600
				case "expired":
					expire = time.Now().Unix() - 10
				case "future_start":
					start += 300
				case "clock_skew":
					start += 3
				}
				u := &url.URL{Scheme: "https", Host: "bucket.cos.ap-beijing.myqcloud.com", Path: r.URL.Path}
				q := url.Values{"q-signature": {"private-signature"}, "q-sign-time": {strconv.FormatInt(start, 10) + ";" + strconv.FormatInt(expire, 10)}}
				switch mode {
				case "http":
					u.Scheme = "http"
				case "foreign_host":
					u.Host += ".example.test"
				case "wrong_path":
					u.Path = "/other/object"
				case "user_info":
					u.User = url.User("private")
				case "port":
					u.Host += ":444"
				case "fragment":
					u.Fragment = "secret"
				case "duplicate_signature":
					q.Add("q-signature", "extra")
				case "missing_signature":
					q.Del("q-signature")
				}
				u.RawQuery = q.Encode()
				_, _ = io.WriteString(w, "<Data><Code>OK</Code><Message>")
				_ = xml.EscapeText(w, []byte(u.String()))
				_, _ = io.WriteString(w, "</Message></Data>")
			}))
			defer srv.Close()
			client := newYOSClient(S3Config{Endpoint: srv.URL, Bucket: "fixture/qa"}, nil)
			link, err := client.Presign(context.Background(), "result.tar.gz", time.Hour)
			if mode == "valid" || mode == "clock_skew" {
				if err != nil || link == "" {
					t.Fatal("valid link rejected")
				}
				return
			}
			if err == nil || link != "" || strings.Contains(err.Error(), "private-signature") || strings.Contains(err.Error(), "DO_NOT_LEAK") {
				t.Fatal("unsafe signing response accepted or exposed")
			}
		})
	}
}

func TestYOSSigningFailureAndTargetChange(t *testing.T) {
	c, f := startYOSFixture(t)
	f.mu.Lock()
	f.signReply = "<Data><Code>Error</Code><Message>PRIVATE_ERROR</Message></Data>"
	f.mu.Unlock()
	a := transferAgent(t, c, fixtureCollector{}, nil)
	task, _, err := a.Submit(Request{RequestID: "yos-sign-failure"})
	if err != nil {
		t.Fatal(err)
	}
	got := waitTransfer(t, a, task.TaskID, "uploaded")
	if got.Result.S3.URL != "" || got.Result.S3.URLErrorCode != "signing_failed" || !got.Result.Available {
		t.Fatal("signing failure broke result contract")
	}
	a.Close()
	changed := c
	changed.S3.Bucket = "fixture/other"
	b := transferAgent(t, changed, fixtureCollector{}, nil)
	got, err = b.Get(task.TaskID)
	if err != nil || got.Result.S3.URL != "" || got.Result.S3.URLErrorCode != "s3_target_changed" {
		t.Fatal("YOS target migrated silently")
	}
	b.Close()
	// Uploaded results can still be signed after local expiration and disabling uploads.
	f.mu.Lock()
	f.signReply = ""
	f.mu.Unlock()
	c.S3.Enabled = false
	d := transferAgent(t, c, fixtureCollector{}, nil)
	d.mu.Lock()
	v := d.tasks[task.TaskID].clone()
	past := time.Now().Add(-time.Second)
	v.ResultExpiresAt = &past
	d.tasks[task.TaskID] = v
	d.mu.Unlock()
	d.cleanup()
	got, err = d.Get(task.TaskID)
	if err != nil || got.Result.Available || got.Result.S3.URL == "" {
		t.Fatal("remote link lost after local expiry")
	}
}

func TestYOSSizeLimitsAndTLS(t *testing.T) {
	c, _ := startYOSFixture(t)
	client := newYOSClient(c.S3, nil)
	f, err := os.CreateTemp(t.TempDir(), "archive")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	o := uploadObject{Key: "result.tar.gz", Size: yosMaxObjectSize + 1}
	if client.Put(context.Background(), o, f, func(string) error { t.Fatal("multipart not allowed"); return nil }) == nil {
		t.Fatal("oversized object accepted")
	}
	if client.Check(context.Background(), o) == nil {
		t.Fatal("oversized verification accepted")
	}
	if client.Abort(context.Background(), "result.tar.gz", "unexpected-upload") == nil {
		t.Fatal("multipart checkpoint accepted")
	}
	if client.Abort(context.Background(), "result.tar.gz", "") != nil {
		t.Fatal("empty cleanup failed")
	}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("untrusted TLS request reached handler") }))
	defer srv.Close()
	c.S3.Endpoint = srv.URL
	untrusted := newYOSClient(c.S3, nil)
	_, err = untrusted.Presign(context.Background(), "result.tar.gz", time.Hour)
	if err == nil {
		t.Fatal("untrusted certificate accepted")
	}
}

type yosBoundaryTransport struct {
	t     *testing.T
	size  int64
	calls int
}

func (r *yosBoundaryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r.calls++
	defer req.Body.Close()
	if req.Method != http.MethodPut || req.ContentLength != r.size || req.URL.RawQuery != "" {
		r.t.Error("expected one fixed-length PUT without multipart")
	}
	// This is an admission check, not a 1 GB transfer or gateway capacity test.
	return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("")), Request: req}, nil
}

func TestYOSExactOneGBBoundary(t *testing.T) {
	for _, tc := range []struct {
		size        int64
		state, code string
		requests    int
	}{
		{999_999_999, "pending", "", 1},
		{1_000_000_000, "pending", "", 1},
		{1_000_000_001, "unavailable", "yos_object_too_large", 0},
	} {
		t.Run(strconv.FormatInt(tc.size, 10), func(t *testing.T) {
			cfg := testConfig(t)
			cfg.S3.Enabled, cfg.S3.Provider, cfg.S3.AllowHTTP = true, "yos", true
			cfg.S3.Endpoint, cfg.S3.Bucket = "http://yos.example.test", "fixture/qa"
			transport := &yosBoundaryTransport{t: t, size: tc.size}
			client := newYOSClient(cfg.S3, &http.Client{Transport: transport})
			f, err := os.CreateTemp(t.TempDir(), "sparse-archive")
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			if err = f.Truncate(tc.size); err != nil {
				t.Fatal(err)
			}
			err = client.Put(context.Background(), uploadObject{Key: "boundary.tar.gz", Size: tc.size}, f, func(string) error { t.Error("YOS used multipart"); return nil })
			if (err == nil) != (tc.requests == 1) || transport.calls != tc.requests {
				t.Fatalf("boundary request count=%d error=%v", transport.calls, err)
			}

			future := time.Now().Add(time.Hour)
			task := Task{TaskID: uuid(), State: "completed", ResultExpiresAt: &future, TaskExpiresAt: &future,
				Result: Result{Available: true, URL: "/v1/tasks/local/result", Size: tc.size, SHA256: strings.Repeat("a", 64), S3: &S3Result{S3Target: cfg.S3.target(), State: "waiting_result"}}}
			a := &Agent{cfg: cfg, id: uuid(), remote: client, tasks: make(map[string]Task)}
			a.prepareTransfer(&task)
			a.tasks[task.TaskID] = task
			view, err := a.Get(task.TaskID)
			if err != nil || view.State != "completed" || !view.Result.Available || view.Result.URL != task.Result.URL {
				t.Fatal("size gate changed local result")
			}
			s := view.Result.S3
			if s.State != tc.state || s.LastErrorCode != tc.code || s.URL != "" || s.Attempts != 0 {
				t.Fatalf("unexpected transfer state: %+v", s)
			}
			_, selected := a.nextUpload()
			if selected != (tc.requests == 1) {
				t.Fatal("oversize admission/worker selection disagree")
			}
			// Durable metadata must preserve the rejection across process restarts.
			raw, err := jsonBytes(task)
			if err != nil {
				t.Fatal(err)
			}
			var recovered Task
			if json.Unmarshal(raw, &recovered) != nil || !recovered.Result.S3.valid(recovered, a.id) || recovered.Result.S3.State != tc.state {
				t.Fatal("size boundary produced invalid durable metadata")
			}
		})
	}
}

func TestYOSLargeArchiveUsesSinglePut(t *testing.T) {
	// Larger than the S3 multipart threshold, with bounded memory on both sides.
	f, err := os.CreateTemp(t.TempDir(), "large-archive")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	const size = 65 << 20
	if err = f.Truncate(size); err != nil {
		t.Fatal(err)
	}
	n, sum, err := digest(io.NewSectionReader(f, 0, size))
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	puts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/fixture/qa/result.tar.gz" {
			t.Error("wrong object path")
			w.WriteHeader(400)
			return
		}
		switch r.Method {
		case "PUT":
			if r.URL.RawQuery != "" || r.ContentLength != size {
				t.Error("multipart or chunked upload")
			}
			gotN, gotSum, err := digest(r.Body)
			if err != nil || gotN != n || gotSum != sum {
				t.Error("large upload bytes differ")
			}
			mu.Lock()
			puts++
			mu.Unlock()
		case "GET":
			// No Content-Length: exercise streaming verification and its byte bound.
			w.(http.Flusher).Flush()
			_, _ = io.Copy(w, io.NewSectionReader(f, 0, size))
		default:
			t.Error("unexpected operation")
			w.WriteHeader(405)
		}
	}))
	defer srv.Close()
	client := newYOSClient(S3Config{Endpoint: srv.URL, Bucket: "fixture/qa"}, nil)
	o := uploadObject{Key: "result.tar.gz", Size: n, SHA256: sum}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err = client.Put(ctx, o, f, func(string) error { t.Error("multipart checkpoint"); return nil }); err != nil {
		t.Fatal(err)
	}
	if err = client.Check(ctx, o); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if puts != 1 {
		t.Fatal("expected one PUT")
	}
}

func TestYOSTaskTransferAndRestart(t *testing.T) {
	c, fixture := startYOSFixture(t)
	a := transferAgent(t, c, fixtureCollector{}, nil)
	task, _, err := a.Submit(Request{RequestID: "yos-transfer"})
	if err != nil {
		t.Fatal(err)
	}
	got := waitTransfer(t, a, task.TaskID, "uploaded")
	if got.State != "completed" || !strings.HasPrefix(got.Result.S3.URL, "https://fixture.cos.") || got.Result.S3.URLExpiresAt == nil {
		t.Fatal("missing COS result")
	}
	if len(c.S3.Bucket+"/"+got.Result.S3.Key) > 128 {
		t.Fatal("YOS key too long")
	}
	local := request(t, a, "GET", "/v1/tasks/"+task.TaskID+"/result", "", c.HTTP.Token)
	fixture.mu.Lock()
	remote := bytes.Clone(fixture.objects["/"+c.S3.Bucket+"/"+got.Result.S3.Key])
	fixture.mu.Unlock()
	if !bytes.Equal(local.Body.Bytes(), remote) {
		t.Fatal("remote differs from local archive")
	}
	a.Close()
	raw, err := os.ReadFile(filepath.Join(c.Storage.Path, taskPath(task.TaskID, "task.json")))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("fixture-signature")) {
		t.Fatal("signed link persisted")
	}
	var saved Task
	if err = json.Unmarshal(raw, &saved); err != nil {
		t.Fatal(err)
	}
	// A crash after PUT but before durable acknowledgement must reconcile bytes.
	saved.Result.S3.State = "uploading"
	saved.Result.S3.UploadedAt = nil
	raw, _ = jsonBytes(saved)
	fixtureWrite(t, c.Storage.Path, taskPath(task.TaskID, "task.json"), raw)
	b := transferAgent(t, c, fixtureCollector{}, nil)
	_ = waitTransfer(t, b, task.TaskID, "uploaded")
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if fixture.puts != 1 {
		t.Fatalf("restart reuploaded: %d", fixture.puts)
	}
}
