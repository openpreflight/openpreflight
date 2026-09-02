// SPDX-License-Identifier: Apache-2.0

package githubapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// ManifestApp is the credential payload GitHub returns after converting a
// manifest code. Callers must not log PEM or webhook_secret.
type ManifestApp struct {
	ID            int64  `json:"id"`
	Slug          string `json:"slug"`
	Name          string `json:"name"`
	PEM           string `json:"pem"`
	WebhookSecret string `json:"webhook_secret"`
}

// ConvertManifest exchanges a one-time code from GitHub's App manifest flow.
// The conversion POST is unauthenticated.
func ConvertManifest(ctx context.Context, apiURL, code string) (ManifestApp, error) {
	code = strings.TrimSpace(code)
	if code == "" {
		return ManifestApp{}, errors.New("githubapp: conversion code is required")
	}
	if apiURL == "" {
		apiURL = "https://api.github.com"
	}
	apiURL = strings.TrimRight(apiURL, "/")
	target := apiURL + "/app-manifests/" + url.PathEscape(code) + "/conversions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, nil)
	if err != nil {
		return ManifestApp{}, fmt.Errorf("githubapp: conversion request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "openpreflight")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return ManifestApp{}, fmt.Errorf("githubapp: convert manifest: %w", err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return ManifestApp{}, fmt.Errorf("githubapp: read conversion: %w", err)
	}
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return ManifestApp{}, &APIError{Status: res.StatusCode, Path: target, Body: string(raw)}
	}
	var out ManifestApp
	if err := json.Unmarshal(raw, &out); err != nil {
		return ManifestApp{}, fmt.Errorf("githubapp: decode conversion: %w", err)
	}
	if out.ID <= 0 || out.Slug == "" || out.PEM == "" || out.WebhookSecret == "" {
		return ManifestApp{}, errors.New("githubapp: conversion response is missing id, slug, pem, or webhook_secret")
	}
	if out.Name == "" {
		out.Name = out.Slug
	}
	return out, nil
}

// SetHookConfig points the App webhook at hookURL (App JWT).
func (c *Client) SetHookConfig(ctx context.Context, hookURL string) error {
	auth, err := c.appAuth()
	if err != nil {
		return err
	}
	return c.do(ctx, http.MethodPatch, "/app/hook/config", auth, map[string]any{
		"url":          hookURL,
		"content_type": "json",
	}, nil)
}
