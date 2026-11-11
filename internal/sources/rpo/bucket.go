package rpo

import (
	"bufio"
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// DefaultBucketURL is the public, anonymous S3-compatible bucket of the RPO bulk files.
const DefaultBucketURL = "https://frkqbrydxwdp.compat.objectstorage.eu-frankfurt-1.oraclecloud.com/susr-rpo"

// Bucket reads the RPO bucket over plain HTTPS (S3 ListObjectsV2 and GET with Range).
type Bucket struct {
	BaseURL    string
	HTTP       *http.Client
	Retries    int              // per download; default 4
	RetryDelay time.Duration    // first backoff; default 2 s, doubled each time
	Now        func() time.Time // default time.Now; files named for a later day than tomorrow are ignored
}

// NewBucket returns a client for baseURL.
func NewBucket(baseURL string) *Bucket {
	return &Bucket{BaseURL: strings.TrimRight(baseURL, "/"), HTTP: &http.Client{Timeout: 30 * time.Minute}}
}

type listResult struct {
	Contents []struct {
		Key          string    `xml:"Key"`
		Size         int64     `xml:"Size"`
		LastModified time.Time `xml:"LastModified"`
		ETag         string    `xml:"ETag"`
	} `xml:"Contents"`
	IsTruncated           bool   `xml:"IsTruncated"`
	NextContinuationToken string `xml:"NextContinuationToken"`
}

// List returns every object under prefix, following continuation tokens.
func (b *Bucket) List(ctx context.Context, prefix string) ([]Object, error) {
	var out []Object
	token := ""
	for page := 0; page < 1000; page++ {
		q := url.Values{"list-type": {"2"}, "prefix": {prefix}}
		if token != "" {
			q.Set("continuation-token", token)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.BaseURL+"/?"+q.Encode(), nil)
		if err != nil {
			return nil, err
		}
		resp, err := b.HTTP.Do(req)
		if err != nil {
			return nil, fmt.Errorf("list %s: %w", prefix, err)
		}
		var lr listResult
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return nil, fmt.Errorf("list %s: HTTP %d", prefix, resp.StatusCode)
		}
		err = xml.NewDecoder(resp.Body).Decode(&lr)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("list %s: %w", prefix, err)
		}
		for _, c := range lr.Contents {
			out = append(out, Object{Key: c.Key, Size: c.Size, Modified: c.LastModified, ETag: strings.Trim(c.ETag, `"`)})
		}
		if !lr.IsTruncated || lr.NextContinuationToken == "" {
			return out, nil
		}
		token = lr.NextContinuationToken
	}
	return nil, fmt.Errorf("list %s: too many pages", prefix)
}

// Inventory lists the daily and snapshot prefixes.
func (b *Bucket) Inventory(ctx context.Context) (Inventory, error) {
	daily, err := b.List(ctx, "batch-daily/")
	if err != nil {
		return Inventory{}, err
	}
	init, err := b.List(ctx, "batch-init/")
	if err != nil {
		return Inventory{}, err
	}
	now := time.Now
	if b.Now != nil {
		now = b.Now
	}
	// one day of tolerance: the publisher's clock and time zone
	return Classify(append(daily, init...), now().UTC().Truncate(24*time.Hour).AddDate(0, 0, 1)), nil
}

// ReadManifest returns the part file names a snapshot manifest lists.
func (b *Bucket) ReadManifest(ctx context.Context, key string) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.BaseURL+"/"+key, nil)
	if err != nil {
		return nil, err
	}
	resp, err := b.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("manifest %s: HTTP %d", key, resp.StatusCode)
	}
	var parts []string
	sc := bufio.NewScanner(io.LimitReader(resp.Body, 1<<20))
	for sc.Scan() {
		if line := strings.TrimSpace(sc.Text()); line != "" {
			parts = append(parts, line)
		}
	}
	return parts, sc.Err()
}

