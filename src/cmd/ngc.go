// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	ngcDefaultBaseURL = "https://api.ngc.nvidia.com"
	// defaultFileWorkers bounds concurrent multipart sessions
	// (initiate→complete). A tree of many small files is latency-bound on the
	// NGC API rather than bandwidth-bound: each file costs four serial
	// round-trips, so throughput is workers/round-trips, and the only lever is
	// how many chains run at once. HTTP/2 on the API client makes the extra
	// sessions nearly free (they multiplex over one connection) and apiThrottle
	// paces the whole pool back if NGC starts answering 429.
	defaultFileWorkers = 20
	// defaultMultipartWorkers is a per-file budget, not a global one, so peak
	// concurrent part PUTs is defaultFileWorkers × this. It only engages for
	// multi-part files; a file that fits in one part bypasses the part pool
	// entirely, which is why small-file trees scale with defaultFileWorkers
	// alone and a single huge file scales with this alone.
	defaultMultipartWorkers = 8  // S3 part upload workers per file
	defaultHashWorkers      = 32 // parallel file-hashing workers during scan
	// defaultDownloadWorkers bounds concurrent file downloads, and mirrors
	// defaultFileWorkers for the same reason: a file's bytes cannot start
	// moving until a presign round-trip has said where they live, so a tree of
	// many small files is latency-bound on the NGC API rather than
	// bandwidth-bound and the only lever is how many of those chains run at
	// once.
	defaultDownloadWorkers = 20
	// defaultDownloadChunkWorkers is a per-file budget, not a global one, so
	// peak concurrent range GETs is defaultDownloadWorkers × this. It only
	// engages for files past downloadRangeMinSize; anything smaller is fetched
	// as a single stream, which is why small-file trees scale with
	// defaultDownloadWorkers alone and one huge file scales with this alone.
	defaultDownloadChunkWorkers = 8
	// maxListPages caps how many pages any NGC listing will request. It is a
	// safety net against a misbehaving pager that never signals the last page;
	// normal termination happens on an empty page or paginationInfo.totalPages.
	maxListPages = 100
)

const (
	// The retry budget for one presigned transfer, in either direction: a part
	// PUT going up, a byte range coming down.
	uploadPartMaxAttempts = 5
	uploadPartRetryBase   = 2 * time.Second
	uploadPartRetryMax    = 30 * time.Second
	// uploadStallTimeout cancels a transfer whose connection makes no I/O
	// progress for this long. net/http has no overall read or write deadline,
	// so a wedged S3 connection would otherwise block a worker forever. Kept
	// comfortably above the worst-case gap between body reads on a
	// slow-but-healthy link (a single ~32 KiB write, i.e. seconds even at
	// dial-up rates) to avoid false aborts.
	uploadStallTimeout = 20 * time.Second
	// uploadResponseHeaderTimeout bounds the wait for S3's response headers
	// after a part body is fully sent (the "bytes accepted, then silence" stall,
	// common when a proxy/LB drops the connection post-upload).
	uploadResponseHeaderTimeout = 20 * time.Second
	uploadDialTimeout           = 15 * time.Second
	uploadTLSHandshakeTimeout   = 10 * time.Second
	// commitResponseHeaderTimeout bounds the wait for the manifest commit.
	// NGC validates every file in the manifest before it answers, so a version
	// with tens of thousands of files takes minutes; the part-upload budget
	// would abandon a commit that is still on its way to succeeding.
	commitResponseHeaderTimeout = 30 * time.Minute

	// Idle pool sized for peak concurrent part PUTs
	// (defaultFileWorkers × defaultMultipartWorkers).
	uploadMaxIdleConns        = 256
	uploadMaxIdleConnsPerHost = 128
	uploadWriteBufferSize     = 256 << 10 // 256 KiB
	// The same transport carries presigned GETs for downloads, where it is the
	// read buffer that bounds syscalls per transfer.
	downloadReadBufferSize = 256 << 10 // 256 KiB

	// The API client carries NGC metadata calls. Small files are latency-bound,
	// not bandwidth-bound: each one costs three NGC round-trips (initiate,
	// get-URLs, complete) plus one PUT, so the win comes from running many
	// concurrently rather than from bigger buffers.
	apiMaxIdleConns          = 256
	apiMaxIdleConnsPerHost   = 128
	apiResponseHeaderTimeout = 60 * time.Second
)

// NGC resource names must start with a lowercase letter; see RecipeCreateRequest.
var ngcArtifactNamePattern = regexp.MustCompile(`^[a-z][a-z\d._-]*$`)

func validateNGCArtifactName(name string) error {
	if name == "" {
		return fmt.Errorf("NGC artifact name is required")
	}
	if !ngcArtifactNamePattern.MatchString(name) {
		return fmt.Errorf("invalid NGC artifact name %q: must match ^[a-z][a-z\\d._-]*$ (lowercase letter first, then lowercase letters, digits, '.', '_', or '-')", name)
	}
	return nil
}

type coordinates struct { // NGC artifact version identity
	Org, Type, Name, Version string
}

func (c coordinates) uri() string {
	return fmt.Sprintf("ngc://%s/%s:%s", c.Org, c.Name, c.Version)
}

