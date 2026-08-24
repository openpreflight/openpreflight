package coolify

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
)

// ComposeApplicationInput is POST /api/v1/applications/dockercompose.
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

// CreateComposeApplication is POST /api/v1/applications/dockercompose.
// InstantDeploy is left false so the operator can set CI_SECRET_KEY first.
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
	payload := map[string]any{
		"project_uuid":     in.ProjectUUID,
		"server_uuid":      in.ServerUUID,
		"environment_name": in.EnvironmentName,
		"name":             in.Name,
		"instant_deploy":   in.InstantDeploy,
		"docker_compose_raw": in.Compose,
	}
	var raw json.RawMessage
	err := c.post(ctx, "/api/v1/applications/dockercompose", payload, &raw)
	if err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.Status == 422 {
			payload["docker_compose_raw"] = base64.StdEncoding.EncodeToString([]byte(in.Compose))
			err = c.post(ctx, "/api/v1/applications/dockercompose", payload, &raw)
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
	return resp.UUID, nil
}
