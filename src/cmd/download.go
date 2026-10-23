// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// downloadCopyBufferSize is the staging buffer one transfer copies through.
// Peak allocation is one of these per in-flight transfer (file workers ×
// chunk workers), so it is deliberately modest: the transport already reads
// into a buffer of its own (downloadReadBufferSize), and a larger buffer here
// only adds memory without removing a syscall.
const downloadCopyBufferSize = 256 << 10

// errDownloadFmt wraps a failure with the remote path it was downloading.
const errDownloadFmt = "download %q: %w"

// ngcFileEntry is one file in a resource version. Field names vary across NGC
// responses, so both common spellings are accepted.
type ngcFileEntry struct {
	Path         string `json:"path"`
	RelativePath string `json:"relativePath"`
	SizeInBytes  int64  `json:"sizeInBytes"`
	Size         int64  `json:"size"`
	// A per-file digest is optional: whether the listing carries one depends on
	// the NGC deployment, and it is spelled differently when it does. Nothing
	// here requires it — without one, --skip-existing has nothing to verify a
	// local copy against and says so rather than guessing from the size.
	Checksum string `json:"checksum"`
	SHA256   string `json:"sha256"`
	Digest   string `json:"digest"`
}

func (e ngcFileEntry) filePath() string { return firstNonEmpty(e.Path, e.RelativePath) }

func (e ngcFileEntry) fileSize() int64 {
	if e.SizeInBytes > 0 {
		return e.SizeInBytes
	}
	return e.Size
}

func (e ngcFileEntry) checksum() string { return firstNonEmpty(e.Checksum, e.SHA256, e.Digest) }

// listingHasChecksums reports whether any entry came with a digest, which is
// what verifying a local copy depends on.
func listingHasChecksums(files []ngcFileEntry) bool {
	for _, f := range files {
		if f.checksum() != "" {
			return true
		}
	}
	return false
}

// localCopyMatches reports whether dest already holds exactly the content the
// listing describes for e.
//
// The digest is the only safe basis for that answer: NGC keys its content store
// by checksum, and a file of the right length says nothing about its bytes. The
// size is still consulted first because it is free and settles almost every
// mismatch without reading anything. Any trouble reading the local copy counts
// as "not a match", leaving it to be downloaded as usual.
func localCopyMatches(ctx context.Context, dest string, e ngcFileEntry) bool {
	remote := e.checksum()
	if remote == "" {
		return false
	}
	info, err := os.Stat(dest)
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	if size := e.fileSize(); size > 0 && info.Size() != size {
		return false
	}
	local := appFile{LocalPath: dest}
	if err := local.hash(ctx); err != nil {
		return false
	}
	return sameDigest(remote, local.SHA256)
}

// sameDigest reports whether a digest from the registry describes the same
// content as a locally computed base64 sha256. The upload side commits base64,
// so that is the expected spelling; hex is recognised as well, since the
// alternative is a comparison that silently never matches.
func sameDigest(remote, localBase64 string) bool {
	remote = strings.TrimPrefix(strings.TrimSpace(remote), "sha256:")
	if remote == "" || localBase64 == "" {
		return false
	}
	if remote == localBase64 {
		return true
	}
	if raw, err := hex.DecodeString(remote); err == nil {
		return base64.StdEncoding.EncodeToString(raw) == localBase64
	}
	return false
}

// ngcVersionFiles is the resource-version file listing response.
type ngcVersionFiles struct {
	Status         string         `json:"status"`
	RecipeFiles    []ngcFileEntry `json:"recipeFiles"`
	Files          []ngcFileEntry `json:"files"`
	PaginationInfo struct {
		TotalPages int `json:"totalPages"`
	} `json:"paginationInfo"`
}

func (v ngcVersionFiles) entries() []ngcFileEntry {
	if len(v.RecipeFiles) > 0 {
		return v.RecipeFiles
	}
	return v.Files
}

// ngcDebug reports whether verbose NGC HTTP debugging is enabled (GDN_DEBUG).
func ngcDebug() bool { return os.Getenv("GDN_DEBUG") != "" }