func (c coordinates) String() string {
	return fmt.Sprintf("org=%s type=%s name=%s version=%s", c.Org, c.Type, c.Name, c.Version)
}

// ngcClient uploads content-addressed artifact files to NGC.
//
// Two HTTP clients are used deliberately, split by workload rather than by file
// size. http carries NGC's JSON API, keeping HTTP/2 enabled so thousands of
// latency-bound metadata round-trips share one multiplexed connection instead of
// contending for the connection pool. partHTTP carries every presigned S3 PUT
// over HTTP/1.1, where a connection per in-flight transfer is what saturates the
// link.
type ngcClient struct {
	baseURL      string
	apiKey       string
	coords       coordinates
	http         *http.Client
	partHTTP     *http.Client  // multipart part PUTs; falls back to http when nil
	stallTimeout time.Duration // per-part no-progress timeout; 0 disables
	retryLog     retryLogger   // optional; set during upload for rate-limit messages
	progress     progressFunc  // optional; reports bytes as they reach the wire
	// throttle is shared by every request this client makes, so one worker's 429
	// slows all of them instead of each backing off in isolation.
	throttle *apiThrottle
}

// newAPIHTTPClient returns the client for NGC metadata calls. HTTP/2 is left
// enabled on purpose: NGC's gateway negotiates h2, which multiplexes many
// concurrent small requests over a single connection and makes high
// --file-workers values cheap, because Go's HTTP/1.1 transport allows only one
// in-flight request per connection. Client.Timeout stays unset because this
// client also streams downloads.
func newAPIHTTPClient() *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.DialContext = (&net.Dialer{
		Timeout:   uploadDialTimeout,
		KeepAlive: 30 * time.Second,
	}).DialContext
	t.TLSHandshakeTimeout = uploadTLSHandshakeTimeout
	t.ResponseHeaderTimeout = apiResponseHeaderTimeout
	t.MaxIdleConns = apiMaxIdleConns
	t.MaxIdleConnsPerHost = apiMaxIdleConnsPerHost
	t.ForceAttemptHTTP2 = true
	hardenTransport(t)
	return &http.Client{Transport: t}
}

// newPartHTTPClient returns the client for every presigned S3 transfer, PUT
// going up and GET coming down. It deliberately omits Client.Timeout (which
// would cap the entire transfer and kill legitimately long parts) and instead
// bounds each phase that can wedge: connection setup, TLS handshake, and the
// wait for response headers. Mid-body stalls are handled by the stall watchdog
// in putPartOnce and fetchRangeOnce. TCP keep-alives let the OS surface a
// truly dead peer.
func newPartHTTPClient() *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.DialContext = (&net.Dialer{
		Timeout:   uploadDialTimeout,
		KeepAlive: 30 * time.Second,
	}).DialContext
	t.TLSHandshakeTimeout = uploadTLSHandshakeTimeout
	t.ResponseHeaderTimeout = uploadResponseHeaderTimeout
	t.ExpectContinueTimeout = 1 * time.Second
	t.MaxIdleConns = uploadMaxIdleConns
	t.MaxIdleConnsPerHost = uploadMaxIdleConnsPerHost
	t.WriteBufferSize = uploadWriteBufferSize
	t.ReadBufferSize = downloadReadBufferSize

	// Parallel transfers want one connection each, so opt out of HTTP/2 and its
	// single-connection multiplexing. S3 does not offer h2 today, so this is
	// belt-and-braces: it makes the property ours rather than inherited from
	// whatever ALPN a presigned host happens to advertise.
	// ForceAttemptHTTP2=false alone is not
	// enough: this transport is a clone of http.DefaultTransport, whose
	// TLSClientConfig.NextProtos still advertises "h2", so ALPN would negotiate
	// HTTP/2 while the HTTP/2 round-tripper goes uninstalled and the HTTP/1.x
	// reader then rejects the server's SETTINGS frame as a malformed response.
	// An empty TLSNextProto map does not help either; ALPN must be pinned.
	t.ForceAttemptHTTP2 = false
	if t.TLSClientConfig == nil {
		t.TLSClientConfig = &tls.Config{}
	}
	t.TLSClientConfig.NextProtos = []string{"http/1.1"}
	hardenTransport(t)
	return &http.Client{Transport: t}
}

func newNGC(baseURL, apiKey string, coords coordinates, hc *http.Client) *ngcClient {
	return &ngcClient{
		baseURL:      strings.TrimRight(firstNonEmpty(baseURL, ngcDefaultBaseURL), "/"),
		apiKey:       apiKey,
		coords:       coords,
		http:         hc,
		stallTimeout: uploadStallTimeout,
		throttle:     &apiThrottle{},
	}
}

// newNGCForUpload builds a client with both transports wired: HTTP/2 for NGC
// metadata calls and small single-part files, pinned HTTP/1.1 for multipart
// part PUTs.
func newNGCForUpload(baseURL, apiKey string, coords coordinates) *ngcClient {
	c := newNGC(baseURL, apiKey, coords, newAPIHTTPClient())
	c.partHTTP = newPartHTTPClient()
	return c
}

