package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"fmt"
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

type storedS3 struct {
	data     []byte
	metadata map[string]string
}
type multipartS3 struct {
	key      string
	metadata map[string]string
	parts    map[int][]byte
}
type s3Fixture struct {
	mu         sync.Mutex
	objects    map[string]storedS3
	uploads    map[string]*multipartS3
	calls      map[string]int
	failPart   int
	headStatus int
}

func fixtureError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, "<Error><Code>%s</Code><Message>provider-secret-must-not-leak</Message></Error>", code)
}
func (f *s3Fixture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls[r.Method]++
	q := r.URL.Query()
	if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 ") && q.Get("X-Amz-Signature") == "" {
		fixtureError(w, 403, "AccessDenied")
		return
	}
	key := strings.TrimPrefix(r.URL.Path, "/test-bucket/")
	meta := map[string]string{}
	for k, v := range r.Header {
		if strings.HasPrefix(strings.ToLower(k), "x-amz-meta-") {
			meta[strings.TrimPrefix(strings.ToLower(k), "x-amz-meta-")] = v[0]
		}
	}
	if r.Method == "POST" && q.Has("uploads") {
		id := fmt.Sprintf("upload-%d", f.calls["POST"])
		f.uploads[id] = &multipartS3{key: key, metadata: meta, parts: map[int][]byte{}}
		_, _ = fmt.Fprintf(w, "<InitiateMultipartUploadResult><Bucket>test-bucket</Bucket><Key>%s</Key><UploadId>%s</UploadId></InitiateMultipartUploadResult>", key, id)
		return
	}
	if id := q.Get("uploadId"); id != "" {
		u, ok := f.uploads[id]
		if !ok {
			fixtureError(w, 404, "NoSuchUpload")
			return
		}
		switch r.Method {
		case "DELETE":
			delete(f.uploads, id)
			w.WriteHeader(204)
			return
		case "GET":
			_, _ = io.WriteString(w, "<ListPartsResult><IsTruncated>false</IsTruncated></ListPartsResult>")
			return
		case "PUT":
			n, _ := strconv.Atoi(q.Get("partNumber"))
			if n == f.failPart {
				fixtureError(w, 500, "InternalError")
				return
			}
			b, _ := io.ReadAll(r.Body)
			sum := sha256.Sum256(b)
			if r.Header.Get("X-Amz-Checksum-Sha256") != base64.StdEncoding.EncodeToString(sum[:]) {
				fixtureError(w, 400, "BadDigest")
				return
			}
			u.parts[n] = b
			w.Header().Set("ETag", fmt.Sprintf("\"part-%d\"", n))
			return
		case "POST":
			if _, exists := f.objects[key]; exists {
				fixtureError(w, 412, "PreconditionFailed")
				return
			}
			var completion struct {
				Parts []struct {
					Number int `xml:"PartNumber"`
				} `xml:"Part"`
			}
			if xml.NewDecoder(r.Body).Decode(&completion) != nil {
				fixtureError(w, 400, "MalformedXML")
				return
			}
			var b []byte
			for _, part := range completion.Parts {
				b = append(b, u.parts[part.Number]...)
			}
			f.objects[key] = storedS3{b, u.metadata}
			delete(f.uploads, id)
			_, _ = io.WriteString(w, "<CompleteMultipartUploadResult><ETag>\"multipart-etag-not-sha256\"</ETag></CompleteMultipartUploadResult>")
			return
		}
	}
	switch r.Method {
	case "HEAD":
		if f.headStatus != 0 {
			fixtureError(w, f.headStatus, "AccessDenied")
			return
		}
		o, ok := f.objects[key]
		if !ok {
			fixtureError(w, 404, "NotFound")
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(o.data)))
		for k, v := range o.metadata {
			w.Header().Set("X-Amz-Meta-"+k, v)
		}
		w.Header().Set("ETag", "\"not-the-sha256\"")
	case "GET":
		o, ok := f.objects[key]
		if !ok {
			fixtureError(w, 404, "NoSuchKey")
			return
		}
		_, _ = w.Write(o.data)
	case "PUT":
		if _, ok := f.objects[key]; ok && r.Header.Get("If-None-Match") == "*" {
			fixtureError(w, 412, "PreconditionFailed")
			return
		}
		b, _ := io.ReadAll(r.Body)
		sum := sha256.Sum256(b)
		if r.Header.Get("X-Amz-Checksum-Sha256") != base64.StdEncoding.EncodeToString(sum[:]) {
			fixtureError(w, 400, "BadDigest")
			return
		}
		f.objects[key] = storedS3{b, meta}
		w.Header().Set("ETag", "\"opaque-etag\"")
	default:
		fixtureError(w, 405, "MethodNotAllowed")
	}
}
func startS3Fixture(t *testing.T) (Config, *s3Client, *s3Fixture, *http.Client) {
	t.Helper()
	f := &s3Fixture{objects: map[string]storedS3{}, uploads: map[string]*multipartS3{}, calls: map[string]int{}}
	server := httptest.NewTLSServer(f)
	t.Cleanup(server.Close)
	cfg := transferConfig(t)
	cfg.S3.Endpoint = server.URL
	return cfg, newS3Client(cfg.S3, server.Client()), f, server.Client()
}
func objectFile(t *testing.T, data []byte) (*os.File, uploadObject) {
	t.Helper()
	file, e := os.CreateTemp(t.TempDir(), "result")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = file.Write(data); e != nil {
		t.Fatal(e)
	}
	_, _ = file.Seek(0, 0)
	t.Cleanup(func() { file.Close() })
	sum := sha256.Sum256(data)
	return file, uploadObject{Key: "results/one.tar.gz", AgentID: uuid(), TaskID: uuid(), SHA256: hex.EncodeToString(sum[:]), Size: int64(len(data))}
}

