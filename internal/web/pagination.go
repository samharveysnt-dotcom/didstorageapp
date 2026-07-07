package web

import (
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strconv"
)

// Pagination is the data passed to templates so they can render page links and
// preserve filters across page changes. It also produces the SQL LIMIT/OFFSET
// values for the handler's query.
type Pagination struct {
	Page     int
	PerPage  int
	Total    int
	Sort     string // requested sort column key (whitelisted by the handler)
	Dir      string // "asc" or "desc"
	Q        string // free-text search term entered by the user
	BaseURL  string // path for page links, e.g. "/users"
	BaseArgs string // pre-encoded query-string of OTHER filters (no q/page/sort/dir)
	// preserved is a snapshot of every other query param that came in on the
	// request — we rebuild this for every link so filters carry across pages.
	preserved url.Values
}

// Pages returns 1-indexed page count.
func (p Pagination) Pages() int {
	if p.PerPage <= 0 {
		return 1
	}
	n := (p.Total + p.PerPage - 1) / p.PerPage
	if n == 0 {
		return 1
	}
	return n
}

// Offset is the SQL OFFSET to use given the current page.
func (p Pagination) Offset() int { return (p.Page - 1) * p.PerPage }

// Limit is the SQL LIMIT — equals PerPage.
func (p Pagination) Limit() int { return p.PerPage }

// PageURL builds the URL for a specific page, preserving every other query
// param the handler received (filters, free-text q, sort, dir, per_page).
func (p Pagination) PageURL(n int) string {
	v := url.Values{}
	for k, vs := range p.preserved {
		for _, x := range vs {
			v.Add(k, x)
		}
	}
	v.Set("page", strconv.Itoa(n))
	v.Set("per_page", strconv.Itoa(p.PerPage))
	if p.Sort != "" {
		v.Set("sort", p.Sort)
	}
	if p.Dir != "" {
		v.Set("dir", p.Dir)
	}
	if p.Q != "" {
		v.Set("q", p.Q)
	}
	return p.BaseURL + "?" + v.Encode()
}

// PageWindow returns the list of page numbers to render around the current
// page (with -1 entries acting as "…" gap markers). Limited to 7 visible
// page numbers + first / last.
func (p Pagination) PageWindow() []int {
	total := p.Pages()
	cur := p.Page
	if total <= 9 {
		out := make([]int, total)
		for i := range out {
			out[i] = i + 1
		}
		return out
	}
	out := []int{1}
	start := cur - 2
	end := cur + 2
	if start < 2 {
		start = 2
	}
	if end > total-1 {
		end = total - 1
	}
	if start > 2 {
		out = append(out, -1)
	}
	for i := start; i <= end; i++ {
		out = append(out, i)
	}
	if end < total-1 {
		out = append(out, -1)
	}
	out = append(out, total)
	return out
}

// SortLink returns the URL to toggle sort on `col`. If currently sorted by
// the same column ascending, flips to desc; otherwise sets asc. Page is
// reset to 1.
func (p Pagination) SortLink(col string) string {
	dir := "asc"
	if p.Sort == col && p.Dir == "asc" {
		dir = "desc"
	}
	v := url.Values{}
	for k, vs := range p.preserved {
		for _, x := range vs {
			v.Add(k, x)
		}
	}
	v.Set("page", "1")
	v.Set("per_page", strconv.Itoa(p.PerPage))
	v.Set("sort", col)
	v.Set("dir", dir)
	if p.Q != "" {
		v.Set("q", p.Q)
	}
	return p.BaseURL + "?" + v.Encode()
}

// SortArrow returns "", "asc", or "desc" — the templates pass that through
// {{icon "sort-asc"}} / {{icon "sort-desc"}} to render an inline SVG.
// Inactive columns get an empty string and the cell shows nothing.
func (p Pagination) SortArrow(col string) string {
	if p.Sort != col {
		return ""
	}
	return p.Dir
}

// SortIcon renders the sort indicator for column `col` directly as inline
// SVG. Use this in templates instead of the {{icon …}} helper when you want
// to embed an indicator inside a sort header link.
func (p Pagination) SortIcon(col string) template.HTML {
	if p.Sort != col {
		return template.HTML(`<span class="sort-placeholder"></span>`)
	}
	if p.Dir == "asc" {
		return iconSVG("sort-asc")
	}
	return iconSVG("sort-desc")
}

func extra(k, v string) string {
	if v == "" {
		return ""
	}
	return "&" + k + "=" + url.QueryEscape(v)
}

// readPagination reads page / per_page / sort / dir / q from query params,
// applies sane defaults and the requested per-page cap. All OTHER params
// are stashed on the Pagination so PageURL / SortLink can preserve them.
func readPagination(r *http.Request, baseURL string, allowedSorts map[string]string, defaultSort string) Pagination {
	q := r.URL.Query()
	preserved := url.Values{}
	for k, vs := range q {
		switch k {
		case "page", "per_page", "sort", "dir", "q":
			continue
		}
		preserved[k] = vs
	}
	p := Pagination{
		Page:      1,
		PerPage:   25,
		Sort:      defaultSort,
		Dir:       "desc",
		Q:         q.Get("q"),
		BaseURL:   baseURL,
		preserved: preserved,
	}
	if v := q.Get("page"); v != "" {
		if n, _ := strconv.Atoi(v); n > 0 {
			p.Page = n
		}
	}
	if v := q.Get("per_page"); v != "" {
		if n, _ := strconv.Atoi(v); n >= 10 && n <= 1000 {
			p.PerPage = n
		}
	}
	if v := q.Get("sort"); v != "" {
		if _, ok := allowedSorts[v]; ok {
			p.Sort = v
		}
	}
	if v := q.Get("dir"); v == "asc" || v == "desc" {
		p.Dir = v
	}
	return p
}

// orderByClause builds " ORDER BY <expr> <dir>" from the whitelist + state in
// p. Falls back to the default if the column isn't recognized.
func orderByClause(allowed map[string]string, p Pagination) string {
	expr, ok := allowed[p.Sort]
	if !ok {
		// default to the first allowed key
		for _, e := range allowed {
			expr = e
			break
		}
	}
	dir := "DESC"
	if p.Dir == "asc" {
		dir = "ASC"
	}
	return fmt.Sprintf(" ORDER BY %s %s", expr, dir)
}
