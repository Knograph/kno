package mine

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/knograph/kno/adapters/agent/pricing"
)

// maxLineBytes caps one transcript line. A transcript is messages, not a
// corpus; a line over the cap is a binary blob renamed .jsonl or a corrupt
// file, and reading it into memory in full is what the streaming profile of
// the whole pipeline exists to prevent.
const maxLineBytes = 4 << 20 // 4 MiB

// Mode selects how a human reply becomes a Case's expected.
type Mode int

const (
	// ModeResolution (default) uses a run's final human message as the
	// expected — the answer that closed the thread. It pairs each run's
	// opening question with what the human settled on, so the expected is
	// what the agent had to converge to; this is the mode whose expected can
	// be honest under exact-match, not just judge goals.
	ModeResolution Mode = iota

	// ModeImmediate pairs each agent answer with the human reply that
	// follows it. Replies are shaped and the modal classes — gratitude,
	// acknowledgment, escalation, quote-back, retraction, counter-question —
	// are filtered, each with its own count. An immediate reply was never
	// told to the agent as a requirement, so this mode's expectations are
	// honest under judge goals and honest under exact-match only when
	// shaped to a short answer.
	ModeImmediate
)

// String is the flag vocabulary for a resolution mode.
func (m Mode) String() string {
	switch m {
	case ModeImmediate:
		return "immediate"
	case ModeResolution:
		return "resolution"
	}
	return "unknown"
}

// DefaultMaxQuestionTokens is the cap a mined question must fit under.
//
// 32k tokens at the pricing approximation is well inside any frontier
// model's context window and far beyond any support question. The cap is a
// guard against an attachment or a pasted log being mined as a "question",
// not a filter on real exchanges.
const DefaultMaxQuestionTokens = 32_000

// Options configures a mine run.
type Options struct {
	// Logs are the transcript files or directories to mine. Directories are
	// walked for .jsonl, .md, .markdown, and .csv files, sorted for
	// determinism.
	Logs []string

	// Format is auto (sniff each file), jsonl-chat, markdown, or csv.
	Format string

	// AgentName marks the agent's speaker in markdown transcripts. Required
	// for markdown: without it there is no way to tell the agent's messages
	// from the human's, and guessing would pair the wrong sides.
	AgentName string

	// Mode selects how expectations are shaped. See Mode.
	Mode Mode

	// MaxQuestionTokens is the cap on a mined question, counted by the same
	// approximation reservations run on. A question alone over the cap is
	// dropped with a count, never truncated: truncation would fabricate an
	// input nobody asked. The cap is also folded into the case id, so
	// changing it re-ids the set loudly instead of quietly.
	MaxQuestionTokens int64

	// Manifest is the review manifest path. Read at mine time so curated
	// drops survive re-mining; written by Review. Absent when the file does
	// not exist.
	Manifest string
}

func (o Options) validate() error {
	if len(o.Logs) == 0 {
		return fmt.Errorf("mine: no --logs given")
	}
	switch o.Format {
	case "auto", "jsonl-chat", "markdown", "csv":
	default:
		return fmt.Errorf("mine: unknown format %q; pass auto, jsonl-chat, markdown, or csv", o.Format)
	}
	switch o.Mode {
	case ModeImmediate, ModeResolution:
	default:
		return fmt.Errorf("mine: unknown mode %d; pass resolution or immediate", o.Mode)
	}
	if o.MaxQuestionTokens <= 0 {
		return fmt.Errorf("mine: --max-question-tokens %d is not a usable cap; pass a positive number", o.MaxQuestionTokens)
	}
	return nil
}

// Case is one mined weak-label case, ready for the output writer.
//
// ID is the on-disk id — the content-keyed ULID-shaped id, re-derived when
// the manifest or review edits the expected. ident carries the ORIGINAL
// mined content, so the manifest key (the id the source produces) survives
// edits: a decision made on the pre-edit id is the decision re-mining can
// look up again.
type Case struct {
	ID        string
	Input     string
	Expected  string
	Note      string
	SourceRef string

	ident identity
}

