package agent

import (
	"context"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// YOS documents a 1G single PUT limit. Use the conservative decimal limit;
// multipart is not part of the supported YOS contract.
const yosMaxObjectSize int64 = 1_000_000_000

type yosClient struct {
	http   *http.Client
	target S3Target
}

func newYOSClient(c S3Config, hc *http.Client) *yosClient {
	if hc == nil {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.MaxIdleConnsPerHost = 2
		transport.ResponseHeaderTimeout = 30 * time.Second
		transport.DisableCompression = true
		hc = &http.Client{Transport: transport}
	}
	copy := *hc
	copy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &yosClient{http: &copy, target: c.target()}
}

type yosStatusError int

func (e yosStatusError) Error() string { return "YOS request failed" }

func (c *yosClient) request(ctx context.Context, method, key string, query url.Values, body io.Reader, size int64) (*http.Response, error) {
	if !validYOSPath(key) || len(c.target.Bucket+"/"+key) > 128 {
		return nil, errors.New("invalid YOS object key")
	}
	u := c.target.Endpoint + "/" + c.target.Bucket + "/" + key
	if len(query) != 0 {
		u += "?" + query.Encode()
	}
	r, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}
	r.ContentLength = size
	r.Header.Set("Cache-Control", "no-cache")
	if body != nil {
		r.Header.Set("Content-Type", "application/gzip")
	}
	response, err := c.http.Do(r)
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		response.Body.Close()
		if method == http.MethodGet && response.StatusCode == http.StatusNotFound {
			return nil, errObjectMissing
		}
		return nil, yosStatusError(response.StatusCode)
	}
	return response, nil
}

// YOS does not return our metadata. Read and hash actual bytes instead. A task's
// unique, durable content-derived key provides identity; HEAD/ETag do not.
func (c *yosClient) Check(ctx context.Context, o uploadObject) error {
	if o.Size < 0 || o.Size > yosMaxObjectSize {
		return errors.New("YOS object too large")
	}
	r, err := c.request(ctx, http.MethodGet, o.Key, url.Values{"cloud": {"cos"}}, nil, 0)
	if err != nil {
		return err
	}
	defer r.Body.Close()
	if r.ContentLength >= 0 && r.ContentLength != o.Size {
		return errObjectConflict
	}
	n, sum, err := digest(io.LimitReader(&contextReader{ctx: ctx, r: r.Body}, o.Size+1))
	if err != nil {
		return err
	}
	if n != o.Size || sum != o.SHA256 {
		return errObjectConflict
	}
	return nil
}

func (c *yosClient) Put(ctx context.Context, o uploadObject, f *os.File, _ func(string) error) error {
	if o.Size < 0 || o.Size > yosMaxObjectSize || o.UploadID != "" {
		return errors.New("unsupported YOS upload")
	}
	// The worker checks for an existing object first and verifies bytes afterward.
	// YOS has no atomic create-only write: other writers must not share agent keys.
	r, err := c.request(ctx, http.MethodPut, o.Key, nil, io.NewSectionReader(f, 0, o.Size), o.Size)
	if err != nil {
		return err
	}
	r.Body.Close()
	return nil
}

func (c *yosClient) Abort(_ context.Context, _, id string) error {
	if id != "" {
		return errors.New("YOS multipart is unsupported")
	}
	return nil
}

func (c *yosClient) Presign(ctx context.Context, key string, ttl time.Duration) (string, error) {
	now := time.Now().UTC().Truncate(time.Second)
	end := now.Add(ttl)
	r, err := c.request(ctx, http.MethodGet, key, url.Values{"presign": {"true"}, "cloud": {"cos"}, "expire": {strconv.FormatInt(end.Unix(), 10)}}, nil, 0)
	if err != nil {
		return "", err
	}
	defer r.Body.Close()
	const maxReply = 16 << 10
	b, err := io.ReadAll(io.LimitReader(r.Body, maxReply+1))
	if err != nil {
		return "", err
	}
	if len(b) > maxReply {
		return "", errors.New("YOS signing response too large")
	}
	var reply struct {
		XMLName xml.Name `xml:"Data"`
		Code    string   `xml:"Code"`
		Message string   `xml:"Message"`
	}
	if xml.Unmarshal(b, &reply) != nil || reply.Code != "OK" || !validCOSLink(reply.Message, c.target.Bucket+"/"+key, now, end) {
		return "", errors.New("invalid YOS signing response")
	}
	return reply.Message, nil
}

// Only return the expected COS HTTPS object URL. Never follow a gateway
// redirect, return an arbitrary host, or extend the requested link lifetime.
func validCOSLink(link, objectPath string, now, end time.Time) bool {
	u, err := url.Parse(link)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Fragment != "" || (u.Port() != "" && u.Port() != "443") || u.Path != "/"+objectPath {
		return false
	}
	host := strings.ToLower(u.Hostname())
	if !strings.HasSuffix(host, ".myqcloud.com") || !strings.Contains(host, ".cos.") {
		return false
	}
	// COS uses literal semicolons in q-sign-time and header lists. Go's query
	// parser rejects those unless escaped; only normalize the validation copy.
	q, err := url.ParseQuery(strings.ReplaceAll(u.RawQuery, ";", "%3B"))
	if err != nil || len(q["q-signature"]) != 1 || q.Get("q-signature") == "" || len(q["q-sign-time"]) != 1 {
		return false
	}
	span := strings.Split(q.Get("q-sign-time"), ";")
	if len(span) != 2 {
		return false
	}
	start, e1 := strconv.ParseInt(span[0], 10, 64)
	expires, e2 := strconv.ParseInt(span[1], 10, 64)
	// The gateway clock can be slightly ahead of the agent (observed in QA).
	// Accept bounded start-time skew, but never a longer expiry than requested.
	return e1 == nil && e2 == nil && start <= now.Add(30*time.Second).Unix() && start < expires && expires == end.Unix() && expires > time.Now().Unix()
}
