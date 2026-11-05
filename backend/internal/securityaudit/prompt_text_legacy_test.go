package securityaudit

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"unicode/utf8"
)

// The snapshot code as it was at 21d3427d38, before review a4a7d843 blocker 2
// removed its copies. prompt_text_test.go checks that the in-place code gives
// the same snapshots, scan text and chunks.

func legacyTrimRunes(value string, limit int) string {
	if limit <= 0 {
		return ""
	}
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit]) + "…"
}

func legacySplitRunes(value string, limit int) []string {
	if limit <= 0 {
		return nil
	}
	segments := strings.Split(value, promptAuditPrioritySeparator)
	chunks := make([]string, 0, len(segments))
	for _, segment := range segments {
		runes := []rune(segment)
		for start := 0; start < len(runes); start += limit {
			end := min(start+limit, len(runes))
			chunks = append(chunks, string(runes[start:end]))
		}
	}
	return chunks
}

func legacyBuildFullPrompt(value string, maxRunes int) string {
	if maxRunes <= 0 {
		maxRunes = DefaultFullPromptMaxRunes
	}
	value = strings.ReplaceAll(value, "\x00", "")
	return legacyTrimRunes(strings.TrimSpace(value), maxRunes)
}

func legacyRedactPreview(value string, maxRunes int) string {
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
	return legacyTrimRunes(value, maxRunes)
}

func legacyBuildPromptPreview(value string, maxRunes int) string {
	if maxRunes <= 0 {
		maxRunes = DefaultPromptPreviewMaxRunes
	}
	redacted := strings.TrimSpace(legacyRedactPreview(value, maxRunes))
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
	if len(runes) < 32 {
		if hadTruncation {
			return "***…"
		}
		return "***"
	}
	keep := min(len(runes)/4, 24)
	preview := string(runes[:keep]) + "***"
	if hadTruncation || keep < len(runes) {
		preview += "…"
	}
	return preview
}

func legacyBlockingSegments(values []promptSegment) []string {
	normalized := normalizedPromptSegments(values)
	latestUserStart := latestUserSegmentStart(normalized)
	if latestUserStart < 0 {
		return normalizeSegmentsLatestUserFirst(values)
	}
	latestUserEnd := latestUserStart
	for latestUserEnd < len(normalized) && isUserSegment(normalized[latestUserEnd]) {
		latestUserEnd++
	}
	currentUserText := make([]string, 0, latestUserEnd-latestUserStart)
	for _, segment := range normalized[latestUserStart:latestUserEnd] {
		currentUserText = append(currentUserText, segment.text)
	}
	selected := []promptSegment{{text: strings.Join(currentUserText, "\n\n"), user: true, role: "user"}}
	for index := latestUserStart - 1; index >= 0; index-- {
		if !isAssistantOutputSegment(normalized[index]) {
			continue
		}
		start := index
		for start > 0 && isAssistantOutputSegment(normalized[start-1]) {
			start--
		}
		selected = append(selected, normalized[start:index+1]...)
		break
	}
	return promptSegmentTexts(selected)
}

func legacyExtractPromptSnapshot(req Request, latestTurnOnly bool) (PromptSnapshot, error) {
	var document any
	if err := json.Unmarshal(req.Body, &document); err != nil {
		return PromptSnapshot{}, err
	}
	extracted := extractProtocolSegments(req.Protocol, document)
	segments := normalizeSegmentsLatestUserFirst(extracted)
	if latestTurnOnly {
		segments = legacyBlockingSegments(extracted)
	}
	if len(segments) == 0 {
		return PromptSnapshot{}, ErrNoPromptText
	}
	metadataText := strings.Join(segments, "\n\n")
	scanText := metadataText
	if len(segments) > 1 {
		scanText = segments[0] + promptAuditPrioritySeparator + strings.Join(segments[1:], "\n\n")
	}
	digest := sha256.Sum256([]byte(metadataText))
	stage := strings.TrimSpace(req.Stage)
	if stage == "" {
		stage = "http"
	}
	return PromptSnapshot{
		RequestID: req.RequestID, Protocol: req.Protocol, Model: req.Model,
		PromptHash: hex.EncodeToString(digest[:]), RedactedPreview: legacyBuildPromptPreview(metadataText, DefaultPromptPreviewMaxRunes),
		FullPrompt:   legacyBuildFullPrompt(metadataText, DefaultFullPromptMaxRunes),
		PromptLength: utf8.RuneCountInString(metadataText), MessageCount: len(segments), Stage: stage,
		ScanText: scanText,
	}, nil
}

func legacyFullPromptFromScanText(scanText string) string {
	return legacyBuildFullPrompt(strings.ReplaceAll(scanText, promptAuditPrioritySeparator, "\n\n"), DefaultFullPromptMaxRunes)
}
