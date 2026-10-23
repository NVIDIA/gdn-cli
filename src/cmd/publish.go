// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
)

type options struct {
	AppDir         string
	ExecutablePath string // relative to AppDir (upload) or StartupResource (reference); guessed if empty in upload mode
	ExecutableArgs []string
	AppName        string
	DisplayName    string // NGC display name; defaults to AppName or artifact name
	Description    string
	SeatType       string // WINDOWS | LINUX | LINUX_PROTON
	InstanceNames  []string
	VersionName    string
	ExistingAppID  string // non-empty = add version to existing app
	AllowDuplicate bool
	SetLatest      bool
	DryRun         bool
	HashWorkers    int // <=0 = one worker per CPU
	UploadWorkers  int // <=0 = one worker per CPU; parallel S3 part uploads per file
	FileWorkers    int // <=0 = defaultFileWorkers; concurrent file upload sessions

	// Resources references already-uploaded NGC resource versions to attach to
	// the app version in addition to (or instead of) an uploaded --dir.
	Resources []resourceRef
	// StartupResource names which resource (uploaded or referenced) holds the
	// executable; defaults to the uploaded resource, else the first reference.
	StartupResource string
	// LaunchConfig, when non-nil, is used as the base per-version launch config
	// (executablePath/executableArgs), overridden by ExecutablePath/ExecutableArgs.
	LaunchConfig *launchConfig
	// AppFeatures holds the applicationFeatures fields to send when creating a
	// new app. GDN applies its own default to every field left out.
	AppFeatures *appFeature
}

type result struct {
	Files       []appFile
	TotalBytes  int64
	ArtifactURI string
	AppID       string
	VersionID   string
	dedupSummary
}

// dedupSummary accounts for the files NGC declined to accept because it already
// held their checksum. They are still part of the version, so they stay counted
// in result.Files and result.TotalBytes; none of their bytes crossed the wire.
//
// The two causes look identical in the response and mean opposite things:
//
//   - A tree holding the same bytes at several paths always sees a conflict for
//     every copy after the first, because NGC keys its store by checksum rather
//     than by path. Ordinary, and unavoidable.
//   - A conflict on every copy of a checksum means NGC held those bytes before
//     this upload began. That store is per (org, resource name, checksum) and
//     survives the deletion of every version referencing it, so it is easy to
//     hit unawares — and it makes the elapsed time meaningless as a measure of
//     transfer speed.
type dedupSummary struct {
	// DedupedFiles and DedupedBytes cover both causes.
	DedupedFiles int
	DedupedBytes int64
	// PreexistingFiles and PreexistingBytes are the subset NGC held beforehand.
	PreexistingFiles int
	PreexistingBytes int64
}

// DuplicateFiles reports the copies skipped because an identical file in this
// same upload had already been sent.
func (d dedupSummary) DuplicateFiles() int { return d.DedupedFiles - d.PreexistingFiles }

// DuplicateBytes reports the bytes those duplicate copies would have cost.
func (d dedupSummary) DuplicateBytes() int64 { return d.DedupedBytes - d.PreexistingBytes }

// summarizeDedup splits the skipped files by cause. A checksum whose every copy
// came back as a conflict was in NGC before this run: had it not been, exactly
// one copy would have uploaded and the rest would have collided with it.
func summarizeDedup(files []appFile, abandoned, deduped []bool) dedupSummary {
	copies := make(map[string]int, len(files))
	conflicts := make(map[string]int)
	for i, f := range files {
		if abandoned[i] {
			continue
		}
		copies[f.SHA256]++
		if deduped[i] {
			conflicts[f.SHA256]++
		}
	}

	var s dedupSummary
	for i, f := range files {
		if abandoned[i] || !deduped[i] {
			continue
		}
		s.DedupedFiles++
		s.DedupedBytes += f.Size
		if conflicts[f.SHA256] == copies[f.SHA256] {
			s.PreexistingFiles++
			s.PreexistingBytes += f.Size
		}
	}
	return s
}

type logger func(format string, args ...any)

func ensureLog(log logger) logger {
	if log != nil {
		return log
	}
	return func(string, ...any) {
		// A nil logger means the caller wants no progress output, so discard.
	}
}