func TestS3HTTPSProtocolAndPresignedDownload(t *testing.T) {
	cfg, client, fixture, httpClient := startS3Fixture(t)
	a := transferAgent(t, cfg, fixtureCollector{}, client)
	task, _, e := a.Submit(Request{RequestID: "sdk-https"})
	if e != nil {
		t.Fatal(e)
	}
	v := waitTransfer(t, a, task.TaskID, "uploaded")
	link, e := url.Parse(v.Result.S3.URL)
	if e != nil {
		t.Fatal(e)
	}
	q := link.Query()
	if link.Scheme != "https" || q.Get("X-Amz-Algorithm") != "AWS4-HMAC-SHA256" || q.Get("X-Amz-Expires") != "3600" || !strings.Contains(q.Get("X-Amz-Credential"), cfg.S3.AccessKeyID) {
		t.Fatal("invalid presigned URL")
	}
	response, e := httpClient.Get(v.Result.S3.URL)
	if e != nil {
		t.Fatal(e)
	}
	data, e := io.ReadAll(response.Body)
	response.Body.Close()
	if e != nil {
		t.Fatal(e)
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != v.Result.SHA256 {
		t.Fatal("download checksum mismatch")
	}
	fixture.mu.Lock()
	puts := fixture.calls["PUT"]
	fixture.mu.Unlock()
	if puts != 1 {
		t.Fatal(puts)
	}
	persisted, e := os.ReadFile(filepath.Join(cfg.Storage.Path, taskPath(task.TaskID, "task.json")))
	if e != nil {
		t.Fatal(e)
	}
	if bytes.Contains(persisted, []byte("X-Amz-")) {
		t.Fatal("signed URL persisted")
	}
	// Never trust the same endpoint when its TLS certificate isn't in the root pool.
	untrusted := newS3Client(cfg.S3, nil)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, e = untrusted.Head(ctx, v.Result.S3.Key); e == nil {
		t.Fatal("untrusted certificate accepted")
	}
}

func TestS3MultipartCheckpointChecksumAndAbort(t *testing.T) {
	_, client, fixture, _ := startS3Fixture(t)
	client.multipartThreshold = 1
	data := bytes.Repeat([]byte("multipart-data"), 900000)
	file, o := objectFile(t, data)
	var checkpoint string
	if e := client.Put(context.Background(), o, file, func(id string) error { checkpoint = id; return nil }); e != nil {
		t.Fatal(e)
	}
	if checkpoint == "" {
		t.Fatal("multipart upload id was not checkpointed")
	}
	info, e := client.Head(context.Background(), o.Key)
	if e != nil || !o.matches(info) {
		t.Fatalf("multipart metadata %v %+v", e, info)
	}
	fixture.mu.Lock()
	remote := fixture.objects[o.Key].data
	fixture.failPart = 2
	fixture.mu.Unlock()
	if !bytes.Equal(data, remote) {
		t.Fatal("multipart assembly changed bytes")
	}
	o.Key = "results/failing.tar.gz"
	checkpoint = ""
	if e = client.Put(context.Background(), o, file, func(id string) error { checkpoint = id; return nil }); e == nil {
		t.Fatal("part failure ignored")
	}
	fixture.mu.Lock()
	remaining := len(fixture.uploads)
	fixture.mu.Unlock()
	if remaining != 0 || checkpoint != "" {
		t.Fatal("failed multipart not aborted")
	}
	if e = client.Abort(context.Background(), o.Key, "already-gone"); e != nil {
		t.Fatal(e)
	}
	fixture.mu.Lock()
	fixture.failPart = 0
	fixture.mu.Unlock()
	if e = client.Put(context.Background(), o, file, func(string) error { return io.ErrClosedPipe }); e == nil {
		t.Fatal("checkpoint failure ignored")
	}
	fixture.mu.Lock()
	remaining = len(fixture.uploads)
	fixture.mu.Unlock()
	if remaining != 0 {
		t.Fatal("uncheckpointed multipart leaked")
	}
}

func TestS3Head403IsNotAbsenceAndConflictsDoNotOverwrite(t *testing.T) {
	_, client, fixture, _ := startS3Fixture(t)
	file, o := objectFile(t, []byte("original"))
	if e := client.Put(context.Background(), o, file, func(string) error { return nil }); e != nil {
		t.Fatal(e)
	}
	if e := client.Put(context.Background(), o, file, func(string) error { return nil }); remoteErrorCode(e) != "s3_object_conflict" {
		t.Fatalf("conditional put: %v", e)
	}
	fixture.mu.Lock()
	fixture.headStatus = 403
	fixture.mu.Unlock()
	_, e := client.Head(context.Background(), "missing")
	if e == nil || e == errObjectMissing || remoteErrorCode(e) != "s3_access_denied" {
		t.Fatalf("403 treated as absence: %v", e)
	}
}
