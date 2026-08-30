// SPDX-License-Identifier: Apache-2.0

package web

import (
	"strings"
	"testing"
)

func TestCompiledCSSIsEmbedded(t *testing.T) {
	if len(outputCSS) < 500 {
		t.Fatal("embedded CSS is empty; from internal/web run: npm install && npm run css")
	}
	for _, want := range []string{"--primary", "--sidebar", ".dark"} {
		if !strings.Contains(outputCSS, want) {
			t.Errorf("compiled CSS missing %q", want)
		}
	}
}
