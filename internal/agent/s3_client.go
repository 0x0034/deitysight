package agent

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	"github.com/aws/smithy-go/logging"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

type s3Client struct {
	api                          *s3.Client
	bucket                       string
	multipartThreshold, partSize int64
}

func newRemoteStore(c S3Config) remoteStore {
	if c.Provider == "yos" {
		return newYOSClient(c, nil)
	}
	return newS3Client(c, nil)
}

func (c *s3Client) Check(ctx context.Context, o uploadObject) error {
	r, err := c.Head(ctx, o.Key)
	if err != nil {
		return err
	}
	if !o.matches(r) {
		return errObjectConflict
	}
	return nil
}

func newS3Client(c S3Config, client *http.Client) *s3Client {
	if client == nil {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.MaxIdleConnsPerHost = 2
		transport.ResponseHeaderTimeout = 30 * time.Second
		client = &http.Client{Transport: transport}
	}
	copy := *client
	copy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	api := s3.New(s3.Options{Region: c.Region, BaseEndpoint: aws.String(c.target().Endpoint), UsePathStyle: c.ForcePathStyle,
		Credentials: aws.NewCredentialsCache(credentials.NewStaticCredentialsProvider(c.AccessKeyID, c.SecretAccessKey, c.SessionToken)),
		HTTPClient:  &copy, RetryMaxAttempts: 1, RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		Logger: logging.NewStandardLogger(io.Discard)})
	return &s3Client{api: api, bucket: c.Bucket, multipartThreshold: 64 << 20, partSize: 8 << 20}
}
func (c *s3Client) Head(ctx context.Context, key string) (remoteObject, error) {
	r, e := c.api.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &c.bucket, Key: &key})
	if e != nil {
		var response *smithyhttp.ResponseError
		if errors.As(e, &response) && response.HTTPStatusCode() == 404 {
			return remoteObject{}, errObjectMissing
		}
		return remoteObject{}, e
	}
	return remoteObject{Size: aws.ToInt64(r.ContentLength), SHA256: r.Metadata["sha256"], AgentID: r.Metadata["agent-id"], TaskID: r.Metadata["task-id"]}, nil
}
func (c *s3Client) Put(ctx context.Context, o uploadObject, f *os.File, checkpoint func(string) error) error {
	metadata := map[string]string{"sha256": o.SHA256, "agent-id": o.AgentID, "task-id": o.TaskID}
	if o.Size <= c.multipartThreshold {
		raw, e := hex.DecodeString(o.SHA256)
		if e != nil {
			return errRemoteIntegrity
		}
		sum := base64.StdEncoding.EncodeToString(raw)
		_, e = c.api.PutObject(ctx, &s3.PutObjectInput{Bucket: &c.bucket, Key: &o.Key, Body: io.NewSectionReader(f, 0, o.Size), ContentLength: &o.Size, ContentType: aws.String("application/gzip"), Metadata: metadata, IfNoneMatch: aws.String("*"), ChecksumSHA256: &sum})
		return e
	}
	// At most 10,000 parts; each section is an io.ReaderAt/Seeker, not a buffered archive.
	partSize := max(c.partSize, (o.Size+9999)/10000)
	if partSize > 5<<30 {
		return errors.New("object too large")
	}
	created, e := c.api.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: &c.bucket, Key: &o.Key, ContentType: aws.String("application/gzip"), Metadata: metadata, ChecksumAlgorithm: types.ChecksumAlgorithmSha256})
	if e != nil {
		return e
	}
	id := aws.ToString(created.UploadId)
	if id == "" {
		return errRemoteIntegrity
	}
	// Persist before sending parts. If persistence fails, attempt to remove the orphan.
	if e = checkpoint(id); e != nil {
		_ = c.Abort(ctx, o.Key, id)
		return e
	}
	completed := false
	defer func() {
		if !completed {
			if c.Abort(ctx, o.Key, id) == nil {
				_ = checkpoint("")
			}
		}
	}()
	parts := []types.CompletedPart{}
	for offset := int64(0); offset < o.Size; offset += partSize {
		size := min(partSize, o.Size-offset)
		partNumber := int32(len(parts) + 1)
		h := sha256.New()
		if _, e = io.Copy(h, &contextReader{ctx: ctx, r: io.NewSectionReader(f, offset, size)}); e != nil {
			return e
		}
		sum := base64.StdEncoding.EncodeToString(h.Sum(nil))
		part, e := c.api.UploadPart(ctx, &s3.UploadPartInput{Bucket: &c.bucket, Key: &o.Key, UploadId: &id, PartNumber: &partNumber, ContentLength: &size, Body: io.NewSectionReader(f, offset, size), ChecksumSHA256: &sum})
		if e != nil {
			return e
		}
		if aws.ToString(part.ETag) == "" {
			return errRemoteIntegrity
		}
		parts = append(parts, types.CompletedPart{PartNumber: &partNumber, ETag: part.ETag, ChecksumSHA256: &sum})
	}
	_, e = c.api.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{Bucket: &c.bucket, Key: &o.Key, UploadId: &id, MultipartUpload: &types.CompletedMultipartUpload{Parts: parts}, IfNoneMatch: aws.String("*")})
	if e != nil {
		return e
	}
	completed = true
	return nil
}
func (c *s3Client) Abort(ctx context.Context, key, id string) error {
	if id == "" {
		return nil
	}
	for i := 0; i < 3; i++ {
		_, e := c.api.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{Bucket: &c.bucket, Key: &key, UploadId: &id})
		if noSuchUpload(e) {
			return nil
		}
		if e != nil {
			return e
		}
		list, e := c.api.ListParts(ctx, &s3.ListPartsInput{Bucket: &c.bucket, Key: &key, UploadId: &id, MaxParts: aws.Int32(1)})
		if noSuchUpload(e) {
			return nil
		}
		if e != nil {
			return e
		}
		if len(list.Parts) == 0 && !aws.ToBool(list.IsTruncated) {
			return nil
		}
	}
	return errors.New("multipart cleanup incomplete")
}
func noSuchUpload(e error) bool {
	var api smithy.APIError
	return errors.As(e, &api) && api.ErrorCode() == "NoSuchUpload"
}
func (c *s3Client) Presign(ctx context.Context, key string, ttl time.Duration) (string, error) {
	r, e := s3.NewPresignClient(c.api).PresignGetObject(ctx, &s3.GetObjectInput{Bucket: &c.bucket, Key: &key}, func(o *s3.PresignOptions) { o.Expires = ttl })
	if e != nil {
		return "", e
	}
	return r.URL, nil
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r *contextReader) Read(b []byte) (int, error) {
	if e := r.ctx.Err(); e != nil {
		return 0, e
	}
	return r.r.Read(b)
}

