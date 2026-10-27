// Package htmltext turns the HTML of a mail into plain text for the terminal.
package htmltext

import (
	"strings"
	"unicode"

	"golang.org/x/net/html"
)

var ignoredTags = map[string]bool{
	"audio": true, "canvas": true, "head": true, "iframe": true, "noscript": true,
	"object": true, "script": true, "style": true, "svg": true, "template": true,
	"title": true, "video": true,
}

var blockTags = map[string]bool{
	"article": true, "blockquote": true, "br": true, "dd": true, "div": true,
	"dl": true, "dt": true, "footer": true, "h1": true, "h2": true, "h3": true,
	"h4": true, "h5": true, "h6": true, "header": true, "hr": true, "ol": true,
	"p": true, "pre": true, "section": true, "table": true, "tr": true,
	"ul": true,
}

// linkContext remembers where a link's visible text started, so the URL is
// added only when the text does not already show it.
type linkContext struct {
	href  string
	start int
}

// ToReadable turns HTML into plain text that is safe to print in a terminal.
// It only reads the string it gets. It never loads images or other remote
// content and never runs scripts.
func ToReadable(htmlContent string) string {
	if strings.TrimSpace(htmlContent) == "" {
		return ""
	}
	var b strings.Builder
	tokenizer := html.NewTokenizer(strings.NewReader(htmlContent))
	ignoreDepth := 0
	var links []linkContext
	for {
		tt := tokenizer.Next()
		switch tt {
		case html.ErrorToken:
			return cleanup(b.String())
		case html.TextToken:
			if ignoreDepth == 0 {
				appendText(&b, string(tokenizer.Text()))
			}
		case html.StartTagToken, html.SelfClosingTagToken:
			nameBytes, hasAttr := tokenizer.TagName()
			name := strings.ToLower(string(nameBytes))
			if ignoredTags[name] {
				// Leave out scripts, styles and media. The depth counter
				// also covers broken HTML that nests these tags.
				if tt == html.StartTagToken {
					ignoreDepth++
				}
				continue
			}
			if ignoreDepth > 0 {
				continue
			}
			switch name {
			case "li":
				appendNewline(&b)
				b.WriteString("- ")
			case "td", "th":
				appendSpace(&b)
			case "a":
				if href := linkHref(tokenizer, hasAttr); usefulHref(href) {
					// Links inside links are invalid HTML, but mail has them.
					// The stack handles them in a predictable way.
					links = append(links, linkContext{href: href, start: b.Len()})
				}
			default:
				if blockTags[name] {
					appendNewline(&b)
				}
			}
		case html.EndTagToken:
			nameBytes, _ := tokenizer.TagName()
			name := strings.ToLower(string(nameBytes))
			if ignoreDepth > 0 {
				if ignoredTags[name] {
					ignoreDepth--
				}
				continue
			}
			switch name {
			case "a":
				if len(links) > 0 {
					ctx := links[len(links)-1]
					links = links[:len(links)-1]
					appendLinkURL(&b, ctx)
				}
			case "li", "tr":
				appendNewline(&b)
			default:
				if blockTags[name] {
					appendNewline(&b)
				}
			}
		}
	}
}

func linkHref(tokenizer *html.Tokenizer, hasAttr bool) string {
	if !hasAttr {
		return ""
	}
	for {
		key, value, more := tokenizer.TagAttr()
		if strings.EqualFold(string(key), "href") {
			return strings.TrimSpace(string(value))
		}
		if !more {
			return ""
		}
	}
}

func usefulHref(href string) bool {
	lower := strings.ToLower(href)
	return strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://") || strings.HasPrefix(lower, "mailto:")
}

// appendLinkURL writes "text (URL)", or nothing extra when the visible text
// already is the URL.
func appendLinkURL(b *strings.Builder, ctx linkContext) {
	text := strings.TrimSpace(b.String()[ctx.start:])
	if text == "" {
		appendText(b, ctx.href)
		return
	}
	if strings.EqualFold(strings.TrimSpace(text), ctx.href) {
		return
	}
	b.WriteString(" (")
	b.WriteString(ctx.href)
	b.WriteString(")")
}

func appendText(b *strings.Builder, raw string) {
	text, leading, trailing := collapseSpaces(raw)
	if text == "" {
		return
	}
	if leading {
		appendSpace(b)
	}
	b.WriteString(text)
	if trailing {
		appendSpace(b)
	}
}

// collapseSpaces turns each run of whitespace into one space. It also
// reports whether the text started or ended with whitespace, so words in
// neighbouring tags stay apart.
func collapseSpaces(raw string) (text string, leading, trailing bool) {
	if raw == "" {
		return "", false, false
	}
	runes := []rune(raw)
	leading = len(runes) > 0 && unicode.IsSpace(runes[0])
	trailing = len(runes) > 0 && unicode.IsSpace(runes[len(runes)-1])
	var b strings.Builder
	inSpace := false
	for _, r := range runes {
		if unicode.IsSpace(r) {
			inSpace = true
			continue
		}
		if inSpace && b.Len() > 0 {
			b.WriteByte(' ')
		}
		b.WriteRune(r)
		inSpace = false
	}
	return b.String(), leading, trailing
}

func appendSpace(b *strings.Builder) {
	if b.Len() == 0 {
		return
	}
	s := b.String()
	last := s[len(s)-1]
	if last == ' ' || last == '\n' {
		return
	}
	b.WriteByte(' ')
}

// appendNewline drops a trailing space, then adds a line break. It never
// writes more than one blank line in a row.
func appendNewline(b *strings.Builder) {
	s := b.String()
	s = strings.TrimRight(s, " ")
	b.Reset()
	b.WriteString(s)
	if b.Len() == 0 {
		return
	}
	if strings.HasSuffix(b.String(), "\n\n") {
		return
	}
	b.WriteByte('\n')
}

func cleanup(in string) string {
	lines := strings.Split(in, "\n")
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], " ")
	}
	out := strings.Join(lines, "\n")
	for strings.Contains(out, "\n\n\n") {
		out = strings.ReplaceAll(out, "\n\n\n", "\n\n")
	}
	return strings.TrimSpace(out)
}
