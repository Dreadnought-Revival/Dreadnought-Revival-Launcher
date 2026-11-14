package main

// Patch notes: the project page's RSS feed, shown in the window's right-hand
// panel. Fetched over ordinary, fully verified HTTPS (updateHTTPClient), not
// through launcherHTTPClient, which sends every connection to the game server.
// The feed is the project's, so it is the same whichever server is picked.

import (
	"encoding/xml"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"strings"
	"time"
)

// patchNotesURL is the feed. Override at build time
// (-X main.patchNotesURL=...).
var patchNotesURL = "https://dreadnought-revival.github.io/Dreadnought-Revival-Page/feed.xml"

// patreonURL is the project's Patreon page (linked from the project page).
var patreonURL = "https://www.patreon.com/cw/DARKACE39"

const (
	patchNotesTimeout  = 10 * time.Second
	patchNotesMaxBytes = 4 << 20
	// patchNotesMax is how many entries the panel lists, newest first.
	patchNotesMax = 10
)

type patchNote struct {
	Title string `json:"title"`
	Link  string `json:"link,omitempty"`
	Date  string `json:"date,omitempty"` // YYYY-MM-DD, "" when pubDate does not parse
	HTML  string `json:"html"`           // the entry's description; the page keeps only harmless tags
}

type rssDoc struct {
	Channel struct {
		// Every <link>: encoding/xml matches by local name, so the feed's
		// self-reference <atom:link href=.../> lands here too (and, as a
		// plain string field, overwrote the real link with "").
		Links []struct {
			Value string `xml:",chardata"`
		} `xml:"link"`
		Items []struct {
			Title   string `xml:"title"`
			Link    string `xml:"link"`
			PubDate string `xml:"pubDate"`
			// innerxml: the feed puts the HTML in as markup, not as escaped
			// text (descriptionHTML handles both, and CDATA).
			Description struct {
				Inner string `xml:",innerxml"`
			} `xml:"description"`
		} `xml:"item"`
	} `xml:"channel"`
}

func fetchPatchNotes() ([]patchNote, string, error) {
	req, err := http.NewRequest(http.MethodGet, patchNotesURL, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("User-Agent", "Dreadnought-Revival-launcher/"+launcherVersion)
	resp, err := (&http.Client{Timeout: patchNotesTimeout}).Do(req)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("patch notes: HTTP %d", resp.StatusCode)
	}
	return parsePatchNotes(io.LimitReader(resp.Body, patchNotesMaxBytes))
}

// parsePatchNotes reads an RSS 2.0 feed: its entries (at most patchNotesMax,
// in feed order) and the channel's link.
func parsePatchNotes(r io.Reader) ([]patchNote, string, error) {
	var doc rssDoc
	if err := xml.NewDecoder(r).Decode(&doc); err != nil {
		return nil, "", fmt.Errorf("patch notes: %w", err)
	}
	var out []patchNote
	for _, it := range doc.Channel.Items {
		if len(out) == patchNotesMax {
			break
		}
		n := patchNote{Title: strings.TrimSpace(it.Title), Link: strings.TrimSpace(it.Link),
			HTML: descriptionHTML(it.Description.Inner)}
		if t, err := time.Parse(time.RFC1123Z, strings.TrimSpace(it.PubDate)); err == nil {
			n.Date = t.UTC().Format("2006-01-02")
		}
		if n.Title == "" && n.HTML == "" {
			continue
		}
		out = append(out, n)
	}
	if len(out) == 0 {
		return nil, "", errors.New("patch notes: the feed has no entries")
	}
	link := ""
	for _, l := range doc.Channel.Links {
		if link = strings.TrimSpace(l.Value); link != "" {
			break
		}
	}
	return out, link, nil
}

// descriptionHTML turns a description's raw XML content into HTML: markup
// as-is, a CDATA section unwrapped, escaped HTML ("&lt;p&gt;") unescaped.
func descriptionHTML(inner string) string {
	s := strings.TrimSpace(inner)
	if strings.HasPrefix(s, "<![CDATA[") && strings.HasSuffix(s, "]]>") {
		return strings.TrimSpace(s[len("<![CDATA[") : len(s)-len("]]>")])
	}
	if !strings.Contains(s, "<") {
		return html.UnescapeString(s)
	}
	return s
}