// Download fetches obj into dir and returns the local path. A partial file left by an earlier
// attempt is resumed with a Range request. The final size must equal the listed size.
func (b *Bucket) Download(ctx context.Context, obj Object, dir string) (string, error) {
	if limit := maxObjectBytes(obj.Key); obj.Size > limit {
		return "", fmt.Errorf("download %s: %w: the bucket lists %d bytes, the limit is %d", obj.Key, ErrTooLarge, obj.Size, limit)
	}
	path := localPath(obj, dir)
	// Copies of other versions of this object are never reused: a partial or whole file is resumed
	// or kept only when it belongs to the same content (the same ETag). Without an ETag nothing
	// can be told apart, so nothing is reused.
	if old, _ := filepath.Glob(filepath.Join(dir, filepath.Base(obj.Key)+"*")); len(old) > 0 {
		for _, f := range old {
			if f != path || obj.ETag == "" {
				_ = os.Remove(f)
			}
		}
	}
	retries, delay := b.Retries, b.RetryDelay
	if retries <= 0 {
		retries = 4
	}
	if delay <= 0 {
		delay = 2 * time.Second
	}
	var last error
	for attempt := 0; attempt <= retries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(delay):
			}
			delay *= 2
		}
		last = b.downloadOnce(ctx, obj, path)
		if last == nil {
			last = verifyETag(obj, path)
		}
		if last == nil {
			return path, nil
		}
		if errors.Is(last, ErrChanged) {
			_ = os.Remove(path)
			return "", fmt.Errorf("download %s: %w", obj.Key, last)
		}
		if errors.Is(last, context.Canceled) || errors.Is(last, context.DeadlineExceeded) {
			return "", last
		}
	}
	return "", fmt.Errorf("download %s: %w", obj.Key, last)
}

func (b *Bucket) downloadOnce(ctx context.Context, obj Object, path string) error {
	var have int64
	if st, err := os.Stat(path); err == nil {
		have = st.Size()
	}
	if have == obj.Size && obj.Size > 0 {
		return nil
	}
	if have > obj.Size {
		if err := os.Remove(path); err != nil {
			return err
		}
		have = 0
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.BaseURL+"/"+obj.Key, nil)
	if err != nil {
		return err
	}
	if have > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", have))
	}
	resp, err := b.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	flags := os.O_CREATE | os.O_WRONLY
	switch {
	case have > 0 && resp.StatusCode == http.StatusPartialContent:
		flags |= os.O_APPEND
	case resp.StatusCode == http.StatusOK:
		flags |= os.O_TRUNC
		if have > 0 {
			have = 0 // full body: start over
		}
	default:
		return fmt.Errorf("GET %s: HTTP %d", obj.Key, resp.StatusCode)
	}
	f, err := os.OpenFile(path, flags, 0o600)
	if err != nil {
		return err
	}
	// never more than the listing promised (one byte over shows a longer body as a size mismatch)
	n, copyErr := io.Copy(f, io.LimitReader(resp.Body, obj.Size-have+1))
	closeErr := f.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	if have+n != obj.Size {
		return fmt.Errorf("%s: got %d bytes, want %d", obj.Key, have+n, obj.Size)
	}
	return nil
}

// Download limits, with headroom over the real files (docs/sources.md and the bucket on
// 2026-11-11: a snapshot part is up to about 100 MB, a daily file up to 2.3 MB).
const (
	MaxPartBytes  = 1 << 30   // 1 GiB
	MaxDailyBytes = 256 << 20 // 256 MiB
)

func maxObjectBytes(key string) int64 {
	if reDaily.MatchString(key) {
		return MaxDailyBytes
	}
	return MaxPartBytes
}

// ErrChanged means the object's content is not the one the listing described: the file was
// replaced in the bucket after it was listed.
var ErrChanged = errors.New("the file changed in the bucket after it was listed")

// localPath is where obj is kept while it is applied. The name carries a digest of the ETag, so
// two versions of one key never share a file.
func localPath(obj Object, dir string) string {
	base := filepath.Base(obj.Key)
	if obj.ETag == "" {
		return filepath.Join(dir, base)
	}
	sum := sha256.Sum256([]byte(obj.ETag))
	return filepath.Join(dir, base+"~"+hex.EncodeToString(sum[:6]))
}

// verifyETag checks the downloaded bytes against the listing when the ETag is a plain MD5 (an
// object uploaded in one piece). Multipart ETags are not content hashes and are not checked here;
// the snapshot fingerprint still catches a change of them.
func verifyETag(obj Object, path string) error {
	if len(obj.ETag) != 32 || strings.Trim(obj.ETag, "0123456789abcdef") != "" {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	h := md5.New() //nolint:gosec // S3 ETags are MD5; this detects a replaced file, it is not a signature
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	if hex.EncodeToString(h.Sum(nil)) != obj.ETag {
		return ErrChanged
	}
	return nil
}

// DownloadAll fetches objs with up to concurrency parallel streams. It returns the local paths in
// the order of objs, or the first error.
func (b *Bucket) DownloadAll(ctx context.Context, objs []Object, dir string, concurrency int) ([]string, error) {
	if concurrency < 1 {
		concurrency = 1
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	paths := make([]string, len(objs))
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	var once sync.Once
	var firstErr error
	for i, o := range objs {
		wg.Add(1)
		go func(i int, o Object) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-sem }()
			p, err := b.Download(ctx, o, dir)
			if err != nil {
				once.Do(func() { firstErr = err; cancel() })
				return
			}
			paths[i] = p
		}(i, o)
	}
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return paths, nil
}
