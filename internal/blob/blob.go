package blob

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/mash/kioku/internal/store"
)

const MaxObject = 16 << 20

type Store interface {
	Put(context.Context, string, []byte) error
	Get(context.Context, string) ([]byte, error)
	Delete(context.Context, string) error
	List(context.Context, string) ([]string, error)
}

func (l Local) Delete(_ context.Context, key string) error {
	if !ValidKey(key) {
		return errors.New("invalid object key")
	}
	err := os.Remove(filepath.Join(l.Dir, filepath.FromSlash(key)))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
func (l Local) List(_ context.Context, prefix string) ([]string, error) {
	if !ValidKey(strings.TrimSuffix(prefix, "/")) {
		return nil, errors.New("invalid prefix")
	}
	out := []string{}
	err := filepath.WalkDir(l.Dir, func(p string, d os.DirEntry, err error) error {
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(l.Dir, p)
		if err != nil {
			return err
		}
		key := filepath.ToSlash(rel)
		if strings.HasPrefix(key, prefix) {
			out = append(out, key)
		}
		return nil
	})
	return out, err
}

func ValidKey(key string) bool {
	return key != "" && !strings.HasPrefix(key, "/") && !strings.ContainsAny(key, "\\\x00") && path.Clean(key) == key && key != ".." && !strings.HasPrefix(key, "../")
}

type Local struct{ Dir string }

func (l Local) Put(_ context.Context, key string, b []byte) error {
	if !ValidKey(key) {
		return errors.New("invalid object key")
	}
	p := filepath.Join(l.Dir, filepath.FromSlash(key))
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		return err
	}
	return store.AtomicWrite(p, b, 0600)
}
func (l Local) Get(_ context.Context, key string) ([]byte, error) {
	if !ValidKey(key) {
		return nil, errors.New("invalid object key")
	}
	return os.ReadFile(filepath.Join(l.Dir, filepath.FromSlash(key)))
}

// S3 uses SigV4 and path-style addressing, supported by S3, R2 and MinIO.
// Supply short-lived credentials through the environment when available.
type S3 struct {
	Endpoint, Bucket, Region, AccessKey, SecretKey, SessionToken string
	Client                                                       *http.Client
}

func (s S3) Put(ctx context.Context, key string, b []byte) error {
	_, err := s.request(ctx, http.MethodPut, key, b)
	return err
}
func (s S3) Get(ctx context.Context, key string) ([]byte, error) {
	return s.request(ctx, http.MethodGet, key, nil)
}
func (s S3) Delete(ctx context.Context, key string) error {
	if !ValidKey(key) {
		return errors.New("invalid key")
	}
	_, err := s.request(ctx, "DELETE", key, nil)
	return err
}
func (s S3) List(ctx context.Context, prefix string) ([]string, error) {
	out := []string{}
	continuation := ""
	for {
		q := url.Values{"list-type": {"2"}, "prefix": {prefix}}
		if continuation != "" {
			q.Set("continuation-token", continuation)
		}
		b, err := s.request(ctx, "GET", "", nil, q)
		if err != nil {
			return nil, err
		}
		var page struct {
			Contents []struct {
				Key string `xml:"Key"`
			} `xml:"Contents"`
			Next string `xml:"NextContinuationToken"`
			More bool   `xml:"IsTruncated"`
		}
		if err = xml.Unmarshal(b, &page); err != nil {
			return nil, err
		}
		for _, v := range page.Contents {
			out = append(out, v.Key)
		}
		if !page.More {
			return out, nil
		}
		if page.Next == "" || page.Next == continuation {
			return nil, errors.New("invalid storage pagination")
		}
		continuation = page.Next
	}
}

type HTTPError struct{ Status int }

func (e HTTPError) NotFound() bool { return e.Status == 404 }
func (e HTTPError) Error() string  { return fmt.Sprintf("object storage returned HTTP %d", e.Status) }
func hash(b []byte) string         { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func mac(key []byte, value string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(value))
	return h.Sum(nil)
}
func (s S3) request(ctx context.Context, method, key string, b []byte, query ...url.Values) ([]byte, error) {
	if key != "" && !ValidKey(key) {
		return nil, errors.New("invalid object key")
	}
	u, err := url.Parse(s.Endpoint)
	if err != nil {
		return nil, err
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return nil, errors.New("invalid S3 endpoint")
	}
	if u.Host == "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return nil, errors.New("invalid S3 endpoint")
	}
	u.Path = strings.TrimSuffix(u.Path, "/") + "/" + s.Bucket
	if key != "" {
		u.Path += "/" + key
	}
	if len(query) > 0 {
		u.RawQuery = strings.ReplaceAll(query[0].Encode(), "+", "%20")
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	date := now.Format("20060102")
	stamp := now.Format("20060102T150405Z")
	payload := hash(b)
	req.Header.Set("X-Amz-Date", stamp)
	req.Header.Set("X-Amz-Content-Sha256", payload)
	headers := "host:" + u.Host + "\nx-amz-content-sha256:" + payload + "\nx-amz-date:" + stamp + "\n"
	signed := "host;x-amz-content-sha256;x-amz-date"
	if s.SessionToken != "" {
		req.Header.Set("X-Amz-Security-Token", s.SessionToken)
		headers += "x-amz-security-token:" + s.SessionToken + "\n"
		signed += ";x-amz-security-token"
	}
	canonical := method + "\n" + u.EscapedPath() + "\n" + u.RawQuery + "\n" + headers + "\n" + signed + "\n" + payload
	scope := date + "/" + s.Region + "/s3/aws4_request"
	k := mac([]byte("AWS4"+s.SecretKey), date)
	k = mac(k, s.Region)
	k = mac(k, "s3")
	k = mac(k, "aws4_request")
	signature := hex.EncodeToString(mac(k, "AWS4-HMAC-SHA256\n"+stamp+"\n"+scope+"\n"+hash([]byte(canonical))))
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+s.AccessKey+"/"+scope+", SignedHeaders="+signed+", Signature="+signature)
	client := s.Client
	if client == nil {
		client = &http.Client{Timeout: 60 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, HTTPError{resp.StatusCode}
	}
	limit := objectReadLimit(key)
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if int64(len(data)) > limit {
		return nil, errors.New("object too large")
	}
	return data, err
}

// Job input contains all sources. Allow gzip overhead above its 256 MiB tar
// limit without broadening the limit on source, metadata or output objects.
func objectReadLimit(key string) int64 {
	p := strings.Split(key, "/")
	if len(p) == 6 && p[0] == "projects" && p[2] == ".kioku" && p[3] == "jobs" && p[5] == "input.tar.gz" {
		return 257 << 20
	}
	return MaxObject
}