// publish scans/uploads a folder and/or references existing resources, then
// creates or extends a GDN app. A version may reference multiple resources.
func publish(ctx context.Context, ngc *ngcClient, portal *portalClient, opts options, log logger) (*result, error) {
	log = ensureLog(log)

	if err := checkDuplicateAppName(ctx, portal, opts); err != nil {
		return nil, err
	}

	uploadMode := opts.AppDir != ""
	if !uploadMode && len(opts.Resources) == 0 {
		return nil, fmt.Errorf("nothing to publish: pass --dir to upload a folder, or --resource <name>:<version> to reference an existing resource")
	}

	artifacts, res, err := publishArtifacts(ctx, ngc, opts, log)
	if err != nil {
		return res, err
	}
	if opts.DryRun {
		return res, nil
	}

	launch, err := resolveLaunchConfig(opts, ngc.coords, res.Files, uploadMode)
	if err != nil {
		return res, err
	}
	log("GDN launch path: %s", launch.ExecutablePath)

	version := createVersionReq{
		VersionName:  opts.VersionName,
		LaunchConfig: launch,
		Artifacts:    artifacts,
	}
	if opts.ExistingAppID != "" {
		return addVersionToApp(ctx, portal, opts.ExistingAppID, version, res, log)
	}
	return createAppWithVersion(ctx, portal, opts, version, res, log)
}

// checkDuplicateAppName blocks a create that would duplicate an existing app
// name, since createApp always mints a new id. Callers run it before the
// "nothing to publish" guard so a duplicate name fails fast with the
// actionable error.
func checkDuplicateAppName(ctx context.Context, portal *portalClient, opts options) error {
	if opts.DryRun || opts.ExistingAppID != "" || opts.AllowDuplicate {
		return nil
	}
	existing, err := portal.findAppByName(ctx, opts.AppName)
	if err != nil {
		return err
	}
	if existing == nil {
		return nil
	}
	return fmt.Errorf("an application named %q already exists (app id %s); "+
		"use \"gdn app version add --app-id %s ...\" to add a version to it, "+
		"or pass --allow-duplicate to create another app with the same name",
		opts.AppName, existing.ID, existing.ID)
}

// publishArtifacts uploads the folder when there is one, then returns the
// version's artifact list: the uploaded resource followed by each referenced
// resource. The result is returned even on failure, since a partial upload is
// still worth reporting.
func publishArtifacts(ctx context.Context, ngc *ngcClient, opts options, log logger) ([]artifactRef, *result, error) {
	coords := ngc.coords
	res := &result{}

	var artifacts []artifactRef
	if opts.AppDir != "" {
		up, err := uploadToNGC(ctx, ngc, opts, log)
		if err != nil {
			return nil, up, err
		}
		res = up
		artifacts = append(artifacts, artifactRef{Name: coords.Name, OrgName: coords.Org, Type: coords.Type, VersionID: coords.Version})
	}
	for _, ref := range opts.Resources {
		a := artifactRef{Name: ref.Name, OrgName: coords.Org, TeamName: ref.Team, Type: coords.Type, VersionID: ref.Version}
		artifacts = append(artifacts, a)
		log("referencing resource %s", a.ngcURI())
	}
	if res.ArtifactURI == "" && len(artifacts) > 0 {
		res.ArtifactURI = artifacts[0].ngcURI()
	}
	return artifacts, res, nil
}

// addVersionToApp adds the version to an existing app and promotes it to
// default.
func addVersionToApp(ctx context.Context, portal *portalClient, appID string, version createVersionReq, res *result, log logger) (*result, error) {
	ver, err := portal.createVersion(ctx, appID, version)
	if err != nil {
		return res, err
	}
	res.AppID, res.VersionID = appID, ver.ID
	log("added version %s (%s) to app %s", ver.ID, ver.Status, appID)
	if err := portal.setDefaultVersion(ctx, appID, ver.ID); err != nil {
		return res, err
	}
	log("set default version to %s", ver.ID)
	return res, nil
}

