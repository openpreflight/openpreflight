package web

import (
	"strings"
	"testing"
)

func TestCompiledCSSIsEmbedded(t *testing.T) {
	if len(appCSS) < 500 {
		t.Fatal("embedded CSS is empty; from internal/web run: npm install && npm run css")
	}
	for _, want := range []string{"--color-accent", ".panel", ".pill", ".nav-link"} {
		if !strings.Contains(appCSS, want) {
			t.Errorf("compiled CSS missing %q", want)
		}
	}
}