// listVersionFiles returns the files in the resource version identified by the
// client coordinates, following pagination (the listing is capped per page, so
// a single request would miss files on large versions).
func (c *ngcClient) listVersionFiles(ctx context.Context) ([]ngcFileEntry, error) {
	const pageSize = 1000
	entries, err := fetchAllPages(func(page int) ([]ngcFileEntry, int, error) {
		suffix := fmt.Sprintf("resources/%s/versions/%s/files?page-number=%d&page-size=%d",
			pathSegment(c.coords.Name), pathSegment(c.coords.Version), page, pageSize)
		endpoint := c.url(suffix)
		if ngcDebug() && page == 0 {
			if raw, err := c.debugGetRaw(ctx, endpoint); err == nil {
				logStderr("[debug] GET %s ->\n%s", endpoint, string(raw))
			} else {
				logStderr("[debug] GET %s failed: %v", endpoint, err)
			}
		}
		var out ngcVersionFiles
		if err := c.send(ctx, http.MethodGet, endpoint, nil, &out); err != nil {
			return nil, 0, fmt.Errorf("list resource files: %w", err)
		}
		return out.entries(), out.PaginationInfo.TotalPages, nil
	})
	if err != nil {
		return nil, err
	}
	// Drop directory/placeholder entries that carry no file path.
	files := make([]ngcFileEntry, 0, len(entries))
	for _, e := range entries {
		if e.filePath() != "" {
			files = append(files, e)
		}
	}
	return files, nil
}

// downloadOptions configures one parallel download run.
type downloadOptions struct {
	OutDir string
	// FileWorkers bounds how many files transfer at once; <=0 selects
	// defaultDownloadWorkers.
	FileWorkers int
	// ChunkWorkers is a per-file budget for the ranged GETs a large file is
	// split into, so peak concurrent transfers is FileWorkers × this. <=0 means
	// one per CPU.
	ChunkWorkers int
	// SkipExisting leaves a file alone when the local copy's digest already
	// matches the listing. It needs the listing to carry digests; without them
	// nothing is skipped.
	SkipExisting bool
}

// downloadSummary is what one parallel download run produced. Skipped files are
// reported separately because their bytes never crossed the wire, so counting
// them as transferred would make the elapsed time meaningless as a measure of
// throughput.
type downloadSummary struct {
	Files        []downloadFileInfo
	TotalBytes   int64
	SkippedFiles int
	SkippedBytes int64
}

// downloadFilesParallel downloads every file into opts.OutDir with the
// configured worker pool, returning what landed on disk in listing order.
//
// A tree of many small files is latency-bound on the NGC API rather than
// bandwidth-bound, exactly as an upload of the same tree is: no file's bytes
// can start moving until a presign round-trip has said where they live, so
// throughput is workers/round-trips and the only lever is how many of those
// chains run at once. The presign calls ride the HTTP/2 API client, which
// multiplexes them over a single connection, and apiThrottle paces the whole
// pool back if NGC starts answering 429.
func downloadFilesParallel(ctx context.Context, ngc *ngcClient, files []ngcFileEntry, opts downloadOptions, log logger) (downloadSummary, error) {
	log = ensureLog(log)

	fileWorkers := opts.FileWorkers
	if fileWorkers <= 0 {
		fileWorkers = defaultDownloadWorkers
	}
	workers := workerCount(fileWorkers, len(files))

	var total int64
	for _, f := range files {
		total += f.fileSize()
	}
	log("downloading %d file(s), %s with %d worker(s) ...", len(files), humanBytes(total), workers)

	p := &downloadPool{
		ngc:     ngc,
		files:   files,
		total:   total,
		opts:    opts,
		log:     log,
		bytes:   make([]int64, len(files)),
		skipped: make([]bool, len(files)),
	}
	p.next.Store(-1)

	// Count bytes as they come off the wire rather than when a file finishes,
	// so the percentage reflects transfers still in flight. Without this a
	// handful of large files hold the total flat and then snap it to 100%.
	ngc.progress = func(n int64) { p.received.Add(n) }
	// Wire the retry logger before the first request: a broken transfer backs
	// off for seconds at a time, which looks like a hang without output. It
	// goes through safeLog because every worker shares it.
	ngc.retryLog = func(format string, args ...any) { p.safeLog("        "+format, args...) }
	defer func() {
		ngc.progress = nil
		ngc.retryLog = nil
	}()

	// The first failure cancels this context so in-flight transfers stop
	// instead of running to completion before wg.Wait can return.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	p.cancel = cancel

	var wg sync.WaitGroup
	wg.Add(workers)
	for range workers {
		go func() {
			defer wg.Done()
			p.work(ctx)
		}()
	}
	wg.Wait()
	if p.firstErr != nil {
		return downloadSummary{}, p.firstErr
	}

	sum := downloadSummary{Files: make([]downloadFileInfo, len(files))}
	for i, f := range files {
		sum.Files[i] = downloadFileInfo{Path: f.filePath(), Bytes: p.bytes[i], Skipped: p.skipped[i]}
		if p.skipped[i] {
			sum.SkippedFiles++
			sum.SkippedBytes += p.bytes[i]
			continue
		}
		sum.TotalBytes += p.bytes[i]
	}
	landed := sum.TotalBytes + sum.SkippedBytes
	log("[%d/%d] 100.0%% (%s/%s) download complete",
		len(files), len(files), humanBytes(landed), humanBytes(landed))
	return sum, nil
}