// createAppWithVersion creates a new app around the version and promotes that
// version to default.
func createAppWithVersion(ctx context.Context, portal *portalClient, opts options, version createVersionReq, res *result, log logger) (*result, error) {
	log("creating application %q", opts.AppName)
	app, err := portal.createApp(ctx, createAppReq{
		Name:                opts.AppName,
		Description:         opts.Description,
		SeatType:            opts.SeatType,
		InstanceNames:       opts.InstanceNames,
		ApplicationFeatures: opts.AppFeatures,
		Version:             version,
	})
	if err != nil {
		return res, err
	}
	res.AppID, res.VersionID = app.ID, app.initialVersionID()
	log("created application %s with version %s", app.ID, res.VersionID)
	if err := portal.setDefaultVersion(ctx, app.ID, res.VersionID); err != nil {
		return res, err
	}
	log("set default version to %s", res.VersionID)
	return res, nil
}

// resolveLaunchConfig determines the per-version launchConfig. Precedence for
// the executable path: an explicit --launch-config JSON blob provides the base,
// then --executable/--exec-path overrides it; in upload mode with no path
// supplied, the executable is guessed from the uploaded files. The executable
// is expressed relative to the startup resource (the uploaded resource by
// default, else the first referenced resource) and expanded to a GDN launch
// path unless it already contains %GDN_ROOT%.
func resolveLaunchConfig(opts options, coords coordinates, files []appFile, uploadMode bool) (launchConfig, error) {
	lc := launchConfig{}
	if opts.LaunchConfig != nil {
		lc = *opts.LaunchConfig
	}

	startupName := opts.StartupResource
	if startupName == "" {
		if uploadMode {
			startupName = coords.Name
		} else if len(opts.Resources) > 0 {
			startupName = opts.Resources[0].Name
		}
	}
	startupCoords := coordinates{Type: coords.Type, Name: startupName}

	rawExec := firstNonEmpty(opts.ExecutablePath, lc.ExecutablePath)
	switch {
	case rawExec != "":
		lc.ExecutablePath = gdnLaunchPath(startupCoords, rawExec)
	case uploadMode:
		exec, err := resolveExecutable(opts, files)
		if err != nil {
			return launchConfig{}, err
		}
		lc.ExecutablePath = gdnLaunchPath(coords, exec)
	default:
		return launchConfig{}, fmt.Errorf("could not determine the executable; pass --exec-path <path> (or --launch-config)")
	}

	if len(opts.ExecutableArgs) > 0 {
		lc.ExecutableArgs = opts.ExecutableArgs
	}
	return lc, nil
}

// uploadToNGC scans, uploads, and commits; no GDN app changes.
func uploadToNGC(ctx context.Context, ngc *ngcClient, opts options, log logger) (*result, error) {
	log = ensureLog(log)

	coords := ngc.coords
	if err := validateCoordinates(coords); err != nil {
		return nil, err
	}
	if strings.EqualFold(coords.Type, "RESOURCE") {
		if err := validateNGCArtifactName(coords.Name); err != nil {
			return nil, err
		}
	}
	log("NGC target: %s", coords)

	log("scanning and hashing %s ...", opts.AppDir)
	files, total, err := scanFiles(ctx, opts.AppDir, opts.HashWorkers, log)
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no files found in %q", opts.AppDir)
	}

	res := &result{Files: files, TotalBytes: total, ArtifactURI: coords.uri()}
	log("Found %d file(s), %s total in %s", len(files), humanBytes(total), opts.AppDir)

	if opts.DryRun {
		log("[dry-run] skipping upload and commit")
		return res, nil
	}

	// Wire the retry logger before the first API call: httpJSONRetry backs off
	// for minutes across its attempts, which looks like a hang without output.
	ngc.retryLog = func(format string, args ...any) { log("        "+format, args...) }
	defer func() { ngc.retryLog = nil }()

	displayName := firstNonEmpty(opts.DisplayName, opts.AppName, coords.Name)
	log("ensuring NGC resource %s and version %s exist", coords.Name, coords.Version)
	if err := ngc.ensureArtifact(ctx, displayName, opts.Description); err != nil {
		return res, err
	}
	files, total, summary, err := uploadFilesParallel(ctx, ngc, files, total, opts, log)
	if err != nil {
		return res, err
	}
	res.Files, res.TotalBytes, res.dedupSummary = files, total, summary

	log("committing %d file(s) to %s", len(files), coords.uri())
	if err := ngc.commit(ctx, files, opts.SetLatest); err != nil {
		return res, err
	}
	return res, nil
}

