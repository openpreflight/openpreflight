// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// imageRe is a conservative docker image name: registry/name:tag or @sha256.
// It rejects shell metacharacters so a pipeline `runtime` cannot become extra
// docker flags.
var imageRe = regexp.MustCompile(`^[A-Za-z0-9]+([._-][A-Za-z0-9]+)*(/[A-Za-z0-9]+([._-][A-Za-z0-9]+)*)*(:[A-Za-z0-9._-]{1,128})?(@sha256:[a-f0-9]{64})?$`)

// ValidImage reports whether name is safe to pass as a `docker run` image.
func ValidImage(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("image name is empty")
	}
	if len(name) > 255 {
		return errors.New("image name is too long")
	}
	if strings.ContainsAny(name, " \t\n$;&|<>`()\\") || strings.HasPrefix(name, "-") {
		return errors.New("image name contains illegal characters")
	}
	if !imageRe.MatchString(name) {
		return fmt.Errorf("invalid docker image %q", name)
	}
	return nil
}
