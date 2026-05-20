package messages

import (
	"regexp"
	"strings"

	"github.com/niuma/kirocc-pro/internal/anthropic"
)

const webFetchBodyLimit = 12000

var dataImagePattern = regexp.MustCompile(`!\[[^\]]*\]\(data:image/[^)]*\)`)

func trimWebFetchRequest(req *anthropic.Request) (int, int, bool) {
	for mi := len(req.Messages) - 1; mi >= 0; mi-- {
		msg := &req.Messages[mi]
		if msg.Role != "user" {
			continue
		}
		if msg.Content.IsString() {
			old := msg.Content.Text
			trimmed, ok := trimWebFetchText(old)
			if ok {
				msg.Content.Text = trimmed
				return len(old), len(trimmed), true
			}
			continue
		}
		for bi := len(msg.Content.Blocks) - 1; bi >= 0; bi-- {
			block := &msg.Content.Blocks[bi]
			if block.Type != anthropic.BlockTypeText {
				continue
			}
			old := block.Text
			trimmed, ok := trimWebFetchText(old)
			if ok {
				block.Text = trimmed
				return len(old), len(trimmed), true
			}
		}
	}
	return 0, 0, false
}

func trimWebFetchText(text string) (string, bool) {
	marker := "Web page content:\n---\n"
	idx := strings.Index(text, marker)
	if idx < 0 {
		return text, false
	}
	bodyStart := idx + len(marker)
	rest := text[bodyStart:]
	sep := strings.LastIndex(rest, "\n---\n\n")
	if sep < 0 {
		sep = strings.LastIndex(rest, "\n---\n")
	}
	if sep < 0 {
		return text, false
	}
	body := rest[:sep]
	if len(body) <= webFetchBodyLimit {
		return text, false
	}
	compact := compactWebFetchBody(body, webFetchBodyLimit)
	if len(compact) >= len(body)-1000 {
		return text, false
	}
	return text[:bodyStart] + compact + "\n\n[Proxy note: navigation, footer, repeated links, and image data were trimmed before model processing.]" + rest[sep:], true
}

func compactWebFetchBody(body string, limit int) string {
	body = dataImagePattern.ReplaceAllString(body, "")
	lines := strings.Split(body, "\n")
	kept := make([]string, 0, len(lines))
	seen := map[string]struct{}{}
	chars := 0
	blank := false
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			if !blank && len(kept) > 0 {
				kept = append(kept, "")
				chars++
				blank = true
			}
			continue
		}
		if isWebFetchNoiseLine(line) {
			continue
		}
		line = trimLongWebFetchLine(line)
		key := normalizeWebFetchLine(line)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		if chars+len(line)+1 > limit {
			break
		}
		kept = append(kept, line)
		chars += len(line) + 1
		blank = false
	}
	if chars < 1200 {
		body = strings.TrimSpace(body)
		if len(body) > limit {
			body = body[:limit]
		}
		return body
	}
	return strings.TrimSpace(strings.Join(kept, "\n"))
}

func isWebFetchNoiseLine(line string) bool {
	lower := strings.ToLower(line)
	if strings.HasPrefix(line, "![") || strings.HasPrefix(line, "[](") {
		return true
	}
	if strings.Count(line, "[") >= 4 && len([]rune(line)) < 240 {
		return true
	}
	noise := []string{
		"login", "sign in", "create an account", "password", "username", "privacy policy",
		"terms of service", "terms of services", "copyright", "all rights reserved", "open main menu",
		"close menu", "打开主菜单", "关闭菜单", "sponsored by", "start free", "forgot your password",
		"recover your password", "popular category", "editor picks", "social", "twitter", "facebook",
		"linkedin", "instagram", "patreon", "buy me a coffee", "support", "legal", "friends",
	}
	for _, phrase := range noise {
		if strings.Contains(lower, phrase) && len([]rune(line)) < 260 {
			return true
		}
	}
	return false
}

func trimLongWebFetchLine(line string) string {
	runes := []rune(line)
	if len(runes) <= 900 {
		return line
	}
	return string(runes[:900]) + "..."
}

func normalizeWebFetchLine(line string) string {
	line = strings.ToLower(line)
	line = strings.Join(strings.Fields(line), " ")
	if len(line) > 180 {
		line = line[:180]
	}
	return line
}