// Never persist provider error strings: they may contain headers, endpoint user
// info, access keys or signed query parameters.
func remoteErrorCode(e error) string {
	switch {
	case errors.Is(e, context.DeadlineExceeded):
		return "s3_timeout"
	case errors.Is(e, context.Canceled):
		return "s3_interrupted"
	case errors.Is(e, errObjectConflict):
		return "s3_object_conflict"
	case errors.Is(e, errRemoteIntegrity):
		return "s3_integrity_mismatch"
	}
	var response *smithyhttp.ResponseError
	var yosStatus yosStatusError
	status := 0
	if errors.As(e, &response) {
		status = response.HTTPStatusCode()
	}
	if errors.As(e, &yosStatus) {
		status = int(yosStatus)
	}
	if status != 0 {
		switch status {
		case 401, 403:
			return "s3_access_denied"
		case 404:
			return "s3_not_found"
		case 409, 412:
			return "s3_object_conflict"
		case 429:
			return "s3_throttled"
		}
		if status >= 500 {
			return "s3_service_unavailable"
		}
	}
	var api smithy.APIError
	if errors.As(e, &api) {
		switch api.ErrorCode() {
		case "AccessDenied", "InvalidAccessKeyId", "SignatureDoesNotMatch", "ExpiredToken", "InvalidToken":
			return "s3_access_denied"
		case "SlowDown", "Throttling":
			return "s3_throttled"
		}
	}
	var network net.Error
	if errors.As(e, &network) && network.Timeout() {
		return "s3_timeout"
	}
	return "s3_request_failed"
}
