//go:build s3integration

package agent

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"io"
	"net/http"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// This test creates objects in a disposable test bucket. Never point it at a
// production bucket. The agent itself never calls CreateBucket.
func TestS3LiveHTTPS(t *testing.T) {
	endpoint, cert := os.Getenv("DEITYSIGHT_TEST_S3_ENDPOINT"), os.Getenv("DEITYSIGHT_TEST_S3_CA")
	if endpoint == "" || cert == "" {
		t.Fatal("set disposable S3 endpoint and CA for -tags=s3integration")
	}
	cfg := transferConfig(t)
	cfg.S3.Endpoint = endpoint
	cfg.S3.AccessKeyID = os.Getenv("DEITYSIGHT_TEST_S3_ACCESS_KEY")
	cfg.S3.SecretAccessKey = os.Getenv("DEITYSIGHT_TEST_S3_SECRET_KEY")
	cfg.S3.Bucket = "deitysight-test-" + uuid()
	pem, e := os.ReadFile(cert)
	if e != nil {
		t.Fatal(e)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(pem) {
		t.Fatal("invalid CA")
	}
	hc := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}}, Timeout: 30 * time.Second}
	client := newS3Client(cfg.S3, hc)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if _, e = client.api.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: &cfg.S3.Bucket}); e != nil {
		t.Fatal(e)
	}
	a := transferAgent(t, cfg, fixtureCollector{}, client)
	task, _, e := a.Submit(Request{RequestID: "real-s3"})
	if e != nil {
		t.Fatal(e)
	}
	got := waitTransfer(t, a, task.TaskID, "uploaded")
	response, e := hc.Get(got.Result.S3.URL)
	if e != nil {
		t.Fatal(e)
	}
	n, sum, e := digest(response.Body)
	response.Body.Close()
	if e != nil || response.StatusCode != 200 || n != got.Result.Size || sum != got.Result.SHA256 {
		t.Fatalf("signed download failed: %d %v", response.StatusCode, e)
	}
	// The server must actually verify the signature, not just the query shape.
	broken, _ := url.Parse(got.Result.S3.URL)
	q := broken.Query()
	q.Set("X-Amz-Signature", hex.EncodeToString(make([]byte, 32)))
	broken.RawQuery = q.Encode()
	response, e = hc.Get(broken.String())
	if e != nil {
		t.Fatal(e)
	}
	response.Body.Close()
	if response.StatusCode != 403 {
		t.Fatal("invalid signature accepted")
	}
	// Exercise the production multipart threshold with a sparse 70 MiB input.
	file, e := os.CreateTemp(t.TempDir(), "large-result")
	if e != nil {
		t.Fatal(e)
	}
	defer file.Close()
	if e = file.Truncate(70 << 20); e != nil {
		t.Fatal(e)
	}
	n, sum, e = digest(file)
	if e != nil {
		t.Fatal(e)
	}
	_, _ = file.Seek(0, io.SeekStart)
	obj := uploadObject{Key: "large/result.tar.gz", AgentID: uuid(), TaskID: uuid(), Size: n, SHA256: sum}
	checkpoint := ""
	start := time.Now()
	if e = client.Put(ctx, obj, file, func(id string) error { checkpoint = id; return nil }); e != nil {
		t.Fatal(e)
	}
	if checkpoint == "" {
		t.Fatal("multipart path not exercised")
	}
	remote, e := client.Head(ctx, obj.Key)
	if e != nil || !obj.matches(remote) {
		t.Fatal("multipart metadata mismatch", e)
	}
	link, e := client.Presign(ctx, obj.Key, time.Minute)
	if e != nil {
		t.Fatal(e)
	}
	response, e = hc.Get(link)
	if e != nil {
		t.Fatal(e)
	}
	gotN, gotSum, e := digest(response.Body)
	response.Body.Close()
	if e != nil || response.StatusCode != 200 || gotN != n || gotSum != sum {
		t.Fatal("multipart download hash mismatch", e)
	}
	if e = client.Put(ctx, obj, file, func(string) error { return nil }); remoteErrorCode(e) != "s3_object_conflict" {
		t.Fatalf("multipart overwrote existing object: %v", e)
	}
	t.Logf("HTTPS task transfer + real signature rejection passed; multipart bytes=%d duration=%s SHA256 verified after download", n, time.Since(start))
}