// newNGCForDownload wires the same split for the other direction: the file
// listing and the presign calls stay on HTTP/2, where many concurrent
// latency-bound requests multiplex over one connection, while the payload GETs
// go over pinned HTTP/1.1. Routing bulk bytes onto the h2 client would put
// every worker's transfer through a single connection, where a shared
// flow-control window, one congestion window, and single-threaded TLS cap the
// throughput the pool exists to reach.
func newNGCForDownload(baseURL, apiKey string, coords coordinates) *ngcClient {
	c := newNGC(baseURL, apiKey, coords, newAPIHTTPClient())
	c.partHTTP = newPartHTTPClient()
	return c
}

// partClient returns the client for presigned S3 transfers, falling back to the
// API client when no dedicated part client was configured (tests, single-shot
// CLI paths that never move payload bytes).
func (c *ngcClient) partClient() *http.Client {
	if c.partHTTP != nil {
		return c.partHTTP
	}
	return c.http
}

// progressFunc reports bytes newly transferred. Negative values unwind a failed
// attempt so a retry cannot double count.
type progressFunc func(n int64)

func (c *ngcClient) reportProgress(n int64) {
	if c.progress != nil && n != 0 {
		c.progress(n)
	}
}

type appFile struct {
	LocalPath string
	Path      string // NGC-relative, forward slashes
	Size      int64
	SHA256    string // base64 sha256
	// ModTime is the mtime observed when the file was hashed. Together with
	// Size it identifies the exact snapshot the SHA256 describes, letting the
	// upload detect a file that changed underneath it.
	ModTime time.Time
}

// ensureArtifact creates RESOURCE + version if missing (idempotent).
func (c *ngcClient) ensureArtifact(ctx context.Context, displayName, description string) error {
	if !strings.EqualFold(c.coords.Type, "RESOURCE") {
		return nil
	}
	displayName = firstNonEmpty(displayName, c.coords.Name)
	resource := map[string]any{
		"name":              c.coords.Name,
		"displayName":       displayName,
		"shortDescription":  firstNonEmpty(description, displayName),
		"description":       description,
		"trainingFramework": "Other",
		"modelFormat":       "Other",
		"precision":         "OTHER",
		"application":       "OTHER",
		"publicDatasetUsed": map[string]any{},
		"labels":            []string{},
	}
	if err := c.post(ctx, c.url("resources"), resource, nil); err != nil && !isConflict(err) {
		return fmt.Errorf("create resource: %w", err)
	}
	version := map[string]any{"versionId": c.coords.Version, "description": description}
	if err := c.post(ctx, c.url(c.resourcePath("versions")), version, nil); err != nil && !isConflict(err) {
		return fmt.Errorf("create version: %w", err)
	}
	return nil
}

type multipartBody struct {
	Name           string   `json:"name"`
	FilePath       string   `json:"filePath"`
	Version        string   `json:"version"`
	ArtifactType   string   `json:"artifactType"`
	SHA256         string   `json:"sha256"`
	Size           int64    `json:"size"`
	UploadID       string   `json:"uploadID,omitempty"`
	PartNumberList []int32  `json:"partNumberList,omitempty"`
	Etags          []string `json:"etags,omitempty"`
}

type multipartResult struct {
	UploadID string   `json:"uploadID"`
	PartSize int64    `json:"partSize"`
	URLs     []string `json:"urls"`
}

// uploadFile: initiate multipart, PUT parts, complete. 409 = already uploaded.
//
// The returned bool reports that NGC already held this content and no bytes
// went over the wire. NGC keys that store by (org, resource name, sha256) and
// it outlives the versions that reference it — deleting every version of a
// resource does not clear it, only deleting the resource itself does. So a
// conflict says nothing about whether the version being built already had the
// file, and a caller timing an upload needs to know its bytes never moved.
func (c *ngcClient) uploadFile(ctx context.Context, f appFile, uploadWorkers int, log logger) (bool, error) {
	log = ensureLog(log)
	mp := fmt.Sprintf("%s/v2/org/%s/files/multipart", c.baseURL, pathSegment(c.coords.Org))
	body := multipartBody{
		Name:         c.coords.Name,
		FilePath:     f.Path,
		Version:      c.coords.Version,
		ArtifactType: c.coords.Type,
		SHA256:       f.SHA256,
		Size:         f.Size,
	}

	var init multipartResult
	if err := c.post(ctx, mp, body, &init); err != nil {
		if isConflict(err) {
			log("        already in NGC (matching checksum), skipping")
			c.reportProgress(f.Size)
			return true, nil
		}
		return false, fmt.Errorf("initiate upload of %q: %w", f.Path, err)
	}
	if init.UploadID == "" {
		return false, fmt.Errorf("ngc returned no uploadID for %q", f.Path)
	}

	partSize := init.PartSize
	if partSize <= 0 {
		partSize = defaultPartSize(f.Size)
	}
	body.UploadID = init.UploadID
	body.PartNumberList = partNumbers(f.Size, partSize)

	// NGC already answers the initiate call with a presigned URL for every part,
	// so asking again with an explicit partNumberList usually just re-fetches
	// what we hold. Skipping it drops a file from four serial round-trips to
	// three, which is the whole cost on a tree of many small files: those are
	// latency-bound on this API, not on bandwidth.
	//
	// partNumberList is really for requesting a subset — the missing parts of a
	// resumed upload — so the second call stays as a fallback for any response
	// that does not already cover every part.
	urls := init.URLs
	if len(urls) != len(body.PartNumberList) {
		var got multipartResult
		if err := c.post(ctx, mp, body, &got); err != nil {
			if isConflict(err) {
				log("        already in NGC (matching checksum), skipping")
				c.reportProgress(f.Size)
				return true, nil
			}
			return false, fmt.Errorf("get upload URLs for %q: %w", f.Path, err)
		}
		urls = got.URLs
	}
	if len(urls) == 0 {
		return false, fmt.Errorf("ngc returned no upload URLs for %q", f.Path)
	}

	etags, err := c.putParts(ctx, f, urls, partSize, uploadWorkers, log)
	if err != nil {
		return false, err
	}
	body.Etags = etags
	if err := c.send(ctx, http.MethodPut, mp, body, nil); err != nil {
		return false, fmt.Errorf("complete upload of %q: %w", f.Path, err)
	}
	return false, nil
}