// downloadPool is the shared state of one parallel download run: the cursor
// into files, the byte count as it comes off the wire, the first error (which
// aborts the rest), and the per-file byte counts indexed alongside files.
type downloadPool struct {
	ngc   *ngcClient
	files []ngcFileEntry
	total int64
	opts  downloadOptions
	log   logger

	next     atomic.Int64
	received atomic.Int64
	aborted  atomic.Int32
	firstErr error
	errOnce  sync.Once
	cancel   context.CancelFunc
	logMu    sync.Mutex
	bytes    []int64
	skipped  []bool
}

func (p *downloadPool) fail(err error) {
	p.errOnce.Do(func() {
		p.firstErr = err
		p.aborted.Store(1)
		p.cancel()
	})
}

// safeLog serializes the progress lines emitted from the parallel workers.
func (p *downloadPool) safeLog(format string, args ...any) {
	p.logMu.Lock()
	defer p.logMu.Unlock()
	p.log(format, args...)
}

// work downloads successive files until the list is exhausted or another worker
// fails.
func (p *downloadPool) work(ctx context.Context) {
	for p.aborted.Load() == 0 {
		i := int(p.next.Add(1))
		if i >= len(p.files) {
			return
		}
		if err := p.downloadOne(ctx, i); err != nil {
			p.fail(err)
			return
		}
	}
}

// downloadOne downloads files[i] and records how many bytes it wrote. Each
// index is owned by exactly one worker, and the slice is only read again after
// wg.Wait, so that write needs no extra synchronization.
func (p *downloadPool) downloadOne(ctx context.Context, i int) error {
	f := p.files[i]
	rel := f.filePath()
	dest, err := safeJoin(p.opts.OutDir, rel)
	if err != nil {
		return fmt.Errorf("refusing to download %q: %w", rel, err)
	}

	// Re-reading a file to hash it costs a fraction of fetching it again, so a
	// re-run over a mostly-intact tree is worth the local pass.
	if p.opts.SkipExisting && localCopyMatches(ctx, dest, f) {
		p.bytes[i], p.skipped[i] = f.fileSize(), true
		// Count it toward the running total anyway: the percentage tracks bytes
		// landed on disk, and these are already there.
		p.received.Add(f.fileSize())
		p.safeLog("[%d/%d] already up to date, skipping %s", i+1, len(p.files), rel)
		return nil
	}

	p.safeLog("[%d/%d] downloading %s (%s)", i+1, len(p.files), rel, humanBytes(f.fileSize()))
	n, err := p.ngc.downloadFile(ctx, rel, dest, f.fileSize(), p.opts.ChunkWorkers)
	if err != nil {
		return err
	}
	p.bytes[i] = n
	// ngc.progress already counted this file's bytes as they arrived, so read
	// the running total rather than adding again.
	done := p.received.Load()
	p.safeLog("[%d/%d] %5.1f%% (%s/%s) done %s",
		i+1, len(p.files), percent(done, p.total), humanBytes(done), humanBytes(p.total), rel)
	return nil
}

