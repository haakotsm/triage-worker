package web

import (
	"html/template"
	"strings"
)

// iconPaths holds the inner SVG markup for each named icon. Paths are from the
// Lucide icon set (ISC licensed) on a 24×24 viewBox, stroked with currentColor
// so they inherit text colour and sizing utilities. Keeping a single curated
// set here replaces the ad-hoc OS emoji that rendered inconsistently per
// platform. Filled glyphs (the state dot) set their own fill/stroke.
var iconPaths = map[string]template.HTML{
	"search":         `<circle cx="11" cy="11" r="8"/><path d="m21 21-4.3-4.3"/>`,
	"moon":           `<path d="M12 3a6.36 6.36 0 0 0 9 9 9 9 0 1 1-9-9Z"/>`,
	"sun":            `<circle cx="12" cy="12" r="4"/><path d="M12 2v2"/><path d="M12 20v2"/><path d="m4.93 4.93 1.41 1.41"/><path d="m17.66 17.66 1.41 1.41"/><path d="M2 12h2"/><path d="M20 12h2"/><path d="m6.34 17.66-1.41 1.41"/><path d="m19.07 4.93-1.41 1.41"/>`,
	"user":           `<path d="M19 21v-2a4 4 0 0 0-4-4H9a4 4 0 0 0-4 4v2"/><circle cx="12" cy="7" r="4"/>`,
	"check":          `<path d="M20 6 9 17l-5-5"/>`,
	"check-circle":   `<circle cx="12" cy="12" r="10"/><path d="m9 12 2 2 4-4"/>`,
	"bell":           `<path d="M10.27 21a2 2 0 0 0 3.46 0"/><path d="M3.26 15.43A2 2 0 0 0 5 18h14a2 2 0 0 0 1.74-2.57L19 12V9a7 7 0 0 0-14 0v3z"/>`,
	"hourglass":      `<path d="M5 22h14"/><path d="M5 2h14"/><path d="M17 22v-4.17a2 2 0 0 0-.59-1.41L12 12l-4.41 4.42A2 2 0 0 0 7 17.83V22"/><path d="M7 2v4.17a2 2 0 0 0 .59 1.41L12 12l4.41-4.42A2 2 0 0 0 17 6.17V2"/>`,
	"help-circle":    `<circle cx="12" cy="12" r="10"/><path d="M9.09 9a3 3 0 0 1 5.83 1c0 2-3 3-3 3"/><path d="M12 17h.01"/>`,
	"clipboard-list": `<rect width="8" height="4" x="8" y="2" rx="1" ry="1"/><path d="M16 4h2a2 2 0 0 1 2 2v14a2 2 0 0 1-2 2H6a2 2 0 0 1-2-2V6a2 2 0 0 1 2-2h2"/><path d="M12 11h4"/><path d="M12 16h4"/><path d="M8 11h.01"/><path d="M8 16h.01"/>`,
	"pencil":         `<path d="M21.17 6.81a1 1 0 0 0-3.98-3.98L3.84 16.17a2 2 0 0 0-.5.83l-1.32 4.35a.5.5 0 0 0 .62.62l4.35-1.32a2 2 0 0 0 .83-.5z"/><path d="m15 5 4 4"/>`,
	"settings":       `<path d="M12.22 2h-.44a2 2 0 0 0-2 2v.18a2 2 0 0 1-1 1.73l-.43.25a2 2 0 0 1-2 0l-.15-.08a2 2 0 0 0-2.73.73l-.22.38a2 2 0 0 0 .73 2.73l.15.1a2 2 0 0 1 1 1.72v.51a2 2 0 0 1-1 1.74l-.15.09a2 2 0 0 0-.73 2.73l.22.38a2 2 0 0 0 2.73.73l.15-.08a2 2 0 0 1 2 0l.43.25a2 2 0 0 1 1 1.73V20a2 2 0 0 0 2 2h.44a2 2 0 0 0 2-2v-.18a2 2 0 0 1 1-1.73l.43-.25a2 2 0 0 1 2 0l.15.08a2 2 0 0 0 2.73-.73l.22-.39a2 2 0 0 0-.73-2.73l-.15-.08a2 2 0 0 1-1-1.74v-.5a2 2 0 0 1 1-1.74l.15-.09a2 2 0 0 0 .73-2.73l-.22-.38a2 2 0 0 0-2.73-.73l-.15.08a2 2 0 0 1-2 0l-.43-.25a2 2 0 0 1-1-1.73V4a2 2 0 0 0-2-2z"/><circle cx="12" cy="12" r="3"/>`,
	"flame":          `<path d="M8.5 14.5A2.5 2.5 0 0 0 11 12c0-1.38-.5-2-1-3-1.07-2.14-.22-4.05 2-6 .5 2.5 2 4.9 4 6.5 2 1.6 3 3.5 3 5.5a7 7 0 1 1-14 0c0-1.15.43-2.29 1-3a2.5 2.5 0 0 0 2.5 2.5z"/>`,
	"alert-triangle": `<path d="m21.73 18-8-14a2 2 0 0 0-3.48 0l-8 14A2 2 0 0 0 4 21h16a2 2 0 0 0 1.73-3z"/><path d="M12 9v4"/><path d="M12 17h.01"/>`,
	"arrow-up":       `<path d="m5 12 7-7 7 7"/><path d="M12 19V5"/>`,
	"refresh":        `<path d="M3 12a9 9 0 0 1 9-9 9.75 9.75 0 0 1 6.74 2.74L21 8"/><path d="M21 3v5h-5"/><path d="M21 12a9 9 0 0 1-9 9 9.75 9.75 0 0 1-6.74-2.74L3 16"/><path d="M8 16H3v5"/>`,
}

// renderIcon returns an inline SVG for the named icon, decorative by default
// (aria-hidden). The optional argument overrides the SIZE classes (default
// "h-4 w-4"); the base classes (inline-block, vertical-centering, shrink-0) are
// always applied so the icon stays aligned with adjacent text regardless of the
// size passed. Unknown names render nothing rather than breaking the page.
//
// Note: alignment uses align-middle (a standard utility) rather than an
// arbitrary value — arbitrary values in this .go file are not reliably picked
// up by Tailwind's content scanner.
func renderIcon(name string, class ...string) template.HTML {
	inner, ok := iconPaths[name]
	if !ok {
		return ""
	}
	size := "h-4 w-4"
	if len(class) > 0 && strings.TrimSpace(class[0]) != "" {
		size = class[0]
	}
	cls := "inline-block shrink-0 align-middle " + size
	return template.HTML(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="none" ` +
		`stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" ` +
		`class="` + template.HTMLEscapeString(cls) + `" aria-hidden="true">` + string(inner) + `</svg>`)
}