func (c *ngcClient) putParts(ctx context.Context, f appFile, urls []string, partSize int64, uploadWorkers int, log logger) ([]string, error) {
	log = ensureLog(log)
	if len(urls) == 0 {
		return nil, fmt.Errorf("no upload URLs for %q", f.Path)
	}
	// Every presigned PUT goes on the part client, single-URL or not. Routing
	// small files onto the HTTP/2 client would multiplex bulk bytes onto one
	// connection, where a shared flow-control window, a single congestion
	// window, and single-threaded TLS all cap throughput. HTTP/2's win is on
	// latency-bound metadata, not on payload.
	hc := c.partClient()
	if len(urls) == 1 {
		return c.putSinglePart(ctx, hc, f, urls[0], partSize)
	}

	workers := workerCount(uploadWorkers, len(urls))
	etags := make([]string, len(urls))
	var (
		next     int64 = -1
		sent     int64
		aborted  int32
		firstErr error
		errOnce  sync.Once
		logMu    sync.Mutex
		wg       sync.WaitGroup
	)
	fail := func(err error) {
		errOnce.Do(func() {
			firstErr = err
			atomic.StoreInt32(&aborted, 1)
		})
	}

	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func() {
			defer wg.Done()
			for atomic.LoadInt32(&aborted) == 0 {
				i := int(atomic.AddInt64(&next, 1))
				if i >= len(urls) {
					return
				}
				offset := int64(i) * partSize
				size := min(partSize, f.Size-offset)
				etag, err := c.putPartAt(ctx, hc, f, urls[i], offset, size)
				if err != nil {
					fail(fmt.Errorf("upload part %d/%d of %q: %w", i+1, len(urls), f.Path, err))
					return
				}
				etags[i] = etag
				partSent := atomic.AddInt64(&sent, size)
				logMu.Lock()
				log("        part %d/%d done (%s/%s)", i+1, len(urls), humanBytes(partSent), humanBytes(f.Size))
				logMu.Unlock()
			}
		}()
	}
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	return etags, nil
}

// putSinglePart uploads a file that NGC presigned as one part. There is no
// part numbering to report, so unlike the parallel path it logs no progress of
// its own.
func (c *ngcClient) putSinglePart(ctx context.Context, hc *http.Client, f appFile, url string, partSize int64) ([]string, error) {
	size := min(partSize, f.Size)
	etag, err := c.putPartAt(ctx, hc, f, url, 0, size)
	if err != nil {
		return nil, fmt.Errorf("upload part 1/1 of %q: %w", f.Path, err)
	}
	return []string{etag}, nil
}

func (c *ngcClient) putPartAt(ctx context.Context, hc *http.Client, f appFile, url string, offset, size int64) (string, error) {
	file, err := os.Open(f.LocalPath)
	if err != nil {
		return "", fmt.Errorf("open %q: %w", f.LocalPath, err)
	}
	defer file.Close()

	// Check via the handle just opened rather than by path: on slow filesystems
	// a path lookup costs milliseconds per file, while fstat on an open
	// descriptor is effectively free.
	info, err := file.Stat()
	if err != nil {
		return "", fmt.Errorf("stat %q: %w", f.LocalPath, err)
	}
	if info.Size() != f.Size || !info.ModTime().Equal(f.ModTime) {
		return "", fmt.Errorf("%w: %q was %s modified %s when hashed, now %s modified %s",
			errFileChanged, f.Path,
			humanBytes(f.Size), f.ModTime.Format(time.RFC3339),
			humanBytes(info.Size()), info.ModTime().Format(time.RFC3339))
	}

	body := &exactReader{r: io.NewSectionReader(file, offset, size), path: f.Path, size: size, left: size}
	return c.putPart(ctx, hc, url, body, size)
}

// errFileChanged marks a file that changed on disk between the hash pass and
// its upload, so something is still writing to it. It is deliberately not
// retryable at the part level: the presigned URL addresses a blob by the digest
// taken during the scan, so only a fresh hash (and a new multipart session) can
// upload the new bytes correctly.
var errFileChanged = errors.New("file changed on disk during upload")

// exactReader reports a body that ends early as errFileChanged, so a file
// truncated mid-upload names its cause instead of surfacing net/http's opaque
// "ContentLength=N with Body length M". It stays seekable so putPart can still
// rewind for ordinary transient retries.
type exactReader struct {
	r          io.ReadSeeker
	path       string
	size, left int64
}