// downloadFile fetches a single file (by its resource-relative path) to dest.
// size is what the file listing reported for it, and 0 when the listing did not
// say; chunkWorkers bounds how many ranged GETs a large file is split into.
func (c *ngcClient) downloadFile(ctx context.Context, remotePath, dest string, size int64, chunkWorkers int) (int64, error) {
	res, err := c.presignDownload(ctx, remotePath)
	if err != nil {
		return 0, err
	}

	var n int64
	if res.body != nil {
		defer res.body.Close()
		n, err = c.writeBodyToFile(res.body, dest, size)
	} else {
		n, err = c.streamURLToFile(ctx, res.url, dest, size, chunkWorkers)
	}
	if err != nil {
		return 0, fmt.Errorf(errDownloadFmt, remotePath, err)
	}
	return n, nil
}

// writeBodyToFile writes a body NGC streamed directly, under the same stall
// watchdog as the presigned-URL path. That body is tied to the presign
// request's context, which is out of reach here, so a stall closes the body to
// unblock the read instead of cancelling the request.
func (c *ngcClient) writeBodyToFile(body io.ReadCloser, dest string, size int64) (int64, error) {
	if c.stallTimeout <= 0 {
		return writeToFile(body, dest, size)
	}
	var stalled atomic.Bool
	sr := &stallReader{r: body, timeout: c.stallTimeout}
	sr.timer = time.AfterFunc(c.stallTimeout, func() {
		stalled.Store(true)
		body.Close()
	})
	defer sr.timer.Stop()

	n, err := writeToFile(sr, dest, size)
	if err != nil && stalled.Load() {
		return n, errTransferStalled
	}
	return n, err
}

// verifyTransferred checks a finished transfer against the size the listing
// promised. It runs before the ".part" file is allowed to become the real one:
// a short transfer that somehow reported success would otherwise leave a
// truncated file behind looking every bit as complete as the genuine article.
// A listing that reported no size at all leaves nothing to check against.
func verifyTransferred(got, want int64) error {
	if want > 0 && got != want {
		return fmt.Errorf("got %s, expected %s", humanBytes(got), humanBytes(want))
	}
	return nil
}

// presignResult is what the CAS-proxy download route answered: either a
// presigned URL to stream the bytes from, or (the defensive fallback) the bytes
// themselves, in which case body is non-nil and the caller owns closing it.
type presignResult struct {
	url  string
	body io.ReadCloser
}

// presignDownload asks the download route where remotePath's bytes live.
//
// This mirrors the NGC SDK's direct-download flow: the file is addressed via
// the CAS-proxy download route
// /v2/org/{org}/resources/{name}/{version}/files?path={filepath} (note: no
// "versions/" segment, and the path is a query parameter). That route returns a
// JSON body containing presigned S3 URL(s), which is what the caller then
// streams from. As a defensive fallback, a direct byte/redirect response is
// also handled, which is why the body comes back unread rather than the URL
// alone.
//
// The retry loop is written out here rather than delegating to httpJSONRetry
// because that helper reads and closes every body in order to decode it, which
// would consume the bytes of exactly the fallback this route has to tolerate.
// The shared throttle is still fed, so one worker's 429 paces the whole pool
// instead of each worker backing off in isolation.
func (c *ngcClient) presignDownload(ctx context.Context, remotePath string) (presignResult, error) {
	if err := validateRemotePath(remotePath); err != nil {
		return presignResult{}, fmt.Errorf(errDownloadFmt, remotePath, err)
	}
	suffix := fmt.Sprintf("resources/%s/%s/files?path=%s",
		pathSegment(c.coords.Name), pathSegment(c.coords.Version), url.QueryEscape(remotePath))
	endpoint := c.url(suffix)
	if ngcDebug() {
		logStderr("[debug] download GET %s", endpoint)
	}

	for attempt := 0; ; attempt++ {
		if err := c.throttle.wait(ctx); err != nil {
			return presignResult{}, err
		}
		res, retryAfter, retry, err := c.presignAttempt(ctx, endpoint, remotePath)
		if err == nil {
			return res, nil
		}
		if !retry || attempt >= retryMaxAttempts-1 {
			return presignResult{}, err
		}
		if werr := waitRetry(ctx, attempt, retryAfter, c.retryLog, "%s", err); werr != nil {
			return presignResult{}, werr
		}
	}
}