// identity is the content a mined id is a pure function of.
type identity struct {
	thread   string
	question string
	expected string // the ORIGINAL expected, pre-edit
	cap      int64
	time     time.Time
}

// id is the id the SOURCE produces — the manifest key, which must not move
// when an edit changes the on-disk id.
func (i identity) id() string {
	return caseID(i.thread, i.question, i.expected, i.cap, i.time)
}

// reID re-derives the on-disk id after the expected changed. Deliberately
// recomputed from the original identity: the timestamp, thread, question,
// and cap are all properties of the SOURCE, never of the edit.
func (c *Case) reID() {
	c.ID = caseID(c.ident.thread, c.ident.question, c.Expected, c.ident.cap, c.ident.time)
}

// Counts is the mine run's ledger: every drop has a line in it, so a run
// that produced fewer cases than its transcripts promised is explained, not
// mysterious.
type Counts struct {
	// Mined is how many Cases survived to the output.
	Mined int

	// Filtered is each modal reply class's drop count. The classes are the
	// pairing rule's vocabulary: gratitude, acknowledgment, escalation,
	// quote-back, counter-question, retraction.
	Filtered map[FilterClass]int

	// Deduped is how many duplicate exchanges (same thread, question,
	// expected) were dropped across all --logs files.
	Deduped int

	// OverCap is how many questions exceeded the token cap.
	OverCap int

	// PreservedDrops is how many curated manifest drops were re-applied.
	PreservedDrops int

	// Pairing is the per-file pairing ledger: agent messages, human replies,
	// mined cases, filtered replies. The markdown pairing-rate summary is
	// built from it — a file where nothing pairs is visible, not silent.
	Pairing []FilePairing

	// Speakers is the aggregated markdown speaker inventory, name to message
	// count. A speaker nobody expected is the fastest way to spot a transcript
	// the agent-name flag has mis-wired.
	Speakers map[string]int
}

// FilePairing is one transcript file's pairing ledger.
type FilePairing struct {
	Path string

	// AgentMessages is the number of agent messages in the file.
	AgentMessages int

	// HumanReplies is the number of human replies that follow an agent
	// message (immediate mode) or the number of runs that close on a human
	// message (resolution mode).
	HumanReplies int

	// Mined is the number of Cases this file produced.
	Mined int

	// Filtered is the number of human replies this file had filtered.
	Filtered int
}

// Mine reads every transcript under opts.Logs and returns the mined Cases.
//
// The pipeline per file: sniff or take the format, parse the transcript,
// resolve thread identity, pair the exchange per the mode, classify and
// shape each human reply, then apply — in order — the token cap, the
// cross-file dedup map, and the review manifest. The cap and dedup come
// before the manifest so a question over the cap is dropped without an id
// and a duplicate is never looked up as if it were a curated case.
func Mine(ctx context.Context, opts Options) ([]Case, Counts, error) {
	if err := opts.validate(); err != nil {
		return nil, Counts{}, err
	}
	files, err := expandLogs(opts.Logs)
	if err != nil {
		return nil, Counts{}, err
	}
	manifest, err := loadManifest(opts.Manifest)
	if err != nil {
		return nil, Counts{}, err
	}

	counts := Counts{Filtered: make(map[FilterClass]int), Speakers: make(map[string]int)}
	seen := make(map[string]struct{})

	var cases []Case
	for _, path := range files {
		if err := ctx.Err(); err != nil {
			return nil, Counts{}, err
		}
		format, err := resolveFormat(opts.Format, path)
		if err != nil {
			return nil, Counts{}, err
		}
		if format == "csv" {
			pairs, err := parseCSVFile(ctx, path)
			if err != nil {
				return nil, Counts{}, err
			}
			for _, p := range pairs {
				addCase(&cases, &counts, seen, manifest, opts, "", p.question, p.answer, time.Time{}, path, "csv")
			}
			continue
		}
		msgs, speakers, err := parseTranscript(ctx, path, format, opts)
		if err != nil {
			return nil, Counts{}, err
		}
		for name, n := range speakers {
			counts.Speakers[name] += n
		}
		msgs = resolveThreads(msgs)
		mineFile(path, msgs, opts, &counts, seen, manifest, &cases)
	}
	return cases, counts, nil
}

