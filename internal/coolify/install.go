// SPDX-License-Identifier: Apache-2.0

package coolify

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
)

const (
	// pathDockerCompose is the older Coolify create-compose endpoint.
	pathDockerCompose = "/api/v1/applications/dockercompose"
	// pathServices is Coolify 4.3+ (dockercompose is gone; compose is a service).
	pathServices = "/api/v1/services"
)

// ComposeApplicationInput is the payload for creating a compose stack.
type ComposeApplicationInput struct {
	Name            string
	ProjectUUID     string
	ServerUUID      string
	EnvironmentName string
	Compose         string
	InstantDeploy   bool
}

// WorkerCompose is the compose file we ask Coolify to run for this worker.
// Ports are omitted so Coolify's proxy owns the public URL. The docker socket
// is mounted so jobs can `docker run` on that server.
func WorkerCompose() string {
	return `services:
  openpreflight:
    build: .
    environment:
      CI_SECRET_KEY: ${CI_SECRET_KEY}
      CI_PUBLIC_BASE_URL: ${CI_PUBLIC_BASE_URL:-}
      CI_BOOTSTRAP_ADMIN_PASSWORD: ${CI_BOOTSTRAP_ADMIN_PASSWORD:-}
      CI_DOCKER_HOST: unix:///var/run/docker.sock
      LISTEN_ADDR: ":8080"
      DATA_DIR: /data
      WORKSPACE_DIR: /workspace
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
      - ci-data:/data
      - ci-workspace:/workspace
    healthcheck:
      test: ["CMD", "wget", "-qO-", "http://127.0.0.1:8080/health"]
      interval: 30s
      timeout: 5s
      retries: 3
      start_period: 10s
    restart: unless-stopped
volumes:
  ci-data:
  ci-workspace:
`
}

// CreateComposeApplication creates a compose stack with instant_deploy as given
// (install-worker hard-codes false so CI_SECRET_KEY can be set first).
//
// Coolify Cloud / newer instances accept POST /api/v1/applications/dockercompose.
// Self-hosted 4.3.x dropped that path; compose stacks are POST /api/v1/services
// and require base64 docker_compose_raw. We try dockercompose first, then
// services on 404.
func (c *Client) CreateComposeApplication(ctx context.Context, in ComposeApplicationInput) (string, error) {
	if in.ProjectUUID == "" || in.ServerUUID == "" {
		return "", fmt.Errorf("coolify: project_uuid and server_uuid are required")
	}
	if in.EnvironmentName == "" {
		in.EnvironmentName = "production"
	}
	if in.Name == "" {
		in.Name = "openpreflight"
	}
	if in.Compose == "" {
		in.Compose = WorkerCompose()
	}
	encoded := base64.StdEncoding.EncodeToString([]byte(in.Compose))
	payload := map[string]any{
		"project_uuid":       in.ProjectUUID,
		"server_uuid":        in.ServerUUID,
		"environment_name":   in.EnvironmentName,
		"name":               in.Name,
		"instant_deploy":     in.InstantDeploy,
		"docker_compose_raw": in.Compose,
	}
	var raw json.RawMessage
	err := c.post(ctx, pathDockerCompose, payload, &raw)
	if err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.Status == 422 {
			payload["docker_compose_raw"] = encoded
			err = c.post(ctx, pathDockerCompose, payload, &raw)
		}
	}
	if err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.NotFound() {
			payload["docker_compose_raw"] = encoded
			err = c.post(ctx, pathServices, payload, &raw)
		}
	}
	if err != nil {
		return "", err
	}
	var resp struct {
		UUID string `json:"uuid"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return "", fmt.Errorf("coolify: create application: %w", err)
	}
	if resp.UUID == "" {
		return "", fmt.Errorf("coolify: create application: response had no uuid")
	}
	return resp.UUID, nil
}