// presignAttempt makes one presign request and classifies the outcome. It never
// sleeps; the backoff belongs to the caller's loop, which owns the attempt
// count. retry reports whether another attempt is worth making.
func (c *ngcClient) presignAttempt(ctx context.Context, endpoint, remotePath string) (res presignResult, retryAfter time.Duration, retry bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return presignResult{}, 0, false, err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		// A cancelled context will not be any healthier next attempt.
		if ctx.Err() != nil {
			return presignResult{}, 0, false, err
		}
		return presignResult{}, 0, isRetryableTransferErr(err),
			fmt.Errorf(errDownloadFmt, remotePath, err)
	}

	retryAfter = parseRetryAfter(resp.Header.Get("Retry-After"))
	switch {
	case isPushbackStatus(resp.StatusCode):
		c.throttle.pushback(retryAfter)
	case !isRetryableStatus(resp.StatusCode):
		// Any definitive answer is evidence the server is not overloaded. See
		// apiThrottle.served.
		c.throttle.served()
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		resp.Body.Close()
		return presignResult{}, retryAfter, isRetryableStatus(resp.StatusCode),
			fmt.Errorf("download %q: %s: %s", remotePath, resp.Status, strings.TrimSpace(string(snippet)))
	}

	// A non-JSON body means NGC streamed the bytes directly, so it is handed
	// back unread for the caller to write out.
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(strings.ToLower(ct), "json") {
		return presignResult{body: resp.Body}, 0, false, nil
	}

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	resp.Body.Close()
	signed := parseDownloadURL(raw)
	if signed == "" {
		return presignResult{}, 0, false,
			fmt.Errorf("download %q: no download URL in response: %s", remotePath, strings.TrimSpace(string(raw)))
	}
	return presignResult{url: signed}, 0, false, nil
}

// debugGetRaw performs an authenticated GET and returns the raw response body,
// prefixed with the status line. Used only for GDN_DEBUG diagnostics.
func (c *ngcClient) debugGetRaw(ctx context.Context, endpoint string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return append([]byte(resp.Status+" ["+resp.Header.Get("Content-Type")+"]\n"), body...), nil
}

// streamURLToFile GETs an already-authorized (presigned) URL and writes it to
// dest. No Authorization header is sent because presigned URLs carry their own
// credentials in the query string.
func (c *ngcClient) streamURLToFile(ctx context.Context, signedURL, dest string, size int64, chunkWorkers int) (int64, error) {
	pf, err := newPartFile(dest)
	if err != nil {
		return 0, err
	}
	defer pf.discard()

	n, err := c.fetchToFile(ctx, signedURL, pf.f, size, chunkWorkers)
	if err != nil {
		return 0, err
	}
	if err := verifyTransferred(n, size); err != nil {
		return 0, err
	}
	if err := pf.commit(n); err != nil {
		return 0, err
	}
	return n, nil
}

// fetchToFile writes the body of signedURL into f, in parallel when the file is
// big enough for that to pay off.
//
// A single stream is bounded by one connection's round-trip behaviour, which on
// a long fat pipe leaves most of the link idle no matter how large the buffers
// are; several ranged GETs at once fill it. Splitting is only possible because
// the file listing already reported the size, and only worth it above
// downloadRangeMinSize, since every range costs a request of its own.
func (c *ngcClient) fetchToFile(ctx context.Context, signedURL string, f *os.File, size int64, chunkWorkers int) (int64, error) {
	if size < downloadRangeMinSize {
		return c.streamWhole(ctx, signedURL, f)
	}
	chunks := chunkRanges(size, downloadChunkSize)
	workers := workerCount(chunkWorkers, len(chunks))
	if workers <= 1 {
		return c.streamWhole(ctx, signedURL, f)
	}

	// Preallocate so the parallel writers only ever fill holes in a file that
	// is already its final length, rather than racing to extend it.
	if err := f.Truncate(size); err != nil {
		return 0, fmt.Errorf("preallocate %s: %w", humanBytes(size), err)
	}

	// The first chunk doubles as the probe. A 206 proves the presigned host
	// honours Range and the rest can go out at once; a 200 means it ignored the
	// header and answered with the whole file, which is already written, so
	// nothing was wasted finding out.
	n, ranged, err := c.fetchRange(ctx, signedURL, f, chunks[0], true)
	if err != nil {
		return 0, err
	}
	if !ranged {
		return n, nil
	}

	rest, err := c.fetchRangesParallel(ctx, signedURL, f, chunks[1:], workers)
	if err != nil {
		return 0, err
	}
	return n + rest, nil
}

