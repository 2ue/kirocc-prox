package messages

import "testing"

func TestTrimWebFetchTextShrinksBoilerplate(t *testing.T) {
	body := "Web page content:\n---\n"
	for i := 0; i < 700; i++ {
		body += "![Logo](data:image/svg+xml;base64,AAAA)\n"
		body += "[Home](/)[AI News](/ai-news)[Pricing](/pricing)[Login](/signin)\n"
		body += "### Important AI update headline number " + string(rune('A'+(i%20))) + "\n"
		body += "This is a substantive paragraph about AI model releases, infrastructure, regulation, and developer tools in May 2026.\n"
	}
	body += "---\n\nSummarize the page"
	trimmed, ok := trimWebFetchText(body)
	if !ok {
		t.Fatal("expected trim")
	}
	if len(trimmed) >= len(body) {
		t.Fatalf("expected shorter text: before=%d after=%d", len(body), len(trimmed))
	}
	if len(trimmed) > 14000 {
		t.Fatalf("trimmed text too large: %d", len(trimmed))
	}
	if !contains(trimmed, "Important AI update headline") {
		t.Fatal("substantive heading removed")
	}
}

func contains(s, sub string) bool { return stringsContains(s, sub) }

func stringsContains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
