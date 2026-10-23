// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// updateOptions is the input for resource update (base-version inherit + new files).
type updateOptions struct {
	BaseVersion   string
	Dir           string
	Files         []string // --file values: PATH or LOCAL=REMOTE
	DisplayName   string
	Description   string
	SetLatest     bool
	DryRun        bool
	HashWorkers   int
	UploadWorkers int
	FileWorkers   int
}

// validateUpdateTarget checks the artifact name, org and both versions an
// update names, before any request is made.
func validateUpdateTarget(coords coordinates, baseVersion string) error {
	if err := validateNGCArtifactName(coords.Name); err != nil {
		return err
	}
	if err := validateCoordinates(coords); err != nil {
		return err
	}
	if baseVersion == "" {
		return fmt.Errorf("--base-version is required")
	}
	if err := validateNGCVersion(baseVersion); err != nil {
		return fmt.Errorf("--base-version: %w", err)
	}
	if baseVersion == coords.Version {
		return fmt.Errorf("--version %q must differ from --base-version", coords.Version)
	}
	return nil
}

// updateToNGC creates a new resource version that inherits baseVersion's files
// server-side, then uploads and commits only the local additions/overwrites.
func updateToNGC(ctx context.Context, ngc *ngcClient, opts updateOptions, log logger) (*result, error) {
	log = ensureLog(log)

	coords := ngc.coords
	if err := validateUpdateTarget(coords, opts.BaseVersion); err != nil {
		return nil, err
	}
	log("NGC target: %s (base %s)", coords, opts.BaseVersion)

	files, total, err := collectUpdateFiles(ctx, opts, log)
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no files to add: pass --file and/or --dir")
	}

	res := &result{Files: files, TotalBytes: total, ArtifactURI: coords.uri()}
	log("Found %d file(s) to add/overwrite, %s total", len(files), humanBytes(total))

	if opts.DryRun {
		log("[dry-run] skipping validate, upload, and commit")
		return res, nil
	}

	ngc.retryLog = func(format string, args ...any) { log("        "+format, args...) }
	defer func() { ngc.retryLog = nil }()

	log("validating base version %s", opts.BaseVersion)
	if err := ngc.validateBaseVersion(ctx, opts.BaseVersion); err != nil {
		return res, err
	}

	displayName := firstNonEmpty(opts.DisplayName, coords.Name)
	log("ensuring NGC resource %s and version %s exist", coords.Name, coords.Version)
	if err := ngc.ensureArtifact(ctx, displayName, opts.Description); err != nil {
		return res, err
	}

	uploadOpts := options{
		HashWorkers:   opts.HashWorkers,
		UploadWorkers: opts.UploadWorkers,
		FileWorkers:   opts.FileWorkers,
		SetLatest:     opts.SetLatest,
	}
	files, total, summary, err := uploadFilesParallel(ctx, ngc, files, total, uploadOpts, log)
	if err != nil {
		return res, err
	}
	res.Files, res.TotalBytes, res.dedupSummary = files, total, summary

	// Match ngcsdk: inherit base with an empty upload-pending commit, then
	// commit the newly uploaded files, then PATCH UPLOAD_COMPLETE.
	log("inheriting files from base version %s", opts.BaseVersion)
	if err := ngc.commitFiles(ctx, commitOpts{
		BaseVersion:   opts.BaseVersion,
		UploadPending: true,
	}); err != nil {
		return res, fmt.Errorf("inherit base version: %w", err)
	}
	log("committing %d new file(s) to %s", len(files), coords.uri())
	if err := ngc.commitFiles(ctx, commitOpts{
		Files:         files,
		UploadPending: true,
	}); err != nil {
		return res, err
	}
	log("marking version %s complete", coords.Version)
	if err := ngc.markUploadComplete(ctx, opts.SetLatest); err != nil {
		return res, err
	}
	return res, nil
}

// collectUpdateFiles builds the hashed file list from --dir and/or --file.
func collectUpdateFiles(ctx context.Context, opts updateOptions, log logger) ([]appFile, int64, error) {
	log = ensureLog(log)
	byPath := map[string]appFile{}

	if opts.Dir != "" {
		log("scanning and hashing %s ...", opts.Dir)
		dirFiles, _, err := scanFiles(ctx, opts.Dir, opts.HashWorkers, log)
		if err != nil {
			return nil, 0, err
		}
		if err := indexUpdateFiles(byPath, dirFiles); err != nil {
			return nil, 0, err
		}
	}

	for _, spec := range opts.Files {
		if err := indexUpdateFileSpec(ctx, byPath, spec); err != nil {
			return nil, 0, err
		}
	}

	files := make([]appFile, 0, len(byPath))
	var total int64
	for _, f := range byPath {
		files = append(files, f)
		total += f.Size
	}
	return files, total, nil
}

// indexUpdateFiles keys scanned files by their remote path, rejecting two
// local files that would land on the same one.
func indexUpdateFiles(byPath map[string]appFile, files []appFile) error {
	for _, f := range files {
		if prev, ok := byPath[f.Path]; ok {
			return fmt.Errorf("duplicate remote path %q from %q and %q", f.Path, prev.LocalPath, f.LocalPath)
		}
		byPath[f.Path] = f
	}
	return nil
}

// indexUpdateFileSpec resolves and hashes one --file spec into byPath.
func indexUpdateFileSpec(ctx context.Context, byPath map[string]appFile, spec string) error {
	local, remote, err := parseFileSpec(spec)
	if err != nil {
		return err
	}
	info, err := os.Stat(local)
	if err != nil {
		return fmt.Errorf("--file %q: %w", spec, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("--file %q is not a regular file", local)
	}
	if prev, ok := byPath[remote]; ok {
		return fmt.Errorf("duplicate remote path %q from %q and %q", remote, prev.LocalPath, local)
	}
	f := appFile{LocalPath: local, Path: remote}
	if err := f.hash(ctx); err != nil {
		return fmt.Errorf("hash %q: %w", local, err)
	}
	byPath[remote] = f
	return nil
}

// parseFileSpec accepts PATH (remote = basename) or LOCAL=REMOTE.
func parseFileSpec(spec string) (local, remote string, err error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return "", "", fmt.Errorf("empty --file value")
	}
	if local, remote, ok := strings.Cut(spec, "="); ok {
		local, remote = strings.TrimSpace(local), filepath.ToSlash(strings.TrimSpace(remote))
		if local == "" || remote == "" {
			return "", "", fmt.Errorf("invalid --file %q: use PATH or LOCAL=REMOTE", spec)
		}
		remote = strings.TrimPrefix(remote, "/")
		if err := validateRemotePath(remote); err != nil {
			return "", "", fmt.Errorf("invalid remote path in --file %q: %w", spec, err)
		}
		return local, remote, nil
	}
	base := filepath.Base(spec)
	if err := validateRemotePath(base); err != nil {
		return "", "", fmt.Errorf("invalid --file %q: %w", spec, err)
	}
	return spec, base, nil
}
