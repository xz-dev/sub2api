package securityaudit

import (
	"encoding/hex"
	"errors"
	"math"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

var (
	ErrNoPromptText = errors.New("prompt audit request contains no user text")

	bearerPattern = regexp.MustCompile(`(?i)\bBearer\s+[A-Za-z0-9._~+\-/]+=*`)
	apiKeyPattern = regexp.MustCompile(`(?i)\b(sk|rk|pk|api[_-]?key|token|secret|password)[-_:=\s]+[A-Za-z0-9._~+\-/]{8,}`)
	canaryPattern = regexp.MustCompile(`(?i)([A-Z]+_CANARY_)[A-Za-z0-9_-]+`)
	emailPattern  = regexp.MustCompile(`(?i)\b[A-Z0-9._%+\-]+@[A-Z0-9.\-]+\.[A-Z]{2,}\b`)
	phonePattern  = regexp.MustCompile(`(?:\+?\d[\d\s().-]{8,}\d)`)
)

const promptAuditPrioritySeparator = "\x00SUB2API_PROMPT_AUDIT_PRIORITY_END\x00"

func ExtractPromptSnapshot(req Request) (PromptSnapshot, error) {
	return extractPromptSnapshot(req, false)
}

// ExtractBlockingPromptSnapshot builds the narrow, low-latency blocking input
// when configured. Asynchronous auditing always uses ExtractPromptSnapshot so
// the complete client-controlled transcript is retained for review.
func ExtractBlockingPromptSnapshot(req Request, latestTurnOnly bool) (PromptSnapshot, error) {
	return extractPromptSnapshot(req, latestTurnOnly)
}

func extractPromptSnapshot(req Request, latestTurnOnly bool) (PromptSnapshot, error) {
	// The body is read in place (prompt_json.go); segment offsets are 32-bit.
	if uint64(len(req.Body)) > math.MaxUint32 || !validJSONDocument(req.Body) {
		return PromptSnapshot{}, errors.New("prompt audit request JSON is invalid")
	}
	root := jsonDocument(req.Body)
	segs := &promptSegments{}
	protocolSegments(segs, req.Protocol, root) // count pass: sizes only
	segs.startFill()
	if protocolSegments(segs, req.Protocol, root) {
		segs.dedupe()
	}
	spans, priority := segs.latestUserFirst(), 1
	if latestTurnOnly {
		spans, priority = segs.latestTurn()
	}
	segments := promptPieces{segs: segs, spans: spans, joiner: "\n\n"}
	if segments.count() == 0 {
		return PromptSnapshot{}, ErrNoPromptText
	}
	// The metadata text (every segment joined by "\n\n") is never built:
	// its hash, length, preview and stored prompt are computed from the
	// segments, so the copies of the text are the segment arena and the scan
	// text the guard reads (the arena itself for a single segment). Review
	// a4a7d843 blocker 2.
	scanText := buildPrioritizedScanText(segments, priority)
	metadata := segments
	digest := metadata.sha256()
	messageCount := segments.count() - (priority - 1) // the priority parts are one message
	stage := strings.TrimSpace(req.Stage)
	if stage == "" {
		stage = "http"
	}
	return PromptSnapshot{
		RequestID: req.RequestID, UserID: req.UserID, UsernameSnapshot: req.Username,
		UserEmailSnapshot: req.UserEmail, APIKeyID: req.APIKeyID, APIKeyNameSnapshot: req.APIKeyName,
		GroupID: cloneInt64Ptr(req.GroupID), GroupName: req.GroupName, Provider: req.Provider,
		Endpoint: req.Endpoint, Protocol: req.Protocol, Model: req.Model,
		PromptHash: hex.EncodeToString(digest[:]), RedactedPreview: BuildPromptPreview(metadata.head(promptPreviewInputBytes), DefaultPromptPreviewMaxRunes),
		FullPrompt:   metadata.fullPrompt(DefaultFullPromptMaxRunes),
		PromptLength: metadata.runeCount(), MessageCount: messageCount, Stage: stage,
		ScanText: scanText,
	}, nil
}

// DefaultPromptPreviewMaxRunes caps how much sanitized prompt text may be
// considered before BuildPromptPreview withholds the majority for storage/UI.
const DefaultPromptPreviewMaxRunes = 96

// promptPreviewInputBytes bounds the head of the prompt that the preview's
// redaction regexes read. The preview keeps at most the first
// DefaultPromptPreviewMaxRunes runes of the redacted text, so it reads a head
// of 16 KiB, not the whole prompt (review a4a7d843 blocker 2: five regex
// passes, each copying the whole text twice). The guard's scan text is not
// affected. The preview differs from a whole-text preview only when the
// redactions shrink the head below DefaultPromptPreviewMaxRunes+1 runes (a
// secret or separator run of about 16 KiB near the start); FullPrompt stores
// the unredacted text anyway.
const promptPreviewInputBytes = 16 << 10

// DefaultFullPromptMaxRunes caps how much unredacted prompt text is persisted
// on an audit event for admin review. It is deliberately generous so realistic
// prompts are kept intact while bounding per-row storage.
const DefaultFullPromptMaxRunes = 65536

// isMediaPromptKey reports whether key, trimmed, lowercased and without "_"
// and "-", names a media prompt (all of them are short ASCII), without
// allocating.
func isMediaPromptKey(key string) bool {
	var buf [32]byte
	n := 0
	for _, r := range strings.TrimSpace(key) {
		r = unicode.ToLower(r)
		if r == '_' || r == '-' {
			continue
		}
		if r >= utf8.RuneSelf || n == len(buf) {
			return false
		}
		buf[n] = byte(r)
		n++
	}
	return isMediaPromptKeyName(string(buf[:n]))
}

func isMediaPromptKeyName(normalized string) bool {
	switch normalized {
	case "prompt", "inputprompt", "textprompt", "description", "query", "lyrics", "negativeprompt",
		"positiveprompt", "gptdescriptionprompt", "prompten", "finalprompt", "finalzhprompt",
		"origprompt", "actualprompt", "imageprompt", "input":
		return true
	default:
		return false
	}
}

// looksLikeMediaPayload reports a data URL, an http(s) URL or a base64 run of
// at least 256 bytes. The prefixes are compared ASCII case-insensitively in
// place (no non-ASCII rune lowercases to these prefixes), not on a lowercased
// copy of the whole text.
func looksLikeMediaPayload(value string) bool {
	trimmed := strings.TrimSpace(value)
	for _, prefix := range [...]string{"data:image/", "data:video/", "http://", "https://"} {
		if len(trimmed) >= len(prefix) && asciiEqualFold(trimmed[:len(prefix)], prefix) {
			return true
		}
	}
	if len(trimmed) >= 256 {
		for _, r := range trimmed {
			alphaNumeric := (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
			if !alphaNumeric && r != '+' && r != '/' && r != '=' {
				return false
			}
		}
		return true
	}
	return false
}

// asciiEqualFold compares s with the lowercase ASCII lower, folding only
// ASCII letters of s.
func asciiEqualFold(s, lower string) bool {
	for i := 0; i < len(lower); i++ {
		c := s[i]
		if 'A' <= c && c <= 'Z' {
			c += 'a' - 'A'
		}
		if c != lower[i] {
			return false
		}
	}
	return true
}

// buildPrioritizedScanText joins the first priority segments, the priority
// separator and the remaining segments, with "\n\n" between segments, in one
// allocation of the exact size. A single segment is returned without a copy
// when it is the whole arena, else copied so the scan text does not keep the
// other segments' text alive.
func buildPrioritizedScanText(segments promptPieces, priority int) string {
	count := segments.count()
	if count == 1 {
		text := segments.first()
		if segments.segs != nil && len(text) != len(segments.segs.arena) {
			text = strings.Clone(text)
		}
		return text
	}
	priority = max(1, min(priority, count))
	size := 0
	segments.each(func(s string) bool {
		size += len(s)
		return true
	})
	if priority < count {
		size += len(promptAuditPrioritySeparator) - len(segments.joiner)
	}
	var b strings.Builder
	b.Grow(size)
	index := 0
	segments.eachPiece(func(s string) bool {
		switch {
		case index == 0:
		case index == priority:
			b.WriteString(promptAuditPrioritySeparator)
		default:
			b.WriteString(segments.joiner)
		}
		b.WriteString(s)
		index++
		return true
	})
	return b.String()
}

func RedactPreview(value string, maxRunes int) string {
	value = bearerPattern.ReplaceAllString(value, "Bearer ***")
	value = apiKeyPattern.ReplaceAllStringFunc(value, func(match string) string {
		if index := strings.IndexAny(match, ":= \t"); index >= 0 {
			return match[:index+1] + "***"
		}
		return "***"
	})
	value = canaryPattern.ReplaceAllString(value, "${1}***")
	value = emailPattern.ReplaceAllString(value, "***@***")
	value = phonePattern.ReplaceAllString(value, "***PHONE***")
	return TrimRunes(value, maxRunes)
}

// BuildPromptPreview stores only a short, non-recoverable head of sanitized
// input. Ordinary confidential prompts must not land nearly intact in PostgreSQL
// or the admin UI merely because no secret regex matched.
func BuildPromptPreview(value string, maxRunes int) string {
	if maxRunes <= 0 {
		maxRunes = DefaultPromptPreviewMaxRunes
	}
	redacted := strings.TrimSpace(RedactPreview(value, maxRunes))
	if redacted == "" {
		return ""
	}
	runes := []rune(redacted)
	hadTruncation := strings.HasSuffix(redacted, "…")
	if hadTruncation && len(runes) > 0 {
		runes = runes[:len(runes)-1]
	}
	if len(runes) == 0 {
		return "***…"
	}
	// Short unlabelled secrets would otherwise leak a recoverable prefix (e.g.
	// 20 runes → 5 visible). Fully withhold anything below the keep threshold.
	const minLengthForPartialPreview = 32
	if len(runes) < minLengthForPartialPreview {
		if hadTruncation {
			return "***…"
		}
		return "***"
	}
	// Keep at most a quarter of the already-truncated text, and never more than
	// 24 runes, so the majority of prompt content is withheld by default.
	keep := len(runes) / 4
	if keep > 24 {
		keep = 24
	}
	preview := string(runes[:keep]) + "***"
	if hadTruncation || keep < len(runes) {
		preview += "…"
	}
	return preview
}

// BuildFullPrompt returns the complete prompt text for audit-event storage and
// admin review, without redaction. NUL bytes are stripped because PostgreSQL
// TEXT rejects them, and the result is capped at maxRunes.
func BuildFullPrompt(value string, maxRunes int) string {
	return promptPieces{texts: []string{value}}.fullPrompt(maxRunes)
}

// FullPromptFromScanText reconstructs the display prompt from the worker scan
// payload. buildPrioritizedScanText inserts exactly one priority separator
// between the prioritized segment and the remainder, so replacing it with the
// metadata joiner yields the original multi-segment text.
func FullPromptFromScanText(scanText string) string {
	pieces := promptPieces{texts: []string{scanText}, splitOn: promptAuditPrioritySeparator, joiner: "\n\n"}
	return pieces.fullPrompt(DefaultFullPromptMaxRunes)
}

// TrimRunes returns value when it has at most limit runes, else its first
// limit runes and "…". It walks value in place: only the result is
// allocated, never a []rune copy of the whole value.
func TrimRunes(value string, limit int) string {
	if limit <= 0 {
		return ""
	}
	count := 0
	for index := range value {
		if count == limit {
			return keepRunes(value[:index]) + "…"
		}
		count++
	}
	return value
}

// keepRunes is head as string([]rune(head)) would return it: invalid UTF-8
// bytes become U+FFFD, as the []rune conversion TrimRunes used to make did.
func keepRunes(head string) string {
	if utf8.ValidString(head) {
		return head
	}
	return string([]rune(head))
}

func cloneInt64Ptr(value *int64) *int64 {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}
