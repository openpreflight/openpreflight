// SPDX-License-Identifier: Apache-2.0

package components

import (
	"strings"
	"testing"
)

// The dialog component is gone but dialog.js is not: sheet/ and alertdialog/
// emit its markup contract, and the mobile sidebar calls window.tui.dialog.
// Deleting the orphan directory would break them with no build error.
func TestBundleStillCarriesDialogScript(t *testing.T) {
	js, _ := buildBundle(TemplFiles)
	for _, want := range []string{"window.tui.dialog", "data-tui-dialog-content"} {
		if !strings.Contains(string(js), want) {
			t.Fatalf("component bundle lost %q; sheet and the mobile sidebar need it", want)
		}
	}
}

func TestBundleCarriesToastScript(t *testing.T) {
	js, _ := buildBundle(TemplFiles)
	for _, want := range []string{"window.tui.toast", "data-tui-toaster"} {
		if !strings.Contains(string(js), want) {
			t.Fatalf("component bundle lost %q; layout toasts need it", want)
		}
	}
}
