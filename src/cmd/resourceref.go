// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"fmt"
	"strings"
)

// resourceRef identifies an already-uploaded NGC resource version, written on
// the command line as "[<team>/]<resource-name>:<version>". Team is empty for
// an org-scoped resource.
type resourceRef struct {
	Team    string
	Name    string
	Version string
}

func (r resourceRef) String() string {
	if r.Team == "" {
		return r.Name + ":" + r.Version
	}
	return r.Team + "/" + r.Name + ":" + r.Version
}

// parseResourceRef parses "[<team>/]<name>:<version>". The version may itself
// contain colons (rare), so only the first colon is used as the separator.
func parseResourceRef(s string) (resourceRef, error) {
	s = strings.TrimSpace(s)
	bad := fmt.Errorf("invalid resource reference %q: expected [<team>/]<resource-name>:<version>", s)
	path, version, ok := strings.Cut(s, ":")
	version = strings.TrimSpace(version)
	if !ok || version == "" {
		return resourceRef{}, bad
	}
	ref := resourceRef{Name: strings.TrimSpace(path), Version: version}
	if team, name, scoped := strings.Cut(path, "/"); scoped {
		ref.Team, ref.Name = strings.TrimSpace(team), strings.TrimSpace(name)
		if ref.Team == "" || strings.Contains(ref.Name, "/") {
			return resourceRef{}, bad
		}
	}
	if ref.Name == "" {
		return resourceRef{}, bad
	}
	return ref, nil
}

// resourceRefList is a repeatable "--resource <name>:<version>" flag.
type resourceRefList []resourceRef

func (l *resourceRefList) String() string {
	parts := make([]string, len(*l))
	for i, r := range *l {
		parts[i] = r.String()
	}
	return strings.Join(parts, ",")
}

func (l *resourceRefList) Set(v string) error {
	// Allow comma-separated refs in one flag value as well as repeats.
	for part := range strings.SplitSeq(v, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		ref, err := parseResourceRef(part)
		if err != nil {
			return err
		}
		*l = append(*l, ref)
	}
	return nil
}

// envVarList is a repeatable "--env-var <key>:<value>" flag that collects launch
// environment variables as an alternative to the --launch-config JSON document.
type envVarList []launchEnvVar

func (l *envVarList) String() string {
	parts := make([]string, len(*l))
	for i, e := range *l {
		parts[i] = e.Key + ":" + e.Value
	}
	return strings.Join(parts, ",")
}

// Set parses "<key>:<value>". Only the first colon separates key from value, so
// values may contain colons (e.g. URLs or Windows paths).
func (l *envVarList) Set(v string) error {
	key, value, ok := strings.Cut(v, ":")
	key = strings.TrimSpace(key)
	if !ok || key == "" {
		return fmt.Errorf("invalid environment variable %q: expected <key>:<value>", v)
	}
	*l = append(*l, launchEnvVar{Key: key, Value: value})
	return nil
}