// uploadFilesParallel uploads every file with the configured worker pool and
// returns the surviving (non-abandoned) files plus a dedup summary. total is
// the byte sum of the input list, used for progress percentages.
func uploadFilesParallel(ctx context.Context, ngc *ngcClient, files []appFile, total int64, opts options, log logger) ([]appFile, int64, dedupSummary, error) {
	fileWorkers := opts.FileWorkers
	if fileWorkers <= 0 {
		fileWorkers = defaultFileWorkers
	}
	workers := workerCount(fileWorkers, len(files))
	log("uploading %d file(s) with %d NGC worker(s), %d S3 part worker(s) per file ...",
		len(files), workers, workerCount(opts.UploadWorkers, 1))

	p := &uploadPool{
		ngc:     ngc,
		files:   files,
		total:   total,
		opts:    opts,
		log:     log,
		skipped: make([]bool, len(files)),
		deduped: make([]bool, len(files)),
	}
	p.next.Store(-1)

	// Count bytes as they reach the wire rather than when a file finishes, so
	// the percentage reflects parts still in flight. Without this a handful of
	// large files hold the total flat and then snap it to 100%.
	ngc.progress = func(n int64) { p.uploaded.Add(n) }
	defer func() { ngc.progress = nil }()

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
		return nil, 0, dedupSummary{}, p.firstErr
	}

	summary := summarizeDedup(files, p.skipped, p.deduped)
	files, total = dropSkipped(files, p.skipped, log)
	if len(files) == 0 {
		return nil, 0, summary, fmt.Errorf("no files could be uploaded: every file changed on disk during the upload")
	}
	log("[%d/%d] 100.0%% (%s/%s) upload complete", len(files), len(files), humanBytes(total), humanBytes(total))
	return files, total, summary, nil
}

// uploadPool is the shared state of one parallel upload run: the cursor into
// files, the byte count as it reaches the wire, the first error (which aborts
// the rest), and the per-file outcomes indexed alongside files.
type uploadPool struct {
	ngc   *ngcClient
	files []appFile
	total int64
	opts  options
	log   logger

	next     atomic.Int64
	uploaded atomic.Int64
	aborted  atomic.Int32
	firstErr error
	errOnce  sync.Once
	logMu    sync.Mutex
	skipped  []bool
	deduped  []bool
}

func (p *uploadPool) fail(err error) {
	p.errOnce.Do(func() {
		p.firstErr = err
		p.aborted.Store(1)
	})
}

// safeLog serializes the progress lines emitted from the parallel workers.
func (p *uploadPool) safeLog(format string, args ...any) {
	p.logMu.Lock()
	defer p.logMu.Unlock()
	p.log(format, args...)
}

// work uploads successive files until the list is exhausted or another worker
// fails.
func (p *uploadPool) work(ctx context.Context) {
	for p.aborted.Load() == 0 {
		i := int(p.next.Add(1))
		if i >= len(p.files) {
			return
		}
		if err := p.uploadOne(ctx, i); err != nil {
			p.fail(err)
			return
		}
	}
}

// uploadOne uploads files[i] and records what became of it.
func (p *uploadPool) uploadOne(ctx context.Context, i int) error {
	f := p.files[i]
	p.safeLog("[%d/%d] uploading %s (%s)", i+1, len(p.files), f.Path, humanBytes(f.Size))
	// Each index is owned by exactly one worker, and the slice is only read
	// again after wg.Wait, so writing back the re-hashed metadata here needs no
	// extra synchronization.
	f, outcome, err := uploadFileRehashing(ctx, p.ngc, f, p.opts.UploadWorkers, p.safeLog)
	if err != nil {
		return err
	}
	p.files[i], p.skipped[i], p.deduped[i] = f, outcome.Abandoned, outcome.Deduped
	if outcome.Abandoned {
		return nil
	}
	// ngc.progress already counted this file's bytes as they went out, so read
	// the running total rather than adding again.
	done := p.uploaded.Load()
	p.safeLog("[%d/%d] %5.1f%% (%s/%s) done %s",
		i+1, len(p.files), percent(done, p.total), humanBytes(done), humanBytes(p.total), f.Path)
	return nil
}

