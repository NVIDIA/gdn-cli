// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

// readJSONArg resolves a JSON-blob flag value that may be inline JSON, a
// "@path/to/file.json" reference, or "-" for stdin.
func readJSONArg(val string) ([]byte, error) {
	switch {
	case val == "-":
		raw, err := io.ReadAll(os.Stdin)
		if err != nil {
			return nil, fmt.Errorf("read JSON from stdin: %w", err)
		}
		return raw, nil
	case strings.HasPrefix(val, "@"):
		path := val[1:]
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read JSON file %q: %w", path, err)
		}
		return raw, nil
	default:
		return []byte(val), nil
	}
}

// decodeJSONStrict decodes raw into out, rejecting unknown fields so typos in a
// JSON blob surface as a clear error rather than being silently ignored.
func decodeJSONStrict(raw []byte, what string, out any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("parse %s JSON: %w", what, err)
	}
	return nil
}

// loadJSONInto reads a JSON-blob flag value (inline/@file/-) and strictly
// decodes it into out.
func loadJSONInto(val, what string, out any) error {
	raw, err := readJSONArg(val)
	if err != nil {
		return err
	}
	return decodeJSONStrict(raw, what, out)
}

// loadAppFeatures strictly decodes an --app-features document into f, over
// any values f already holds. A DISPLAY_MODE in the document replaces f's
// list: decoding into the existing slice would fill its elements in place, so
// an entry missing a member would keep the old entry's value.
// RTX_ENABLED is dropped with a warning: every GDN instance runs with RTX
// enabled, and documents written for earlier releases still carry the key.
func loadAppFeatures(val string, f *appFeature) error {
	raw, err := readJSONArg(val)
	if err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) == nil {
		if _, ok := fields["DISPLAY_MODE"]; ok {
			f.DisplayMode = nil
		}
		if _, ok := fields["RTX_ENABLED"]; ok {
			delete(fields, "RTX_ENABLED")
			warnRTXIgnored("RTX_ENABLED")
			if raw, err = json.Marshal(fields); err != nil {
				return err
			}
		}
	}
	return decodeJSONStrict(raw, "--"+flagAppFeatures, f)
}

func warnRTXIgnored(what string) {
	warnf("%s is no longer supported and was not sent; every GDN instance runs with RTX enabled", what)
}
