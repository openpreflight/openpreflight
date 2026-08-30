package pages

import (
	"context"
	"fmt"
	"io"

	"github.com/openpreflight/openpreflight/internal/web"
)

// Render writes one named page.
func Render(w io.Writer, page string, p web.Page) error {
	ctx := context.Background()
	var err error
	switch page {
	case "login":
		err = Login(p).Render(ctx, w)
	case "setup":
		err = Setup(p).Render(ctx, w)
	case "error":
		err = Error(p).Render(ctx, w)
	case "dashboard":
		err = Dashboard(p).Render(ctx, w)
	case "settings":
		err = Settings(p).Render(ctx, w)
	case "coolify":
		err = Coolify(p).Render(ctx, w)
	case "githubapps":
		err = GitHubApps(p).Render(ctx, w)
	case "repos":
		err = Repos(p).Render(ctx, w)
	case "jobs":
		err = Jobs(p).Render(ctx, w)
	case "run":
		err = Run(p).Render(ctx, w)
	default:
		return fmt.Errorf("web: no template %q", page)
	}
	if err != nil {
		return fmt.Errorf("web: render %s: %w", page, err)
	}
	return nil
}