func (e *exactReader) Read(p []byte) (int, error) {
	n, err := e.r.Read(p)
	e.left -= int64(n)
	if errors.Is(err, io.EOF) && e.left > 0 {
		// Sizes here describe this part, not the whole file, which for a
		// multipart upload is larger than any single part.
		return n, fmt.Errorf("%w: %q ended after %s of the %s expected for this part",
			errFileChanged, e.path, humanBytes(e.size-e.left), humanBytes(e.size))
	}
	return n, err
}

func (e *exactReader) Seek(offset int64, whence int) (int64, error) {
	pos, err := e.r.Seek(offset, whence)
	if err == nil {
		e.left = e.size - pos
	}
	return pos, err
}

func (c *ngcClient) putPart(ctx context.Context, hc *http.Client, url string, body io.Reader, size int64) (string, error) {
	seeker, canSeek := body.(io.ReadSeeker)
	var lastErr error
	for attempt := 0; attempt < uploadPartMaxAttempts; attempt++ {
		if attempt > 0 {
			if err := rewindForRetry(ctx, seeker, canSeek, attempt, lastErr); err != nil {
				return "", err
			}
		}
		etag, err := c.putPartOnce(ctx, hc, url, body, size)
		if err == nil {
			return etag, nil
		}
		if !isRetryableTransferErr(err) {
			return "", err
		}
		lastErr = err
	}
	return "", lastErr
}

// rewindForRetry prepares body for another attempt. The part has to be re-read
// from the start, so a body that cannot seek cannot be retried at all; lastErr
// is what the previous attempt failed with.
func rewindForRetry(ctx context.Context, seeker io.ReadSeeker, canSeek bool, attempt int, lastErr error) error {
	if !canSeek {
		return fmt.Errorf("upload part retry requires seekable body: %w", lastErr)
	}
	if _, err := seeker.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("rewind upload body: %w", err)
	}
	return transferBackoff(ctx, attempt)
}

func (c *ngcClient) putPartOnce(ctx context.Context, hc *http.Client, url string, body io.Reader, size int64) (etag string, err error) {
	reqCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Watch for a wedged connection: net/http stops reading the body when the
	// socket write blocks, so an absence of Reads signals no progress. On
	// timeout we cancel the request and report a retryable sentinel rather
	// than letting the worker block indefinitely.
	var stalled atomic.Bool
	if size > 0 && c.stallTimeout > 0 {
		sr := &stallReader{r: body, timeout: c.stallTimeout}
		sr.timer = time.AfterFunc(c.stallTimeout, func() {
			stalled.Store(true)
			cancel()
		})
		defer sr.timer.Stop()
		body = sr
	}

	// Report bytes as the transport reads them so progress tracks parts that
	// are still in flight instead of jumping when whole files land. A failed
	// attempt unwinds its own contribution, so the retry cannot double count.
	if size > 0 && c.progress != nil {
		pr := &progressReader{r: body, report: c.progress}
		body = pr
		defer func() {
			if err != nil {
				c.reportProgress(-pr.count())
			}
		}()
	}

	// An empty part must go out with an explicit "Content-Length: 0". Go treats
	// a non-nil body with ContentLength 0 as unknown length and switches to
	// chunked encoding, which omits the header that the presigned URL signs
	// over (X-Amz-SignedHeaders=content-length;host), so S3 rejects the PUT
	// with SignatureDoesNotMatch. http.NoBody makes the length known.
	if size == 0 {
		body = http.NoBody
	}

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPut, url, body)
	if err != nil {
		return "", err
	}
	req.ContentLength = size
	resp, err := hc.Do(req)
	if err != nil {
		if stalled.Load() {
			return "", errTransferStalled
		}
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return "", &transferStatusError{
			Method: http.MethodPut,
			Code:   resp.StatusCode,
			Status: resp.Status,
			Body:   strings.TrimSpace(string(snippet)),
		}
	}
	return strings.Trim(resp.Header.Get("ETag"), `"`), nil
}

// transferStatusError is returned when a presigned request yields a non-2xx
// response. Carrying the numeric status lets callers decide retryability by
// value instead of parsing the formatted message.
type transferStatusError struct {
	Method string // the HTTP method, as it reads back in the message
	Code   int
	Status string
	Body   string
}

func (e *transferStatusError) Error() string {
	method := firstNonEmpty(e.Method, "request")
	if e.Body == "" {
		return fmt.Sprintf("presigned %s returned %s", method, e.Status)
	}
	return fmt.Sprintf("presigned %s returned %s: %s", method, e.Status, e.Body)
}

// errTransferStalled marks a transfer whose connection made no I/O progress
// within the stall timeout. It is retryable: the link wedged, the caller did
// not cancel (which would surface as context.Canceled instead).
var errTransferStalled = errors.New("transfer stalled: no I/O progress within timeout")

// stallReader wraps a transfer body and resets a watchdog timer on every read
// with forward progress. When the timer fires (no reads within timeout) it
// invokes the AfterFunc callback that cancels the in-flight request. It serves
// both directions: net/http reads a request body to send it, and a download
// reads the response body to write it out.
type stallReader struct {
	r       io.Reader
	timeout time.Duration
	timer   *time.Timer
}