// maxFileChangeAttempts bounds re-hash retries for a file that keeps changing.
// A file actively being appended to (a running app's log) would otherwise loop
// forever, so after this many tries it is skipped rather than failing a run
// that may already have uploaded tens of thousands of files.
const maxFileChangeAttempts = 3

// fileOutcome records what became of one file, beyond the metadata that was
// uploaded for it.
type fileOutcome struct {
	// Abandoned marks a file given up on after it kept changing on disk. It is
	// left out of the commit manifest.
	Abandoned bool
	// Deduped marks a file NGC already held, so none of its bytes crossed the
	// wire. It is still part of the version.
	Deduped bool
}

// uploadFileRehashing uploads f, re-hashing and re-uploading when the file
// changes on disk mid-run. The hash is the blob's address in NGC, so a changed
// file cannot simply be retried; it needs a fresh digest and multipart session.
// It returns the metadata that was actually uploaded, so the commit manifest
// carries the digest NGC received, alongside what became of the file.
func uploadFileRehashing(ctx context.Context, ngc *ngcClient, f appFile, uploadWorkers int, log logger) (appFile, fileOutcome, error) {
	log = ensureLog(log)
	for attempt := 1; ; attempt++ {
		deduped, err := ngc.uploadFile(ctx, f, uploadWorkers, log)
		if err == nil {
			return f, fileOutcome{Deduped: deduped}, nil
		}
		if !errors.Is(err, errFileChanged) {
			return f, fileOutcome{}, err
		}
		if attempt >= maxFileChangeAttempts {
			log("        %s", formatWarning(cliWarning{
				Message:    fmt.Sprintf("skipping %s after %d attempts: %v", f.Path, attempt, err),
				Suggestion: "it will not be part of this version; stop whatever is writing to it, or point --dir at a static copy",
			}))
			return f, fileOutcome{Abandoned: true}, nil
		}
		log("        %s changed on disk; re-hashing and retrying (attempt %d of %d)", f.Path, attempt+1, maxFileChangeAttempts)
		if err := f.hash(ctx); err != nil {
			return f, fileOutcome{}, fmt.Errorf("re-hash %q after it changed during upload: %w", f.Path, err)
		}
	}
}

// dropSkipped removes files that were abandoned mid-upload and recomputes the
// byte total, so the commit manifest never references a blob NGC never got.
func dropSkipped(files []appFile, skipped []bool, log logger) ([]appFile, int64) {
	var names []string
	kept := make([]appFile, 0, len(files))
	var total int64
	for i, f := range files {
		if skipped[i] {
			names = append(names, f.Path)
			continue
		}
		kept = append(kept, f)
		total += f.Size
	}
	if len(names) > 0 {
		log("%s", formatWarning(cliWarning{
			Message: fmt.Sprintf("%d file(s) were skipped because they kept changing during the upload: %s",
				len(names), strings.Join(names, ", ")),
			Suggestion: "they are not part of this version",
		}))
	}
	return kept, total
}