// role is the parsed side of a message.
type role int

const (
	roleHuman role = iota
	roleAgent
)

// message is one transcript message, before pairing.
type message struct {
	role    role
	id      string // the format-provided message id, when the format has one
	content string
	thread  string // the format-provided thread id, when the format has one
	time    time.Time
}

// parseTranscript parses one non-csv transcript file.
func parseTranscript(ctx context.Context, path, format string, opts Options) ([]message, map[string]int, error) {
	switch format {
	case "jsonl-chat":
		msgs, err := parseChatFile(ctx, path)
		return msgs, nil, err
	case "markdown":
		return parseMarkdownFile(ctx, path, opts.AgentName)
	}
	return nil, nil, fmt.Errorf("mine: unknown transcript format %q", format)
}

// parseChatFile reads a jsonl-chat transcript.
//
// One message per line (format.go). The message's own thread_id is the
// thread identity when present; resolveThreads fills the identity from the
// opening message's id when it is not.
func parseChatFile(ctx context.Context, path string) ([]message, error) {
	f, err := os.Open(path) //nolint:gosec // an explicit user-supplied transcript path: reading it is the command's contract, not attacker-controlled inclusion
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), maxLineBytes)

	var msgs []message
	line := 0
	for sc.Scan() {
		line++
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		raw := bytes.TrimSpace(sc.Bytes())
		if len(raw) == 0 {
			continue
		}
		rec, err := decodeChat(raw)
		if err != nil {
			return nil, fmt.Errorf("%s line %d: %w", path, line, err)
		}
		if rec.ID == "" {
			return nil, fmt.Errorf(
				"%s line %d: message has no id; message ids anchor thread identity, "+
					"and a transcript without them cannot be paired or deduplicated",
				path, line,
			)
		}
		roleName := strings.ToLower(strings.TrimSpace(rec.Role))
		if !chatRoles[roleName] {
			return nil, fmt.Errorf(
				"%s line %d: role %q is not one of assistant, agent, user, human", path, line, rec.Role,
			)
		}
		if rec.Content == "" {
			return nil, fmt.Errorf("%s line %d: message %q has no content", path, line, rec.ID)
		}
		t, err := parseChatTime(rec.Timestamp)
		if err != nil {
			return nil, fmt.Errorf("%s line %d: %w", path, line, err)
		}
		m := message{id: rec.ID, content: rec.Content, thread: rec.ThreadID, time: t}
		if roleName == "assistant" || roleName == "agent" {
			m.role = roleAgent
		} else {
			m.role = roleHuman
		}
		msgs = append(msgs, m)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("%s line %d: %w (lines are capped at %d bytes)",
			path, line+1, err, maxLineBytes)
	}
	return msgs, nil
}

// resolveThreads assigns thread identity to messages that carry none.
//
// A run of messages between two human-openers is one thread, identified by
// its opening message's id — the same content-keyed stability rule as the
// case ids themselves: the identity is a property of the first message,
// never of a position. A format-provided thread id wins where it exists.
func resolveThreads(msgs []message) []message {
	var threadID string
	for i := range msgs {
		if msgs[i].thread != "" {
			threadID = msgs[i].thread
			continue
		}
		if i == 0 || (msgs[i].role == roleHuman && msgs[i-1].role == roleHuman) {
			threadID = msgs[i].id
		}
		msgs[i].thread = threadID
	}
	return msgs
}

