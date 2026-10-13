package jira

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
)

// adfNode is one node of an Atlassian Document Format tree.
type adfNode struct {
	Type    string         `json:"type"`
	Text    string         `json:"text"`
	Attrs   map[string]any `json:"attrs"`
	Content []adfNode      `json:"content"`
	Marks   []adfMark      `json:"marks"`
}

type adfMark struct {
	Type  string         `json:"type"`
	Attrs map[string]any `json:"attrs"`
}

// ADFText renders an ADF document to Markdown-flavoured plain text: headings, lists,
// task lists, code blocks, links, and mentions survive; styling is dropped. A JSON string
// (Jira's v2 wiki-markup form) is returned as-is; anything unparseable renders empty.
func ADFText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return strings.TrimSpace(s)
	}
	var doc adfNode
	if err := json.Unmarshal(raw, &doc); err != nil {
		return ""
	}
	var b strings.Builder
	renderBlocks(&b, doc.Content, "")
	return strings.TrimSpace(collapseBlankLines(b.String()))
}

func renderBlocks(b *strings.Builder, nodes []adfNode, indent string) {
	for _, n := range nodes {
		switch n.Type {
		case "paragraph":
			b.WriteString(indent + renderInline(n.Content) + "\n\n")
		case "heading":
			level := 1
			if l, ok := n.Attrs["level"].(float64); ok && l >= 1 && l <= 6 {
				level = int(l)
			}
			b.WriteString(strings.Repeat("#", level) + " " + renderInline(n.Content) + "\n\n")
		case "bulletList", "orderedList", "taskList":
			renderList(b, n, indent)
			b.WriteString("\n")
		case "codeBlock":
			lang, _ := n.Attrs["language"].(string)
			b.WriteString(indent + "```" + lang + "\n" + renderInline(n.Content) + "\n" + indent + "```\n\n")
		case "blockquote":
			var inner strings.Builder
			renderBlocks(&inner, n.Content, "")
			for _, line := range strings.Split(strings.TrimSpace(inner.String()), "\n") {
				b.WriteString(indent + "> " + line + "\n")
			}
			b.WriteString("\n")
		case "rule":
			b.WriteString(indent + "---\n\n")
		case "table":
			renderTable(b, n, indent)
		case "panel", "expand", "nestedExpand", "layoutSection", "layoutColumn":
			if title, ok := n.Attrs["title"].(string); ok && title != "" {
				b.WriteString(indent + title + ":\n")
			}
			renderBlocks(b, n.Content, indent)
		default:
			if len(n.Content) > 0 {
				renderBlocks(b, n.Content, indent)
			} else if t := renderInline([]adfNode{n}); t != "" {
				b.WriteString(indent + t + "\n\n")
			}
		}
	}
}

func renderList(b *strings.Builder, list adfNode, indent string) {
	for i, item := range list.Content {
		marker := "- "
		switch {
		case list.Type == "orderedList":
			marker = strconv.Itoa(i+1) + ". "
		case item.Type == "taskItem":
			if state, _ := item.Attrs["state"].(string); state == "DONE" {
				marker = "- [x] "
			} else {
				marker = "- [ ] "
			}
		}
		var first strings.Builder
		var nested []adfNode
		for _, c := range item.Content {
			switch c.Type {
			case "bulletList", "orderedList", "taskList":
				nested = append(nested, c)
			case "paragraph":
				if first.Len() > 0 {
					first.WriteString(" ")
				}
				first.WriteString(renderInline(c.Content))
			default:
				if first.Len() > 0 {
					first.WriteString(" ")
				}
				first.WriteString(renderInline([]adfNode{c}))
			}
		}
		b.WriteString(indent + marker + first.String() + "\n")
		for _, n := range nested {
			renderList(b, n, indent+"  ")
		}
	}
}

func renderTable(b *strings.Builder, table adfNode, indent string) {
	for _, row := range table.Content {
		var cells []string
		for _, cell := range row.Content {
			var cb strings.Builder
			renderBlocks(&cb, cell.Content, "")
			cells = append(cells, strings.ReplaceAll(strings.TrimSpace(cb.String()), "\n", " "))
		}
		b.WriteString(indent + "| " + strings.Join(cells, " | ") + " |\n")
	}
	b.WriteString("\n")
}

func renderInline(nodes []adfNode) string {
	var b strings.Builder
	for _, n := range nodes {
		switch n.Type {
		case "text":
			t := n.Text
			for _, m := range n.Marks {
				switch m.Type {
				case "code":
					t = "`" + t + "`"
				case "link":
					if href, _ := m.Attrs["href"].(string); href != "" && href != t {
						t = "[" + t + "](" + href + ")"
					}
				}
			}
			b.WriteString(t)
		case "hardBreak":
			b.WriteString("\n")
		case "mention":
			if t, _ := n.Attrs["text"].(string); t != "" {
				b.WriteString(t)
			}
		case "emoji":
			if t, _ := n.Attrs["text"].(string); t != "" {
				b.WriteString(t)
			} else if s, _ := n.Attrs["shortName"].(string); s != "" {
				b.WriteString(s)
			}
		case "inlineCard", "blockCard", "embedCard":
			if u, _ := n.Attrs["url"].(string); u != "" {
				b.WriteString(u)
			}
		case "status", "date":
			if t, _ := n.Attrs["text"].(string); t != "" {
				b.WriteString(t)
			}
		default:
			b.WriteString(renderInline(n.Content))
		}
	}
	return b.String()
}

var urlPattern = regexp.MustCompile(`https?://[^\s<>()]+`)

// TextToADF converts plain text to an ADF document: blank-line-separated paragraphs,
// single newlines as hard breaks, and bare URLs as links.
func TextToADF(text string) map[string]any {
	var paragraphs []any
	for _, para := range strings.Split(strings.TrimSpace(text), "\n\n") {
		para = strings.TrimSpace(para)
		if para == "" {
			continue
		}
		var inline []any
		for i, line := range strings.Split(para, "\n") {
			if i > 0 {
				inline = append(inline, map[string]any{"type": "hardBreak"})
			}
			inline = append(inline, linkify(line)...)
		}
		paragraphs = append(paragraphs, map[string]any{"type": "paragraph", "content": inline})
	}
	return map[string]any{"type": "doc", "version": 1, "content": paragraphs}
}

func linkify(line string) []any {
	var out []any
	last := 0
	for _, loc := range urlPattern.FindAllStringIndex(line, -1) {
		if loc[0] > last {
			out = append(out, map[string]any{"type": "text", "text": line[last:loc[0]]})
		}
		u := line[loc[0]:loc[1]]
		out = append(out, map[string]any{
			"type":  "text",
			"text":  u,
			"marks": []any{map[string]any{"type": "link", "attrs": map[string]any{"href": u}}},
		})
		last = loc[1]
	}
	if last < len(line) {
		out = append(out, map[string]any{"type": "text", "text": line[last:]})
	}
	return out
}

func collapseBlankLines(s string) string {
	for strings.Contains(s, "\n\n\n") {
		s = strings.ReplaceAll(s, "\n\n\n", "\n\n")
	}
	return s
}
