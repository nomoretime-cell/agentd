package agent

import (
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

const anthropicNewsroom = "https://www.anthropic.com/news"

// The newsroom groups each announcement's linked headline and publication day
// in one listing row. Category links and expanded previews also occur in the row.
func readAnthropicNews(r io.Reader, source string) ([]newsItem, error) {
	doc, err := html.Parse(r)
	if err != nil {
		return nil, err
	}
	base, _ := url.Parse(anthropicNewsroom)
	var items []newsItem
	for row := range doc.Descendants() {
		if row.DataAtom != atom.Li || htmlAttribute(row, "data-listing-row") == "" {
			continue
		}
		item := newsItem{Source: source, Translate: true, DateOnly: true}
		for node := range row.Descendants() {
			switch node.DataAtom {
			case atom.Time:
				item.Date, _ = time.Parse("Jan 2, 2006", htmlText(node))
			case atom.H3:
				if item.Link != "" {
					continue
				}
				for parent := range node.Ancestors() {
					if parent == row {
						break
					}
					if parent.DataAtom != atom.A {
						continue
					}
					href := htmlAttribute(parent, "href")
					link, err := base.Parse(href)
					if href != "" && err == nil && link.Scheme == "https" && link.Host == base.Host && link.User == nil {
						item.Title, item.Link = htmlText(node), link.String()
					}
					break
				}
			}
		}
		if item.Title != "" && !item.Date.IsZero() && !item.Date.After(time.Now()) {
			items = append(items, item)
		}
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("Anthropic newsroom contained no dated headlines")
	}
	return items, nil
}

func htmlAttribute(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

func htmlText(n *html.Node) string {
	var b strings.Builder
	for child := range n.Descendants() {
		if child.Type == html.TextNode {
			b.WriteString(child.Data)
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}