// mineFile pairs one transcript file's messages and applies the mode.
//
// Immediate mode walks agent answers, pairing each with the human reply
// that follows it and the last human message before the answer. Resolution
// mode walks runs — consecutive messages sharing a thread — pairing the
// run's opening question with its final human message; a run that never
// gets a human answer, or whose final message is not human, produces
// nothing, because there is no resolution to mine.
func mineFile(path string, msgs []message, opts Options, counts *Counts, seen map[string]struct{}, m *Manifest, out *[]Case) {
	fp := FilePairing{Path: path}
	switch opts.Mode {
	case ModeImmediate:
		for i := 0; i < len(msgs); i++ {
			if msgs[i].role != roleAgent {
				continue
			}
			fp.AgentMessages++
			// Pair the LAST message of a consecutive agent run: it is the one
			// the human reply answers.
			end := i
			for end+1 < len(msgs) && msgs[end+1].role == roleAgent {
				end++
			}
			i = end
			replyIdx := end + 1
			if replyIdx >= len(msgs) || msgs[replyIdx].role != roleHuman {
				continue
			}
			fp.HumanReplies++
			qIdx := end - 1
			for qIdx >= 0 && msgs[qIdx].role != roleHuman {
				qIdx--
			}
			if qIdx < 0 {
				continue
			}
			class, expected := classifyReply(msgs[replyIdx].content, msgs[qIdx].content)
			if class == FilterNone && expected == "" {
				// The reply had no answer content at all (e.g. punctuation
				// only). Count it as the closest class: a non-answer.
				class = FilterCounterQuestion
			}
			if class != FilterNone {
				counts.Filtered[class]++
				fp.Filtered++
				continue
			}
			addCase(out, counts, seen, m, opts,
				msgs[qIdx].thread, msgs[qIdx].content, expected, msgs[replyIdx].time, path, opts.Mode.String())
			fp.Mined++
		}
	case ModeResolution:
		for start := 0; start < len(msgs); {
			// Run boundary: a human message that follows a human message
			// opens a new run.
			end := start + 1
			for end < len(msgs) {
				if msgs[end].role == roleHuman && msgs[end-1].role == roleHuman {
					break
				}
				end++
			}
			run := msgs[start:end]
			start = end

			var question *message
			hasAgent := false
			for i := range run {
				if run[i].role == roleAgent {
					hasAgent = true
				} else if question == nil {
					question = &run[i]
				}
			}
			last := run[len(run)-1]
			// No resolution: the run never got a human answer, or the agent
			// never answered the question at all.
			if last.role != roleHuman || !hasAgent || question == nil {
				continue
			}
			fp.AgentMessages += agentCount(run)
			fp.HumanReplies++
			class, expected := classifyReply(last.content, question.content)
			if class == FilterNone && expected == "" {
				class = FilterCounterQuestion
			}
			if class != FilterNone {
				counts.Filtered[class]++
				fp.Filtered++
				continue
			}
			addCase(out, counts, seen, m, opts,
				question.thread, question.content, expected, last.time, path, opts.Mode.String())
			fp.Mined++
		}
	}
	counts.Pairing = append(counts.Pairing, fp)
}

func agentCount(run []message) int {
	n := 0
	for _, m := range run {
		if m.role == roleAgent {
			n++
		}
	}
	return n
}

// addCase applies the token cap, the dedup map, and the review manifest to
// one mined candidate. The cap check comes first so an over-cap question is
// dropped without producing an id, and the manifest lookup is keyed by the
// source id — the id the un-edited content produces — so a review decision
// made once applies on every re-mine.
func addCase(out *[]Case, counts *Counts, seen map[string]struct{}, m *Manifest, opts Options,
	thread, question, expected string, t time.Time, src, modeLabel string,
) {
	if pricing.CountTokens(len(question), "") > opts.MaxQuestionTokens {
		counts.OverCap++
		return
	}
	ident := identity{thread: thread, question: question, expected: expected, cap: opts.MaxQuestionTokens, time: t}
	id := ident.id()
	if _, dup := seen[id]; dup {
		counts.Deduped++
		return
	}
	seen[id] = struct{}{}

	edited := false
	if d, ok := m.Decisions[id]; ok {
		switch d.Decision {
		case "drop":
			counts.PreservedDrops++
			return
		case "edit":
			expected = d.Expected
			edited = true
		}
	}
	if edited {
		id = caseID(thread, question, expected, opts.MaxQuestionTokens, t)
		if _, dup := seen[id]; dup {
			counts.Deduped++
			return
		}
		seen[id] = struct{}{}
	}

	note := fmt.Sprintf("mined from %s; mode=%s", src, modeLabel)
	if edited {
		note += "; expected edited by review manifest"
	}
	*out = append(*out, Case{
		ID:        id,
		Input:     question,
		Expected:  expected,
		Note:      note,
		SourceRef: src,
		ident:     ident,
	})
	counts.Mined++
}

