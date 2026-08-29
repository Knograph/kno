package mine

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"
	"time"
)

// parseMarkdownFile reads a markdown transcript.
//
// The pinned schema:
//
//	# Thread title           an H1 opens a thread (optional)
//	**Alice:** hello         a **speaker:** line opens a message
//	> 2026-01-02 15:04       an optional ISO-8601 timestamp line
//	more of the message      content until the next speaker line
//	---                      a rule opens a new thread
//
// The speaker named by --agent-name is the agent; every other speaker is
// human, and every speaker appears in the returned inventory, so a
// transcript whose export names the agent differently is visible in the
// run's summary instead of silently mispaired. A plain "Name: content"
// line is accepted for single-token names ("Human:", "AI:"); multi-word
// names must use the bold form, because a sentence like "Please note: the
// refund arrives Tuesday" would otherwise be read as a speaker line.
//
// A markdown transcript without --agent-name is refused: guessing which
// side the agent is would pair the wrong messages.
func parseMarkdownFile(ctx context.Context, path, agentName string) ([]message, map[string]int, error) {
	if agentName == "" {
		return nil, nil, ErrAgentNameRequired
	}
	f, err := os.Open(path) //nolint:gosec // an explicit user-supplied transcript path: reading it is the command's contract, not attacker-controlled inclusion
	if err != nil {
		return nil, nil, fmt.Errorf("opening %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), maxLineBytes)

	var msgs []message
	speakers := make(map[string]int)
	threadTitle := ""
	var cur *message
	prevWasSpeaker := false

	flush := func() {
		if cur != nil && strings.TrimSpace(cur.content) != "" {
			msgs = append(msgs, *cur)
		}
		cur = nil
	}

	line := 0
	for sc.Scan() {
		line++
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		trimmed := strings.TrimSpace(strings.TrimRight(sc.Text(), "\r"))
		switch {
		case trimmed == "":
			prevWasSpeaker = false
			continue
		case strings.HasPrefix(trimmed, "# "):
			flush()
			threadTitle = strings.TrimSpace(strings.TrimPrefix(trimmed, "# "))
			prevWasSpeaker = false
		case trimmed == "---" || trimmed == "***" || trimmed == "___":
			flush()
			threadTitle = ""
			prevWasSpeaker = false
		default:
			if name, content, ok := splitSpeaker(trimmed); ok {
				flush()
				speakers[name]++
				m := message{
					role:    roleHuman,
					id:      fmt.Sprintf("%s:%d", path, line),
					content: content,
					thread:  threadTitle,
				}
				if strings.EqualFold(name, agentName) {
					m.role = roleAgent
				}
				cur = &m
				prevWasSpeaker = true
				continue
			}
			if t, ok := parseMarkdownTime(trimmed); ok && prevWasSpeaker && cur != nil {
				// A timestamp directly after a speaker line belongs to that
				// message. Timestamps elsewhere are content.
				cur.time = t
				prevWasSpeaker = false
				continue
			}
			prevWasSpeaker = false
			if cur == nil {
				// Prose before the first speaker line is a preamble, not a
				// message; there is no speaker to attribute it to.
				continue
			}
			if cur.content != "" {
				cur.content += "\n"
			}
			cur.content += trimmed
		}
	}
	flush()
	if err := sc.Err(); err != nil {
		return nil, nil, fmt.Errorf("%s line %d: %w (lines are capped at %d bytes)",
			path, line+1, err, maxLineBytes)
	}
	return msgs, speakers, nil
}

// splitSpeaker splits a **Name:** or single-token Name: line. The second
// return is the message content, which may be empty.
func splitSpeaker(line string) (string, string, bool) {
	if strings.HasPrefix(line, "**") {
		rest := line[2:]
		j := strings.Index(rest, "**")
		if j <= 0 {
			return "", "", false
		}
		// The pinned form is **Name:** — the closing stars come AFTER the
		// colon. A stray "**Name** content" line is content, not a speaker.
		name := strings.TrimSpace(strings.TrimSuffix(strings.TrimSuffix(rest[:j], ":"), "|"))
		content := strings.TrimSpace(strings.TrimLeft(rest[j+2:], ":|"))
		return name, content, name != ""
	}
	i := strings.Index(line, ":")
	if i <= 0 {
		return "", "", false
	}
	name := strings.TrimSpace(line[:i])
	// A single-token name cannot collide with a sentence ("Please note:
	// ..."); multi-word names use the bold form.
	if name == "" || strings.ContainsAny(name, " \t!?.,") {
		return "", "", false
	}
	return name, strings.TrimSpace(line[i+1:]), true
}

// parseMarkdownTime parses the optional timestamp line: RFC 3339, or the
// space-separated YYYY-MM-DD HH:MM[:SS] forms, with an optional blockquote
// marker.
func parseMarkdownTime(s string) (time.Time, bool) {
	s = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(s), ">"))
	for _, layout := range []string{time.RFC3339, "2006-01-02 15:04:05", "2006-01-02 15:04", "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}
