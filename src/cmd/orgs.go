// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"context"
	"fmt"
	"net/http"
	"strings"
)

// orgClient reads NGC organization metadata (GET /v2/orgs/{org}). Like the
// portal client it is served by the NGC gateway, so it shares the NGC base URL.
type orgClient struct {
	baseURL string
	apiKey  string
	org     string
	http    *http.Client
}

func newOrgClient(baseURL, apiKey, org string, hc *http.Client) *orgClient {
	return &orgClient{strings.TrimRight(firstNonEmpty(baseURL, ngcDefaultBaseURL), "/"), apiKey, org, hc}
}

// ngcOrg is the subset of the NGC organization payload the CLI uses.
type ngcOrg struct {
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
}

// displayName returns the org's human-readable name, or "" when NGC has none.
// "organizations" is a single org despite the plural key.
func (c *orgClient) displayName(ctx context.Context) (string, error) {
	var out struct {
		Organizations ngcOrg `json:"organizations"`
	}
	url := fmt.Sprintf("%s/v2/orgs/%s", c.baseURL, c.org)
	if err := httpJSON(ctx, c.http, c.apiKey, http.MethodGet, url, nil, &out); err != nil {
		return "", fmt.Errorf("get organization: %w", err)
	}
	return out.Organizations.DisplayName, nil
}

// lookupOrgDisplayName fetches an org's display name, owning the client and
// timeout so callers pass plain settings. Best effort: a failed lookup warns
// and yields "" rather than failing the command that triggered it.
func lookupOrgDisplayName(ctx context.Context, ngcBaseURL, apiKey, org string) string {
	if apiKey == "" || org == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, configCheckTimeout)
	defer cancel()

	c := newOrgClient(ngcBaseURL, apiKey, org, newSecureClient(configCheckTimeout))
	name, err := c.displayName(ctx)
	if err != nil {
		warnf("could not look up the display name for org %s: %v", org, err)
		return ""
	}
	return name
}

// orgFullName renders an org for display. Org names are numeric ids that are
// hard to tell apart, so append the display name when one is known.
func orgFullName(org, displayName string) string {
	if displayName == "" || displayName == org {
		return org
	}
	return fmt.Sprintf("%s (%s)", org, displayName)
}
