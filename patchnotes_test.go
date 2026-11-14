package main

import (
	"strings"
	"testing"
)

// The page's feed puts each description's HTML in as markup; other feeds
// escape it or wrap it in CDATA. All three must come out as the same HTML.
func TestParsePatchNotes(t *testing.T) {
	feed := `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0" xmlns:atom="http://www.w3.org/2005/Atom"><channel>
  <title>Patch Notes</title>
  <link>https://example.org/#patch-notes</link>
  <atom:link href="https://example.org/feed.xml" rel="self" type="application/rss+xml" />
  <item>
    <title>Patch notes 2026-11-12</title>
    <link>https://example.org/#a</link>
    <pubDate>Thu, 12 Nov 2026 00:00:00 +0000</pubDate>
    <description><p><strong>Social</strong></p><ul><li>One &amp; two</li></ul></description>
  </item>
  <item>
    <title>Escaped</title>
    <pubDate>not a date</pubDate>
    <description>&lt;p&gt;x&lt;/p&gt;</description>
  </item>
  <item>
    <title>CDATA</title>
    <description><![CDATA[<p>y</p>]]></description>
  </item>
  <item><title></title><description></description></item>
</channel></rss>`
	notes, link, err := parsePatchNotes(strings.NewReader(feed))
	if err != nil {
		t.Fatal(err)
	}
	if link != "https://example.org/#patch-notes" {
		t.Errorf("link = %q", link)
	}
	if len(notes) != 3 {
		t.Fatalf("got %d notes, want 3 (the empty item is skipped): %+v", len(notes), notes)
	}
	first := notes[0]
	if first.Title != "Patch notes 2026-11-12" || first.Date != "2026-11-12" || first.Link != "https://example.org/#a" {
		t.Errorf("first = %+v", first)
	}
	if first.HTML != `<p><strong>Social</strong></p><ul><li>One &amp; two</li></ul>` {
		t.Errorf("first.HTML = %q", first.HTML)
	}
	if notes[1].HTML != "<p>x</p>" || notes[1].Date != "" {
		t.Errorf("escaped = %+v", notes[1])
	}
	if notes[2].HTML != "<p>y</p>" {
		t.Errorf("cdata = %+v", notes[2])
	}
}

func TestParsePatchNotesLimitsAndErrors(t *testing.T) {
	var b strings.Builder
	b.WriteString(`<rss><channel>`)
	for i := 0; i < patchNotesMax+5; i++ {
		b.WriteString(`<item><title>n</title></item>`)
	}
	b.WriteString(`</channel></rss>`)
	notes, _, err := parsePatchNotes(strings.NewReader(b.String()))
	if err != nil || len(notes) != patchNotesMax {
		t.Errorf("got %d notes, err %v; want %d", len(notes), err, patchNotesMax)
	}
	if _, _, err := parsePatchNotes(strings.NewReader(`<rss><channel></channel></rss>`)); err == nil {
		t.Error("an empty feed must be an error")
	}
	if _, _, err := parsePatchNotes(strings.NewReader(`<html>not a feed`)); err == nil {
		t.Error("a broken feed must be an error")
	}
}