// expandLogs resolves the --logs paths into the files to mine.
//
// A directory is walked for .jsonl, .md, .markdown, and .csv files; the
// walk is sorted so a given tree always mines in the same order. A path
// that names no files at all is an error: the user pointed at the wrong
// thing, and "mined 0 Cases" would hide that.
func expandLogs(paths []string) ([]string, error) {
	var files []string
	for _, p := range paths {
		st, err := os.Stat(p)
		if err != nil {
			return nil, fmt.Errorf("mine: %w", err)
		}
		if !st.IsDir() {
			files = append(files, p)
			continue
		}
		err = filepath.WalkDir(p, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			switch strings.ToLower(filepath.Ext(path)) {
			case ".jsonl", ".md", ".markdown", ".csv":
				files = append(files, path)
			}
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("walking %s: %w", p, err)
		}
	}
	sort.Strings(files)
	if len(files) == 0 {
		return nil, fmt.Errorf(
			"mine: no transcript files under --logs; look for .jsonl, .md, .markdown, or .csv files",
		)
	}
	return files, nil
}

// resolveFormat returns the format to parse the file with: the explicit
// --format when given, the sniffed format otherwise.
func resolveFormat(format, path string) (string, error) {
	if format != "" && format != "auto" {
		return format, nil
	}
	f, err := os.Open(path) //nolint:gosec // an explicit user-supplied transcript path: reading it is the command's contract, not attacker-controlled inclusion
	if err != nil {
		return "", fmt.Errorf("opening %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), maxLineBytes)
	for sc.Scan() {
		raw := bytes.TrimSpace(sc.Bytes())
		if len(raw) == 0 {
			continue
		}
		return sniff(path, raw)
	}
	if err := sc.Err(); err != nil {
		return "", fmt.Errorf("reading %s: %w", path, err)
	}
	return "", fmt.Errorf("%s is empty; pass a transcript file or a directory of transcripts", path)
}

// sniff names the format from the first non-empty line.
//
// A leading { is a JSON message; a heading or **speaker:** line is
// markdown; a header row naming the question and answer columns is csv.
// Anything else is refused with the candidates named — the failure mode of
// a guessed format is silent mispairing, so guessing is the one thing this
// deliberately does not do.
func sniff(path string, first []byte) (string, error) {
	if first[0] == '{' {
		return "jsonl-chat", nil
	}
	line := string(first)
	if strings.HasPrefix(line, "#") || strings.Contains(line, "**") {
		return "markdown", nil
	}
	if strings.Contains(line, ",") {
		has := func(want string) bool {
			for _, c := range strings.Split(line, ",") {
				if strings.EqualFold(strings.TrimSpace(c), want) {
					return true
				}
			}
			return false
		}
		if has("question") && has("answer") {
			return "csv", nil
		}
	}
	return "", fmt.Errorf(
		"cannot sniff a format for %s: the first line is not a JSON message, a markdown "+
			"heading or **speaker:** line, or a question,answer header; pass "+
			"--format jsonl-chat, markdown, or csv", path,
	)
}

// ErrAgentNameRequired is refused by the markdown parser: without the
// agent's speaker name there is no way to tell the agent's messages from
// the human's.
var ErrAgentNameRequired = errors.New("mine: markdown transcripts need --agent-name to tell the agent's messages from the human's")
