package icon

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/a-h/templ"
)

// Props defines the properties that can be set for an icon.
type Props struct {
	Class string
	// Attributes renders extra HTML attributes on the svg element,
	// e.g. data-icon="inline-start" for the icon spacing inside buttons
	// and badges.
	Attributes templ.Attributes
}

// Icon returns a function that generates a templ.Component for the specified icon name.
func Icon(name string) func(...Props) templ.Component {
	return func(props ...Props) templ.Component {
		var p Props
		if len(props) > 0 {
			p = props[0]
		}
		return templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
			content, ok := internalSvgData[name]
			if !ok {
				return fmt.Errorf("icon %q not found", name)
			}
			// The data-lucide attribute helps identify these as Lucide icons if needed.
			_, err := fmt.Fprintf(w, `<svg xmlns="http://www.w3.org/2000/svg" width="24" height="24" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" class="%s" data-lucide="icon"%s>%s</svg>`,
				p.Class, attrString(p.Attributes), content)
			return err
		})
	}
}

// attrString renders extra attributes sorted by key so the markup is stable.
func attrString(attrs templ.Attributes) string {
	if len(attrs) == 0 {
		return ""
	}
	keys := make([]string, 0, len(attrs))
	for k := range attrs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		switch v := attrs[k].(type) {
		case bool:
			if v {
				b.WriteString(" " + templ.EscapeString(k))
			}
		default:
			fmt.Fprintf(&b, " %s=\"%s\"", templ.EscapeString(k), templ.EscapeString(fmt.Sprint(v)))
		}
	}
	return b.String()
}
