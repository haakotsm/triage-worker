package web

import (
	"strings"
	"testing"
)

func TestRenderIcon(t *testing.T) {
	t.Run("known icon renders an svg with currentColor", func(t *testing.T) {
		got := string(renderIcon("user"))
		for _, want := range []string{"<svg", `stroke="currentColor"`, `aria-hidden="true"`, "</svg>"} {
			if !strings.Contains(got, want) {
				t.Errorf("renderIcon(user) missing %q in %q", want, got)
			}
		}
	})

	t.Run("custom class overrides the default size", func(t *testing.T) {
		got := string(renderIcon("check", "h-8 w-8 text-success"))
		if !strings.Contains(got, `class="h-8 w-8 text-success"`) {
			t.Errorf("renderIcon did not apply custom class: %q", got)
		}
		if strings.Contains(got, "h-4 w-4") {
			t.Errorf("renderIcon should not keep the default size when overridden: %q", got)
		}
	})

	t.Run("unknown icon renders nothing", func(t *testing.T) {
		if got := renderIcon("definitely-not-an-icon"); got != "" {
			t.Errorf("renderIcon(unknown) = %q, want empty", got)
		}
	})

	// Names produced at runtime by stateIcon and buildTimeline (not visible to a
	// static grep of templates) must all resolve, or the control renders
	// invisibly. This guards against a future typo in those switch statements.
	t.Run("runtime icon names all resolve", func(t *testing.T) {
		for _, name := range []string{
			"hourglass", "bell", "user", "check", "help-circle", // stateIcon
			"settings", "arrow-up", "pencil", "check-circle", // buildTimeline
		} {
			if renderIcon(name) == "" {
				t.Errorf("icon %q is referenced by stateIcon/buildTimeline but missing from the registry", name)
			}
		}
	})
}