// gdnLaunchPath maps a relative executable to %GDN_ROOT%\<TYPE>\<name>\<rel>.
func gdnLaunchPath(coords coordinates, relExec string) string {
	if strings.Contains(relExec, "%GDN_ROOT%") {
		return relExec
	}
	rel := strings.ReplaceAll(relExec, "/", `\`)
	return fmt.Sprintf(`%%GDN_ROOT%%\%s\%s\%s`, strings.ToUpper(coords.Type), coords.Name, rel)
}

// scanFiles walks dir, hashes files in parallel, returns sorted metadata. Both
// the walk and the hashing honour ctx: on a large tree over a slow mount this
// phase dominates startup, so Ctrl+C has to land here rather than waiting for
// the scan to finish.
func scanFiles(ctx context.Context, dir string, workers int, log logger) ([]appFile, int64, error) {
	log = ensureLog(log)
	files, err := collectFiles(ctx, dir)
	if err != nil {
		return nil, 0, err
	}
	if len(files) == 0 {
		return files, 0, nil
	}

	workers = workerCount(workers, len(files))
	log("  hashing %d files with %d workers ...", len(files), workers)

	total, err := hashFilesParallel(ctx, files, workers, log)
	if err != nil {
		return nil, 0, err
	}

	slices.SortFunc(files, func(a, b appFile) int { return cmp.Compare(a.Path, b.Path) })
	return files, total, nil
}

// collectFiles walks dir and returns metadata for every regular file under it,
// with paths relative to dir.
func collectFiles(ctx context.Context, dir string) ([]appFile, error) {
	info, err := os.Stat(dir)
	if err != nil {
		return nil, fmt.Errorf("app dir: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%q is not a directory", dir)
	}

	files := make([]appFile, 0, 256)
	err = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if err != nil || d.IsDir() || !d.Type().IsRegular() {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		files = append(files, appFile{LocalPath: path, Path: filepath.ToSlash(rel)})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return files, nil
}

// hashPool is the shared state of one parallel hash run: the cursor into
// files, the running totals, and the first error, which aborts the rest.
type hashPool struct {
	files []appFile
	log   logger

	next     atomic.Int64
	hashed   atomic.Int64
	total    atomic.Int64
	aborted  atomic.Int32
	firstErr error
	errOnce  sync.Once
	logMu    sync.Mutex
}

func (p *hashPool) fail(err error) {
	p.errOnce.Do(func() {
		p.firstErr = err
		p.aborted.Store(1)
	})
}

// work hashes successive files in place until the list is exhausted or another
// worker fails.
func (p *hashPool) work(ctx context.Context) {
	for p.aborted.Load() == 0 {
		if ctx.Err() != nil {
			p.fail(ctx.Err())
			return
		}
		i := int(p.next.Add(1))
		if i >= len(p.files) {
			return
		}
		if err := p.files[i].hash(ctx); err != nil {
			p.fail(fmt.Errorf("hash %q: %w", p.files[i].Path, err))
			return
		}
		p.total.Add(p.files[i].Size)
		p.report()
	}
}

// report counts one hashed file and logs progress every 200th.
func (p *hashPool) report() {
	n := p.hashed.Add(1)
	if n%200 != 0 {
		return
	}
	p.logMu.Lock()
	defer p.logMu.Unlock()
	p.log("  ...hashed %d/%d files (%s)", n, len(p.files), humanBytes(p.total.Load()))
}

// hashFilesParallel hashes every file in place and returns the total bytes.
func hashFilesParallel(ctx context.Context, files []appFile, workers int, log logger) (int64, error) {
	p := &hashPool{files: files, log: log}
	p.next.Store(-1)

	var wg sync.WaitGroup
	wg.Add(workers)
	for range workers {
		go func() {
			defer wg.Done()
			p.work(ctx)
		}()
	}
	wg.Wait()

	// Report cancellation plainly; the per-file wrapper ("hash %q: ...") would
	// otherwise blame whichever file happened to be in flight.
	if ctx.Err() != nil {
		return 0, ctx.Err()
	}
	if p.firstErr != nil {
		return 0, p.firstErr
	}
	return p.total.Load(), nil
}

func workerCount(workers, items int) int {
	if items <= 0 {
		return 1
	}
	if workers <= 0 {
		workers = runtime.NumCPU()
	}
	if workers > items {
		workers = items
	}
	return workers
}

func percent(done, total int64) float64 {
	if total <= 0 {
		return 100
	}
	return float64(done) / float64(total) * 100
}

// resolveExecutable picks --executable or guesses largest file (.exe on Windows).
func resolveExecutable(opts options, files []appFile) (string, error) {
	if opts.ExecutablePath != "" {
		want := filepath.ToSlash(opts.ExecutablePath)
		for _, f := range files {
			if f.Path == want {
				return want, nil
			}
		}
		return "", fmt.Errorf("executable %q not found in app dir", want)
	}

	var largest, largestExe appFile
	for _, f := range files {
		if f.Size > largest.Size {
			largest = f
		}
		if strings.HasSuffix(strings.ToLower(f.Path), ".exe") && f.Size > largestExe.Size {
			largestExe = f
		}
	}
	pick := largest
	if strings.EqualFold(opts.SeatType, "WINDOWS") && largestExe.Path != "" {
		pick = largestExe
	}
	if pick.Path == "" {
		return "", fmt.Errorf("could not determine executable; pass --executable")
	}
	return pick.Path, nil
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
