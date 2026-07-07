package web

import "html/template"

// iconSVG returns a small inline SVG marked up for the templates' "icon"
// helper. We deliberately keep these few and consistent — Heroicons/Lucide-
// style strokes, 16×16 viewport, currentColor so they tint with the
// surrounding text. No external dependencies, no font loads.
//
// Adding a new icon: pick a name, supply a viewBox + path. The CSS class
// .icon centers them on the text baseline; .icon-lg bumps the size.
func iconSVG(name string) template.HTML {
	switch name {
	case "arrow-right", "chevron-right":
		return svg(`<polyline points="6 4 12 10 6 16" />`)
	case "arrow-left", "chevron-left":
		return svg(`<polyline points="14 4 8 10 14 16" />`)
	case "chevron-double-right":
		return svg(`<polyline points="3 4 9 10 3 16" /><polyline points="9 4 15 10 9 16" />`)
	case "chevron-double-left":
		return svg(`<polyline points="17 4 11 10 17 16" /><polyline points="11 4 5 10 11 16" />`)
	case "arrow-up", "caret-up":
		return svg(`<polyline points="4 13 10 7 16 13" />`)
	case "arrow-down", "caret-down":
		return svg(`<polyline points="4 7 10 13 16 7" />`)
	case "sort-asc":
		return svg(`<polyline points="5 12 10 7 15 12" />`)
	case "sort-desc":
		return svg(`<polyline points="5 8 10 13 15 8" />`)
	case "sort":
		return svg(`<polyline points="6 8 10 4 14 8" /><polyline points="6 12 10 16 14 12" />`)
	case "download":
		return svg(`<polyline points="6 9 10 13 14 9" /><line x1="10" y1="3" x2="10" y2="13" /><polyline points="4 15 4 17 16 17 16 15" />`)
	case "external", "open":
		return svg(`<polyline points="9 4 16 4 16 11" /><line x1="16" y1="4" x2="9" y2="11" /><polyline points="13 11 13 16 4 16 4 7 9 7" />`)
	case "edit", "pencil":
		return svg(`<polygon points="3 13 3 17 7 17 16 8 12 4 3 13" />`)
	case "trash":
		return svg(`<polyline points="4 5 16 5" /><polyline points="6 5 6 16 14 16 14 5" /><line x1="8" y1="8" x2="8" y2="14" /><line x1="12" y1="8" x2="12" y2="14" /><polyline points="8 3 12 3" />`)
	case "x", "close":
		return svg(`<line x1="5" y1="5" x2="15" y2="15" /><line x1="15" y1="5" x2="5" y2="15" />`)
	case "plus":
		return svg(`<line x1="10" y1="4" x2="10" y2="16" /><line x1="4" y1="10" x2="16" y2="10" />`)
	case "search":
		return svg(`<circle cx="9" cy="9" r="5" /><line x1="13" y1="13" x2="16" y2="16" />`)
	case "calendar":
		return svg(`<rect x="3" y="4" width="14" height="13" rx="1" /><line x1="3" y1="8" x2="17" y2="8" /><line x1="7" y1="2" x2="7" y2="6" /><line x1="13" y1="2" x2="13" y2="6" />`)
	case "phone":
		return svg(`<path d="M5 4 H8 L10 9 L7.5 11 A8 8 0 0 0 14 17 L16 14 L20 16 V19 A2 2 0 0 1 18 21 H17 A14 14 0 0 1 3 7 V6 A2 2 0 0 1 5 4 Z" transform="scale(0.78) translate(0 -1)" />`)
	case "block", "shield-x":
		return svg(`<path d="M10 2 L17 5 V10 C17 14 13 17 10 18 C7 17 3 14 3 10 V5 Z" /><line x1="7" y1="7" x2="13" y2="13" /><line x1="13" y1="7" x2="7" y2="13" />`)
	case "check":
		return svg(`<polyline points="4 10 8 14 16 6" />`)
	case "menu":
		return svg(`<line x1="3" y1="6" x2="17" y2="6" /><line x1="3" y1="10" x2="17" y2="10" /><line x1="3" y1="14" x2="17" y2="14" />`)
	case "info":
		return svg(`<circle cx="10" cy="10" r="7" /><line x1="10" y1="9" x2="10" y2="14" /><circle cx="10" cy="6.5" r=".4" fill="currentColor" stroke="none" />`)
	case "warning":
		return svg(`<polygon points="10 3 17 16 3 16" /><line x1="10" y1="8" x2="10" y2="12" /><circle cx="10" cy="14.2" r=".4" fill="currentColor" stroke="none" />`)
	case "globe":
		return svg(`<circle cx="10" cy="10" r="7" /><ellipse cx="10" cy="10" rx="3" ry="7" /><line x1="3" y1="10" x2="17" y2="10" />`)
	case "cancel", "ban":
		return svg(`<circle cx="10" cy="10" r="7" /><line x1="5" y1="5" x2="15" y2="15" />`)
	case "play":
		return svg(`<polygon points="6 4 16 10 6 16" />`)
	}
	return template.HTML(``)
}

func svg(body string) template.HTML {
	const head = `<svg class="icon" width="14" height="14" viewBox="0 0 20 20" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">`
	return template.HTML(head + body + `</svg>`)
}