func (s *stallReader) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	if n > 0 {
		s.timer.Reset(s.timeout)
	}
	return n, err
}

// progressReader reports each read to report and remembers the running total so
// a failed attempt can be subtracted back out. Reads happen as the transport
// drains the body onto the wire, which is what makes in-flight parts visible.
type progressReader struct {
	r      io.Reader
	report progressFunc
	n      atomic.Int64
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	if n > 0 {
		p.n.Add(int64(n))
		p.report(int64(n))
	}
	return n, err
}

func (p *progressReader) count() int64 { return p.n.Load() }

func transferBackoff(ctx context.Context, attempt int) error {
	delay := uploadPartRetryBase * time.Duration(1<<(attempt-1))
	if delay > uploadPartRetryMax {
		delay = uploadPartRetryMax
	}
	delay += time.Duration(rand.Int64N(int64(delay / 4)))
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// isRetryableTransferErr reports whether a presigned-transfer failure is
// transient, in either direction. It classifies by error value/type (via
// errors.Is/errors.As) rather than by scanning the formatted message, which is
// both more precise and immune to wording changes in the Go runtime or AWS
// responses.
func isRetryableTransferErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}

	if errors.Is(err, errFileChanged) {
		return false
	}

	// Non-2xx from the presigned request: retry only transient server-side
	// codes.
	var statusErr *transferStatusError
	if errors.As(err, &statusErr) {
		switch statusErr.Code {
		case http.StatusRequestTimeout, // 408
			http.StatusTooManyRequests,     // 429
			http.StatusInternalServerError, // 500
			http.StatusBadGateway,          // 502
			http.StatusServiceUnavailable,  // 503
			http.StatusGatewayTimeout:      // 504
			return true
		}
		return false
	}

	// Connection wedged with no progress and our watchdog cancelled it.
	if errors.Is(err, errTransferStalled) {
		return true
	}

	// Peer closed the connection before responding — classic transient
	// S3/proxy behavior on long uploads. http.Client wraps these in
	// *url.Error, so errors.Is unwraps reliably.
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}

	// Connection-level failures, matched by sentinel value rather than text.
	if errors.Is(err, net.ErrClosed) || // "use of closed network connection"
		errors.Is(err, syscall.ECONNRESET) || // "connection reset by peer"
		errors.Is(err, syscall.ECONNREFUSED) || // "connection refused"
		errors.Is(err, syscall.EPIPE) { // "broken pipe"
		return true
	}

	// Any timeout the net stack reports (dial, TLS handshake, read/write,
	// "i/o timeout") implements net.Error with Timeout() == true.
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	return false
}

// commitOpts controls a files/commit call. The plain upload path uses
// SetLatest with upload-pending false (one-shot complete). The base-version
// update path uses UploadPending commits plus a later markUploadComplete.
type commitOpts struct {
	Files         []appFile
	BaseVersion   string // server-side inherit; empty = none
	UploadPending bool   // true = leave version open for more commits
	SetLatest     bool   // only applied when UploadPending is false
}

func (c *ngcClient) commit(ctx context.Context, files []appFile, setLatest bool) error {
	return c.commitFiles(ctx, commitOpts{Files: files, SetLatest: setLatest})
}

func (c *ngcClient) commitFiles(ctx context.Context, opts commitOpts) error {
	type meta struct {
		SHA256     string `json:"sha256"`
		CommitType string `json:"commitType"`
		FilePath   string `json:"filePath"`
	}
	list := make([]meta, len(opts.Files))
	for i, f := range opts.Files {
		list[i] = meta{f.SHA256, "ADDITION", f.Path}
	}
	body := map[string]any{
		"files":        list,
		"artifactType": c.coords.Type,
		"version":      c.coords.Version,
		"name":         c.coords.Name,
	}
	if opts.BaseVersion != "" {
		body["baseVersion"] = opts.BaseVersion
	}
	var url string
	if opts.UploadPending {
		url = c.url("files/commit") + "?upload-pending=true"
	} else {
		url = fmt.Sprintf("%s?set-latest=%t", c.url("files/commit"), opts.SetLatest)
	}

	hc := commitHTTPClient(c.http)
	if hc != c.http {
		defer hc.CloseIdleConnections()
	}
	req := jsonRequest{hc: hc, apiKey: c.apiKey, method: http.MethodPost, url: url, in: body}
	if err := httpJSONRetry(ctx, req,
		retryPolicy{log: c.retryLog, throttle: c.throttle}); err != nil {
		// NGC answers 409 once a version's upload is marked complete. Reaching
		// that state means an earlier attempt landed and only its response was
		// lost, so the commit this call was asked to make has already happened.
		if isConflict(err) {
			if c.retryLog != nil {
				c.retryLog("commit had already completed for %s; an earlier attempt landed but its response was lost", c.coords.uri())
			}
			return nil
		}
		return fmt.Errorf("commit files: %w", err)
	}
	return nil
}

