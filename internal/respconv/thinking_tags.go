package respconv

import "strings"

// thinkingOpenTag and thinkingCloseTag are the XML tags used for prompt-injected thinking.
const (
	thinkingOpenTag  = "<thinking>"
	thinkingCloseTag = "</thinking>"
)

// parseThinkingTags parses <thinking>...</thinking> tags from streaming text.
// Returns (textOut, thinkingOut) — the portions to route to text and thinking buffers.
// Handles tags split across chunk boundaries via thinkingTagBuf.
func (a *responseAccumulator) parseThinkingTags(delta string) (textOut, thinkingOut string) {
	// Fast path: no buffered partial tag, not inside thinking, and no tag in delta.
	if a.thinkingTagBuf == "" && !a.thinkingTagInside && !strings.Contains(delta, "<") {
		return delta, ""
	}

	a.thinkingTagBuf += delta

	var textBuilder, thinkBuilder strings.Builder
	canStartThinking := func(prefix string) bool {
		// Treat <thinking> as a control tag only at the very beginning of a
		// response, allowing leading whitespace. After visible text starts, the
		// same literal tag may appear in code/docs and must stay visible text.
		return !a.HasText && a.TextBuf.Len() == 0 && textBuilder.Len() == 0 && strings.TrimSpace(prefix) == ""
	}

	for len(a.thinkingTagBuf) > 0 {
		if a.thinkingTagInside {
			// Look for </thinking> close tag.
			idx := strings.Index(a.thinkingTagBuf, thinkingCloseTag)
			if idx >= 0 {
				// Found close tag — emit thinking content before it.
				thinkBuilder.WriteString(a.thinkingTagBuf[:idx])
				a.thinkingTagBuf = a.thinkingTagBuf[idx+len(thinkingCloseTag):]
				a.thinkingTagInside = false
				continue
			}
			// No close tag found. Check if the tail could be a partial </thinking>.
			keep := partialTagSuffix(a.thinkingTagBuf, thinkingCloseTag)
			if keep > 0 {
				// Emit everything except the potential partial match.
				thinkBuilder.WriteString(a.thinkingTagBuf[:len(a.thinkingTagBuf)-keep])
				a.thinkingTagBuf = a.thinkingTagBuf[len(a.thinkingTagBuf)-keep:]
			} else {
				// No partial match — emit all as thinking.
				thinkBuilder.WriteString(a.thinkingTagBuf)
				a.thinkingTagBuf = ""
			}
			break
		}

		// Outside thinking — look for <thinking> open tag.
		idx := strings.Index(a.thinkingTagBuf, thinkingOpenTag)
		if idx >= 0 {
			prefix := a.thinkingTagBuf[:idx]
			if !canStartThinking(prefix) {
				textBuilder.WriteString(a.thinkingTagBuf)
				a.thinkingTagBuf = ""
				break
			}
			// Found open tag — emit text before it.
			a.thinkingTagBuf = a.thinkingTagBuf[idx+len(thinkingOpenTag):]
			a.thinkingTagInside = true
			a.suppressReasoningContent = true
			continue
		}
		// No open tag found. Check if the tail could be a partial <thinking>.
		keep := partialTagSuffix(a.thinkingTagBuf, thinkingOpenTag)
		prefix := a.thinkingTagBuf[:len(a.thinkingTagBuf)-keep]
		if keep > 0 && canStartThinking(prefix) {
			a.thinkingTagBuf = a.thinkingTagBuf[len(a.thinkingTagBuf)-keep:]
		} else {
			textBuilder.WriteString(a.thinkingTagBuf)
			a.thinkingTagBuf = ""
		}
		break
	}

	return textBuilder.String(), thinkBuilder.String()
}

// finalizeThinkingTags flushes any remaining content in the thinking tag buffer.
// Must be called before flushStopSeqPending at stream end.
func (a *responseAccumulator) finalizeThinkingTags() (textOut, thinkingOut string) {
	if a.thinkingTagBuf == "" {
		return "", ""
	}
	remaining := a.thinkingTagBuf
	a.thinkingTagBuf = ""
	if a.thinkingTagInside {
		return "", remaining
	}
	return remaining, ""
}

// partialTagSuffix returns the length of the longest suffix of s that is a prefix of tag.
// Returns 0 if no suffix of s matches a prefix of tag.
func partialTagSuffix(s, tag string) int {
	maxLen := min(len(tag)-1, len(s))
	for n := maxLen; n > 0; n-- {
		if s[len(s)-n:] == tag[:n] {
			return n
		}
	}
	return 0
}