// fetchRangesParallel fetches every range at once with a bounded pool, each
// worker writing straight into its own window of f.
func (c *ngcClient) fetchRangesParallel(ctx context.Context, signedURL string, f *os.File, ranges []byteRange, workers int) (int64, error) {
	if len(ranges) == 0 {
		return 0, nil
	}
	workers = workerCount(workers, len(ranges))

	var (
		next     atomic.Int64
		received atomic.Int64
		aborted  atomic.Int32
		firstErr error
		errOnce  sync.Once
		wg       sync.WaitGroup
	)
	next.Store(-1)
	// Cancel sibling range fetches on the first failure so wg.Wait returns
	// promptly rather than after every in-flight range finishes.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	fail := func(err error) {
		errOnce.Do(func() {
			firstErr = err
			aborted.Store(1)
			cancel()
		})
	}

	wg.Add(workers)
	for range workers {
		go func() {
			defer wg.Done()
			for aborted.Load() == 0 {
				i := int(next.Add(1))
				if i >= len(ranges) {
					return
				}
				n, _, err := c.fetchRange(ctx, signedURL, f, ranges[i], false)
				if err != nil {
					fail(fmt.Errorf("fetch bytes %d-%d: %w", ranges[i].start, ranges[i].end(), err))
					return
				}
				received.Add(n)
			}
		}()
	}
	wg.Wait()
	if firstErr != nil {
		return 0, firstErr
	}
	return received.Load(), nil
}

// streamWhole downloads the entire body into f as one stream, resuming rather
// than restarting when a transfer breaks part way through.
//
// Bytes that already arrived are at their final offsets, so a retry only has to
// ask for the remainder. A host that ignores that resume header answers with
// the whole body instead, which is caught before anything is written and starts
// the stream over from zero.
func (c *ngcClient) streamWhole(ctx context.Context, signedURL string, f *os.File) (int64, error) {
	var (
		done    int64
		lastErr error
	)
	for attempt := 0; attempt < uploadPartMaxAttempts; attempt++ {
		if attempt > 0 {
			if err := transferBackoff(ctx, attempt); err != nil {
				return 0, err
			}
		}
		n, _, err := c.fetchRangeOnce(ctx, signedURL, f, byteRange{start: done}, done == 0)
		if err == nil {
			return done + n, nil
		}
		lastErr = err
		switch {
		case errors.Is(err, errRangeUnsupported):
			// Nothing was written: the status was rejected before the body was
			// read. Start over from the top, with no resume header.
			done = 0
		case isRetryableTransferErr(err):
			// Whatever did arrive went to its final offset, so it counts as
			// progress and the next attempt asks only for what is left.
			// fetchRangeOnce unwound its own progress accounting on the way
			// out, so put back the part that is being kept.
			done += n
			c.reportProgress(n)
		default:
			return 0, err
		}
	}
	return 0, lastErr
}

// fetchRange GETs one window of signedURL into f at that window's offset,
// retrying transient failures. A range is re-requested from the start on retry,
// which is safe because the write goes to a fixed offset: the second attempt
// overwrites exactly what the first one left behind.
func (c *ngcClient) fetchRange(ctx context.Context, signedURL string, f *os.File, r byteRange, allowFullBody bool) (int64, bool, error) {
	var lastErr error
	for attempt := 0; attempt < uploadPartMaxAttempts; attempt++ {
		if attempt > 0 {
			if err := transferBackoff(ctx, attempt); err != nil {
				return 0, false, err
			}
		}
		n, ranged, err := c.fetchRangeOnce(ctx, signedURL, f, r, allowFullBody)
		if err == nil {
			return n, ranged, nil
		}
		if !isRetryableTransferErr(err) {
			return 0, false, err
		}
		lastErr = err
	}
	return 0, false, lastErr
}