// markUploadComplete finalizes a version left open by upload-pending commits.
// Mirrors ngcsdk's PATCH {"status":"UPLOAD_COMPLETE"} after a base-version upload.
func (c *ngcClient) markUploadComplete(ctx context.Context, setLatest bool) error {
	url := c.url(c.resourcePath("versions", c.coords.Version))
	if setLatest {
		url += "?set-latest=true"
	}
	body := map[string]any{"status": "UPLOAD_COMPLETE"}
	if err := c.send(ctx, http.MethodPatch, url, body, nil); err != nil {
		if isConflict(err) {
			if c.retryLog != nil {
				c.retryLog("version %s already marked UPLOAD_COMPLETE", c.coords.uri())
			}
			return nil
		}
		return fmt.Errorf("mark upload complete: %w", err)
	}
	return nil
}

// validateBaseVersion checks that baseVersion exists on this resource, is
// UPLOAD_COMPLETE, and has at least one file — the same gates ngcsdk enforces
// before a base-version upload.
func (c *ngcClient) validateBaseVersion(ctx context.Context, baseVersion string) error {
	if baseVersion == "" {
		return fmt.Errorf("base version is required")
	}
	if strings.Contains(baseVersion, "/") || strings.Contains(baseVersion, ":") {
		return fmt.Errorf("base version must be a version id (e.g. 1.0.0), not %q", baseVersion)
	}
	versions, err := c.listVersions(ctx)
	if err != nil {
		return err
	}
	var found *ngcResourceVersion
	for i := range versions {
		if versions[i].version() == baseVersion {
			found = &versions[i]
			break
		}
	}
	if found == nil {
		return fmt.Errorf("base version %q not found on resource %s", baseVersion, c.coords.Name)
	}
	status := strings.ToUpper(found.Status)
	if status != "" && status != "UPLOAD_COMPLETE" {
		return fmt.Errorf("base version %q is %s; must be UPLOAD_COMPLETE", baseVersion, found.Status)
	}
	// NGC rejects base versions with no files. TotalFileCount is a pointer so a
	// missing field (sparse list payload) is not mistaken for a real zero.
	if status == "UPLOAD_COMPLETE" && found.TotalFileCount != nil && *found.TotalFileCount == 0 {
		return fmt.Errorf("base version %q has no files", baseVersion)
	}
	return nil
}

// commitHTTPClient adapts an upload-tuned client for the manifest commit: one
// long server-side operation rather than a stream of part uploads. Clients
// whose transport is not an *http.Transport (tests, callers that inject their
// own) are used unchanged.
func commitHTTPClient(hc *http.Client) *http.Client {
	t, ok := hc.Transport.(*http.Transport)
	if !ok {
		return hc
	}
	ct := t.Clone()
	ct.ResponseHeaderTimeout = commitResponseHeaderTimeout
	return &http.Client{Transport: ct, Timeout: hc.Timeout}
}

// ngcResource is a single NGC resource (a "recipe" in the registry metadata API).
type ngcResource struct {
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
	Description string `json:"description"`
	OrgName     string `json:"orgName"`
	UpdatedDate string `json:"updatedDate"`
}

// fetchAllPages accumulates the results of a paginated NGC listing. fetch
// requests a single zero-based page and returns that page's items plus the
// server-reported total page count (0 when unknown). Iteration stops on an
// empty page or once the reported total is reached; maxListPages bounds a
// misbehaving pager and yields an error rather than silently truncating.
func fetchAllPages[T any](fetch func(page int) (items []T, totalPages int, err error)) ([]T, error) {
	var all []T
	for page := 0; page < maxListPages; page++ {
		items, totalPages, err := fetch(page)
		if err != nil {
			return nil, err
		}
		all = append(all, items...)
		if len(items) == 0 || (totalPages > 0 && page+1 >= totalPages) {
			return all, nil
		}
	}
	return nil, fmt.Errorf("listing exceeded the %d-page limit", maxListPages)
}

// listResources returns every RESOURCE in the org, following pagination.
func (c *ngcClient) listResources(ctx context.Context) ([]ngcResource, error) {
	const pageSize = 100
	return fetchAllPages(func(page int) ([]ngcResource, int, error) {
		var out struct {
			Recipes        []ngcResource `json:"recipes"`
			PaginationInfo struct {
				TotalPages int `json:"totalPages"`
			} `json:"paginationInfo"`
		}
		url := fmt.Sprintf("%s?page-number=%d&page-size=%d", c.url("resources"), page, pageSize)
		if err := c.send(ctx, http.MethodGet, url, nil, &out); err != nil {
			return nil, 0, fmt.Errorf("list resources: %w", err)
		}
		return out.Recipes, out.PaginationInfo.TotalPages, nil
	})
}

// ngcResourceVersion is one version of an NGC resource. Field names vary
// across NGC responses, so both common spellings are accepted.
type ngcResourceVersion struct {
	VersionID   string `json:"versionId"`
	ID          string `json:"id"`
	Status      string `json:"status"`
	CreatedDate string `json:"createdDate"`
	UpdatedDate string `json:"updatedDate"`
	// TotalFileCount is a pointer: list payloads may omit it, and a missing
	// field must not read as a real zero (see validateBaseVersion).
	TotalFileCount   *int64 `json:"totalFileCount"`
	TotalSizeInBytes int64  `json:"totalSizeInBytes"`
}

// fileCount reports the listed file count, or 0 when the field was absent.
func (v ngcResourceVersion) fileCount() int64 {
	if v.TotalFileCount == nil {
		return 0
	}
	return *v.TotalFileCount
}

