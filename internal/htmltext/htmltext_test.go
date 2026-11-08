package htmltext

import (
	"strings"
	"testing"
)

func TestToReadableIgnoresUnsafeAndNonTextContent(t *testing.T) {
	got := ToReadable(`<head>hidden</head><style>.x{}</style><script>alert(1)</script><noscript>no</noscript><template>tmpl</template><svg>vector</svg><iframe>frame</iframe><object>object</object><canvas>canvas</canvas><audio>audio</audio><video>video</video><p>Hello</p><img src="https://example.test/x.png">`)
	for _, leaked := range []string{"hidden", "alert", "no", "tmpl", "vector", "frame", "object", "canvas", "audio", "video", "example.test/x.png"} {
		if strings.Contains(got, leaked) {
			t.Fatalf("unsafe/non-text content leaked %q in %q", leaked, got)
		}
	}
	if got != "Hello" {
		t.Fatalf("readable = %q, want Hello", got)
	}
}

func TestToReadableLinksListsBlocksAndWhitespace(t *testing.T) {
	got := ToReadable(`<div>Alpha <a href="https://example.test/path">site</a></div><ul><li> One </li><li><a href="mailto:a@example.test"></a></li></ul><table><tr><td>A</td><td>B</td></tr></table>`)
	for _, want := range []string{"Alpha site (https://example.test/path)", "- One", "- mailto:a@example.test", "A B"} {
		if !strings.Contains(got, want) {
			t.Fatalf("readable = %q, missing %q", got, want)
		}
	}
	if strings.Contains(got, "  ") || strings.Contains(got, "\n\n\n") {
		t.Fatalf("whitespace not normalized: %q", got)
	}
}

func TestToReadableMalformedAndEmpty(t *testing.T) {
	if got := ToReadable(" \n\t "); got != "" {
		t.Fatalf("empty = %q", got)
	}
	got := ToReadable(`<p>Open <b>tag<script>ignored`)
	if got != "Open tag" {
		t.Fatalf("malformed = %q, want Open tag", got)
	}
}

// Markdown sends turn their HTML into the text part this way. A tight list
// must not get blank lines between its items.
func TestToReadableKeepsListItemsTogether(t *testing.T) {
	got := ToReadable("<p>Hallo,</p>\n<ul>\n<li>eins</li>\n<li>zwei</li>\n</ul>\n<p>Gruß</p>\n")
	want := "Hallo,\n\n- eins\n- zwei\n\nGruß"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}