// fetchRangeOnce makes one ranged GET and writes the body at the window's
// offset. It returns the bytes written even on failure, so a caller that keeps
// them can account for them, and whether the response really was a range.
//
// allowFullBody accepts a 200 (the whole body) in place of a 206, which only
// the probe request may do. Anywhere else that would mean writing the entire
// file into one window's offset, so it is refused before a byte is read.
func (c *ngcClient) fetchRangeOnce(ctx context.Context, signedURL string, f *os.File, r byteRange, allowFullBody bool) (written int64, ranged bool, err error) {
	reqCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, signedURL, nil)
	if err != nil {
		return 0, false, err
	}
	if h := r.header(); h != "" {
		req.Header.Set("Range", h)
	}

	resp, err := c.partClient().Do(req)
	if err != nil {
		return 0, false, err
	}
	defer resp.Body.Close()

	ranged, err = checkRangeResponse(resp, r, allowFullBody)
	if err != nil {
		return 0, false, err
	}

	body := io.Reader(resp.Body)

	// Watch for a wedged connection: an absence of reads with forward progress
	// means the socket has stopped delivering. On timeout we cancel the request
	// and report a retryable sentinel rather than letting the worker block
	// indefinitely.
	var stalled atomic.Bool
	if c.stallTimeout > 0 {
		sr := &stallReader{r: body, timeout: c.stallTimeout}
		sr.timer = time.AfterFunc(c.stallTimeout, func() {
			stalled.Store(true)
			cancel()
		})
		defer sr.timer.Stop()
		body = sr
	}

	// Report bytes as they arrive so progress tracks transfers still in flight
	// instead of jumping when whole files land. A failed attempt unwinds its own
	// contribution, since the window it was filling gets requested again from
	// the start; the one caller that keeps partial bytes puts them back itself.
	if c.progress != nil {
		pr := &progressReader{r: body, report: c.progress}
		body = pr
		defer func() {
			if err != nil {
				c.reportProgress(-pr.count())
			}
		}()
	}

	n, err := io.CopyBuffer(io.NewOffsetWriter(f, r.start), body, make([]byte, downloadCopyBufferSize))
	if err != nil {
		if stalled.Load() {
			return n, ranged, errTransferStalled
		}
		return n, ranged, err
	}
	return n, ranged, nil
}

// checkRangeResponse decides whether resp can be written at r's offset and
// reports whether it really was a range. Anything it rejects is refused before
// a byte of the body is read.
func checkRangeResponse(resp *http.Response, r byteRange, allowFullBody bool) (bool, error) {
	switch {
	case resp.StatusCode == http.StatusPartialContent:
		if err := checkContentRange(resp.Header.Get("Content-Range"), r); err != nil {
			return false, err
		}
		return true, nil
	case resp.StatusCode == http.StatusOK:
		if !allowFullBody {
			return false, errRangeUnsupported
		}
		return false, nil
	default:
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return false, &transferStatusError{
			Method: http.MethodGet,
			Code:   resp.StatusCode,
			Status: resp.Status,
			Body:   strings.TrimSpace(string(snippet)),
		}
	}
}

// checkContentRange confirms a 206 starts at the offset the window will be
// written to, and does not run past it. A host may clamp the end to the file
// size, so a shorter end is fine; a different start or a longer end would land
// bytes at the wrong offsets.
func checkContentRange(header string, r byteRange) error {
	var start, end int64
	var rest string
	if n, _ := fmt.Sscanf(header, "bytes %d-%d/%s", &start, &end, &rest); n < 3 {
		return fmt.Errorf("unusable Content-Range %q for %s", header, r.header())
	}
	if start != r.start || end < start || (r.length > 0 && end > r.end()) {
		return fmt.Errorf("Content-Range %q does not match requested %s", header, r.header())
	}
	return nil
}

// errRangeUnsupported marks a host that answered a ranged request with the
// whole body. The transfer can still succeed, just not in windows, so the
// caller starts over from offset zero without the header.
var errRangeUnsupported = errors.New("presigned host ignored the Range header")

// byteRange is a window of a file: length bytes starting at start. The zero
// value means the whole thing, which goes out as a plain GET carrying no Range
// header at all — the single-stream path's first attempt, and the only shape a
// host that has never heard of ranges has to cope with.
type byteRange struct {
	start  int64
	length int64
}