func (v ngcResourceVersion) version() string { return firstNonEmpty(v.VersionID, v.ID) }

// listVersions returns every version of the resource named in the client
// coordinates, following pagination.
func (c *ngcClient) listVersions(ctx context.Context) ([]ngcResourceVersion, error) {
	const pageSize = 100
	return fetchAllPages(func(page int) ([]ngcResourceVersion, int, error) {
		var out struct {
			RecipeVersions []ngcResourceVersion `json:"recipeVersions"`
			Versions       []ngcResourceVersion `json:"versions"`
			PaginationInfo struct {
				TotalPages int `json:"totalPages"`
			} `json:"paginationInfo"`
		}
		suffix := fmt.Sprintf("resources/%s/versions?page-number=%d&page-size=%d", pathSegment(c.coords.Name), page, pageSize)
		if err := c.send(ctx, http.MethodGet, c.url(suffix), nil, &out); err != nil {
			return nil, 0, fmt.Errorf("list resource versions: %w", err)
		}
		batch := out.RecipeVersions
		if len(batch) == 0 {
			batch = out.Versions
		}
		return batch, out.PaginationInfo.TotalPages, nil
	})
}

func (c *ngcClient) deleteVersion(ctx context.Context) error {
	url := c.url(c.resourcePath("versions", c.coords.Version))
	if err := c.send(ctx, http.MethodDelete, url, nil, nil); err != nil {
		return fmt.Errorf("delete version: %w", err)
	}
	return nil
}

func (c *ngcClient) deleteResource(ctx context.Context) error {
	if err := c.send(ctx, http.MethodDelete, c.url(c.resourcePath()), nil, nil); err != nil {
		return fmt.Errorf("delete resource: %w", err)
	}
	return nil
}

func (c *ngcClient) url(suffix string) string {
	return fmt.Sprintf("%s/v2/org/%s/%s", c.baseURL, pathSegment(c.coords.Org), suffix)
}

// resourcePath is the url suffix for this client's resource, with any extra
// segments joined onto it: resourcePath("versions", v) -> "resources/N/versions/v".
func (c *ngcClient) resourcePath(segments ...string) string {
	parts := []string{"resources", pathSegment(c.coords.Name)}
	for _, s := range segments {
		parts = append(parts, pathSegment(s))
	}
	return strings.Join(parts, "/")
}

func (c *ngcClient) post(ctx context.Context, url string, in, out any) error {
	return c.send(ctx, http.MethodPost, url, in, out)
}

func (c *ngcClient) send(ctx context.Context, method, url string, in, out any) error {
	req := jsonRequest{hc: c.http, apiKey: c.apiKey, method: method, url: url, in: in, out: out}
	return httpJSONRetry(ctx, req, retryPolicy{log: c.retryLog, throttle: c.throttle})
}

// hash records the base64 sha256, size, and mtime of the file at LocalPath
// (NGC expects base64, not hex). The mtime is read from the same handle after
// hashing, so the three describe one snapshot that the upload can later verify
// against without paying for a second path lookup. Nothing is recorded unless
// the whole snapshot is taken. It honours ctx between reads so Ctrl+C
// interrupts a scan promptly even when a single multi-gigabyte file is
// mid-hash.
func (f *appFile) hash(ctx context.Context) error {
	file, err := os.Open(f.LocalPath)
	if err != nil {
		return err
	}
	defer file.Close()

	h := sha256.New()
	// 1 MiB buffer for slow mounts, which also bounds how long a cancellation
	// waits: at most one buffer-sized read.
	n, err := io.CopyBuffer(h, ctxReader{ctx: ctx, r: file}, make([]byte, 1<<20))
	if err != nil {
		return err
	}
	info, err := file.Stat()
	if err != nil {
		return err
	}
	f.SHA256, f.Size, f.ModTime = base64.StdEncoding.EncodeToString(h.Sum(nil)), n, info.ModTime()
	return nil
}

// ctxReader makes a long sequential read cancellable. io.Copy has no context
// awareness, so without this a single large file would run to completion after
// the user has already asked to stop.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

const (
	minPartSize = int64(5 * 1024 * 1024)
	maxParts    = int64(10000)
	defPartSize = int64(16 * 1024 * 1024)

	// downloadChunkSize is how much one ranged GET asks for. It matches the
	// default upload part size, which is the granularity NGC's own store is
	// built around, and it is large enough that the per-request overhead
	// disappears against the transfer while staying small enough to keep every
	// chunk worker fed on a file only a few chunks long.
	downloadChunkSize = int64(16 * 1024 * 1024)
	// downloadRangeMinSize is the size at which splitting a file starts to pay.
	// Below two full chunks the parallel path costs an extra request and buys
	// nothing, so smaller files take the single-stream route.
	downloadRangeMinSize = 2 * downloadChunkSize
)

func defaultPartSize(size int64) int64 {
	ps := defPartSize
	for size/ps+1 > maxParts {
		ps *= 2
	}
	return max(ps, minPartSize)
}

func partNumbers(size, partSize int64) []int32 {
	n := size / partSize
	if size%partSize != 0 || n == 0 {
		n++
	}
	parts := make([]int32, n)
	for i := range parts {
		parts[i] = int32(i + 1)
	}
	return parts
}
