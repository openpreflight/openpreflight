// Package queue runs queued jobs: Check Run, clone, pipeline, logs, cleanup.
package queue

import (
	"fmt"
	"sync"

	"github.com/trivedi-vatsal/coolify-github-ci/internal/githubapp"
	"github.com/trivedi-vatsal/coolify-github-ci/internal/store"
)

// clientCache reuses one githubapp.Client per App so installation tokens are
// cached across jobs. Keyed on the App row's updated_at so re-saving a PEM in
// the UI invalidates the entry.
type clientCache struct {
	mu    sync.Mutex
	items map[int64]cacheEntry
}

type cacheEntry struct {
	version string
	client  *githubapp.Client
}

func newClientCache() *clientCache {
	return &clientCache{items: map[int64]cacheEntry{}}
}

// For returns a client for an App row, building one if the cached entry is stale.
func (c *clientCache) For(st *store.Store, app store.GitHubApp) (*githubapp.Client, error) {
	version := fmt.Sprintf("%d|%s|%s", app.AppID, app.UpdatedAt.Format("20060102150405.000"), app.APIURL)
	c.mu.Lock()
	if e, ok := c.items[app.ID]; ok && e.version == version {
		c.mu.Unlock()
		return e.client, nil
	}
	c.mu.Unlock()

	pem, err := st.DecryptPEM(app)
	if err != nil {
		return nil, fmt.Errorf("github app %q: %w", app.Name, err)
	}
	client, err := githubapp.New(app.AppID, pem, app.APIURL)
	if err != nil {
		return nil, fmt.Errorf("github app %q: %w", app.Name, err)
	}
	c.mu.Lock()
	c.items[app.ID] = cacheEntry{version: version, client: client}
	c.mu.Unlock()
	return client, nil
}

// Drop removes a cached client, used when an App is deleted.
func (c *clientCache) Drop(appID int64) {
	c.mu.Lock()
	delete(c.items, appID)
	c.mu.Unlock()
}