// header renders the window as an HTTP Range value, or "" for the zero value. A
// zero length with a non-zero start is the open-ended "rest of the file" form,
// which is how a resumed stream asks for what it has not received yet.
func (r byteRange) header() string {
	switch {
	case r.start == 0 && r.length <= 0:
		return ""
	case r.length <= 0:
		return fmt.Sprintf("bytes=%d-", r.start)
	default:
		return fmt.Sprintf("bytes=%d-%d", r.start, r.start+r.length-1)
	}
}

// end is the last byte offset the window covers, for error messages.
func (r byteRange) end() int64 { return r.start + r.length - 1 }

// chunkRanges splits size into consecutive windows of at most chunkSize.
func chunkRanges(size, chunkSize int64) []byteRange {
	if chunkSize <= 0 {
		chunkSize = downloadChunkSize
	}
	out := make([]byteRange, 0, int((size+chunkSize-1)/chunkSize))
	for start := int64(0); start < size; start += chunkSize {
		out = append(out, byteRange{start: start, length: min(chunkSize, size-start)})
	}
	return out
}

// parseDownloadURL extracts a presigned URL from a JSON body. The NGC download
// route returns a "urls" array (as consumed by the NGC SDK, which uses
// urls[0]); older/alternate shapes carrying a single URL field are also
// tolerated.
func parseDownloadURL(raw []byte) string {
	var body struct {
		URLs        []string `json:"urls"`
		URL         string   `json:"url"`
		DownloadURL string   `json:"downloadUrl"`
		SignedURL   string   `json:"signedUrl"`
		PresignURL  string   `json:"presignedUrl"`
	}
	_ = json.Unmarshal(raw, &body)
	if len(body.URLs) > 0 {
		return body.URLs[0]
	}
	return firstNonEmpty(body.URL, body.DownloadURL, body.SignedURL, body.PresignURL)
}

// partFile is a download in progress. Bytes land in a sibling ".part" file that
// only becomes dest once the whole transfer has succeeded, so an interrupted
// run leaves nothing a later one would mistake for a complete file.
type partFile struct {
	f    *os.File
	tmp  string
	dest string
}

func newPartFile(dest string) (*partFile, error) {
	if dir := filepath.Dir(dest); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create dir %q: %w", dir, err)
		}
	}
	tmp := dest + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return nil, fmt.Errorf("create %q: %w", tmp, err)
	}
	return &partFile{f: f, tmp: tmp, dest: dest}, nil
}

// commit trims the file to the bytes actually transferred — the parallel path
// preallocates the full length, and a host that ignored Range may have sent
// less than that — then moves it into place.
func (p *partFile) commit(size int64) error {
	if err := p.f.Truncate(size); err != nil {
		return fmt.Errorf("truncate %q: %w", p.tmp, err)
	}
	if err := p.f.Close(); err != nil {
		return fmt.Errorf("close %q: %w", p.tmp, err)
	}
	p.f = nil
	if err := os.Rename(p.tmp, p.dest); err != nil {
		return fmt.Errorf("rename %q: %w", p.tmp, err)
	}
	p.tmp = ""
	return nil
}

// discard closes and removes the partial file unless it was committed. Calling
// it after commit does nothing, which is what makes it usable as a deferred
// cleanup on every path out.
func (p *partFile) discard() {
	if p.f != nil {
		p.f.Close()
		p.f = nil
	}
	if p.tmp != "" {
		os.Remove(p.tmp)
		p.tmp = ""
	}
}

// writeToFile streams r into dest through a ".part" file, returning the number
// of bytes written. size is what the listing promised, or 0 when it said
// nothing.
func writeToFile(r io.Reader, dest string, size int64) (int64, error) {
	pf, err := newPartFile(dest)
	if err != nil {
		return 0, err
	}
	defer pf.discard()

	n, err := io.CopyBuffer(pf.f, r, make([]byte, downloadCopyBufferSize))
	if err != nil {
		return n, fmt.Errorf("write %q: %w", dest, err)
	}
	if err := verifyTransferred(n, size); err != nil {
		return 0, err
	}
	if err := pf.commit(n); err != nil {
		return 0, err
	}
	return n, nil
}
