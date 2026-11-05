package securityaudit

import (
	"bytes"
	"cmp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
	"unsafe"
)

const (
	segUser      uint8 = 1 << iota // user text (the legacy isUserSegment)
	segAssistant                   // assistant or model output (isAssistantOutputSegment)
	segDropped                     // a later equal text, removed by dedupe
)

func segmentKind(user bool, role string) uint8 {
	var kind uint8
	if user || role == "user" {
		kind |= segUser
	}
	if role == "assistant" || role == "model" {
		kind |= segAssistant
	}
	return kind
}

// promptSegments holds the extracted texts in order, trimmed, blank ones
// dropped (the legacy normalizedPromptSegments): their bytes back to back in
// one arena, and per segment its end offset and kind (5 bytes). The
// extractors run twice over the body: a count pass sizes the arena and the
// index exactly, and the fill pass stores. So the guard allocates the kept
// text once plus 5 bytes per segment, whatever the number of JSON values.
type promptSegments struct {
	fill    bool
	emitted int // segments emitted, blank ones included (the legacy len(segments))
	count   int // count pass: segments kept
	size    int // count pass: arena bytes
	arena   []byte
	ends    []uint32
	kinds   []uint8
	scratch []byte // decoded text of a string with escapes, reused

	// Sorted member walks (media prompts, System One): the fill pass keeps
	// the member offsets (from walkRoot) of the objects on the walk's path;
	// the count pass finds how many that can be, so they are sized once.
	walkRoot     jsonRaw
	members      []uint32
	memberNeed   int
	memberLive   int
	nameA, nameB []byte // decoded names with escapes, for sorting
}

// startFill switches from the count pass to the fill pass.
func (s *promptSegments) startFill() {
	s.fill, s.emitted = true, 0
	s.arena = make([]byte, 0, s.size)
	s.ends = make([]uint32, 0, s.count)
	s.kinds = make([]uint8, 0, s.count)
	if s.memberNeed > 0 {
		s.members = make([]uint32, 0, s.memberNeed)
	}
}

// add emits the text of the string literal lit, a blank one included (it
// counts in emitted, and is dropped).
func (s *promptSegments) add(lit jsonRaw, kind uint8) {
	s.emitted++
	s.keep(strings.TrimSpace(s.view(lit)), kind)
}

// addNonBlank emits v when it is a string with non-space text (the legacy
// stringValue(v) != "").
func (s *promptSegments) addNonBlank(v jsonRaw, kind uint8) {
	if !v.isString() {
		return
	}
	if text := strings.TrimSpace(s.view(v)); text != "" {
		s.emitted++
		s.keep(text, kind)
	}
}

// keep stores a trimmed text (a view: it is copied into the arena).
func (s *promptSegments) keep(text string, kind uint8) {
	if text == "" {
		return
	}
	if !s.fill {
		s.count++
		s.size += len(text)
		return
	}
	s.arena = append(s.arena, text...)
	s.ends = append(s.ends, uint32(len(s.arena)))
	s.kinds = append(s.kinds, kind)
}

// trimmed is the legacy stringValue(v): strings.TrimSpace of v's text when v
// is a string, else "". A view (see view): never keep it.
func (s *promptSegments) trimmed(v jsonRaw) string {
	if !v.isString() {
		return ""
	}
	return strings.TrimSpace(s.view(v))
}

// enum is strings.ToLower(stringValue(v)) when that is one of names, "" when
// it is empty, and "?" for any other text, without allocating.
func (s *promptSegments) enum(v jsonRaw, names ...string) string {
	text := s.trimmed(v)
	if text == "" {
		return ""
	}
	for _, name := range names {
		if lowerEquals(text, name) {
			return name
		}
	}
	return "?"
}

// lowerEquals reports strings.ToLower(text) == lower for an ASCII lower:
// unicode.ToLower rune by rune, as strings.ToLower maps (U+0130 and U+212A
// lowercase to ASCII letters).
func lowerEquals(text, lower string) bool {
	j := 0
	for _, r := range text {
		if j == len(lower) || unicode.ToLower(r) != rune(lower[j]) {
			return false
		}
		j++
	}
	return j == len(lower)
}

func (s *promptSegments) len() int { return len(s.ends) }

// text is segment i, a view of the arena (never written after the fill).
func (s *promptSegments) text(i int) string {
	start := uint32(0)
	if i > 0 {
		start = s.ends[i-1]
	}
	if start == s.ends[i] {
		return ""
	}
	return unsafe.String(&s.arena[start], int(s.ends[i]-start))
}

func (s *promptSegments) user(i int) bool      { return s.kinds[i]&segUser != 0 }
func (s *promptSegments) assistant(i int) bool { return s.kinds[i]&segAssistant != 0 }

// dedupe keeps the first segment of each text (media prompts), in place.
func (s *promptSegments) dedupe() {
	n := s.len()
	if n < 2 {
		return
	}
	order := make([]uint32, n)
	for i := range order {
		order[i] = uint32(i)
	}
	slices.SortFunc(order, func(a, b uint32) int {
		if c := strings.Compare(s.text(int(a)), s.text(int(b))); c != 0 {
			return c
		}
		return cmp.Compare(a, b)
	})
	for i := 1; i < n; i++ {
		if s.text(int(order[i])) == s.text(int(order[i-1])) {
			s.kinds[order[i]] |= segDropped
		}
	}
	w, kept, prev := 0, 0, uint32(0)
	for i := 0; i < n; i++ {
		start, end := prev, s.ends[i]
		prev = end
		if s.kinds[i]&segDropped != 0 {
			continue
		}
		w += copy(s.arena[w:], s.arena[start:end])
		s.ends[kept], s.kinds[kept] = uint32(w), s.kinds[i]
		kept++
	}
	s.arena, s.ends, s.kinds = s.arena[:w], s.ends[:kept], s.kinds[:kept]
}

// segmentSpan is the segments [from, to) in order.
type segmentSpan struct{ from, to int }

// latestUserFirst orders the last user segment first, then the others in
// order (the legacy normalizeSegmentsLatestUserFirst).
func (s *promptSegments) latestUserFirst() []segmentSpan {
	n := s.len()
	if n == 0 {
		return nil
	}
	p := n - 1
	for i := n - 1; i >= 0; i-- {
		if s.user(i) {
			p = i
			break
		}
	}
	return []segmentSpan{{p, p + 1}, {0, p}, {p + 1, n}}
}

// latestTurn is the latest user turn (all its parts, the priority) and the
// nearest preceding run of assistant output (the legacy
// blockingSegmentsLatestUserAndPreviousOutput). Without user text it is
// latestUserFirst with priority 1.
func (s *promptSegments) latestTurn() (spans []segmentSpan, priority int) {
	start := -1
	for i := s.len() - 1; i >= 0; i-- {
		if s.user(i) {
			start = i
			break
		}
	}
	if start < 0 {
		return s.latestUserFirst(), 1
	}
	for start > 0 && s.user(start-1) {
		start--
	}
	end := start
	for end < s.len() && s.user(end) {
		end++
	}
	spans = []segmentSpan{{start, end}}
	for i := start - 1; i >= 0; i-- {
		if !s.assistant(i) {
			continue
		}
		from := i
		for from > 0 && s.assistant(from-1) {
			from--
		}
		spans = append(spans, segmentSpan{from, i + 1})
		break
	}
	return spans, end - start
}

// view is the decoded text of the string literal lit: a view of the body, or
// of the scratch buffer when lit has escapes or invalid UTF-8 (valid until
// the next view or add). Never keep it.
func (s *promptSegments) view(lit jsonRaw) string {
	inner := []byte(lit[1 : len(lit)-1])
	if bytes.IndexByte(inner, '\\') >= 0 || !utf8.Valid(inner) {
		s.scratch = appendJSONUnquote(s.scratch[:0], lit)
		inner = s.scratch
	}
	return unsafe.String(unsafe.SliceData(inner), len(inner))
}

// eachMemberSorted calls f with obj's members in sorted name order, the last
// of equal names only (the members a map keeps, in sortedJSONKeys order).
// The count pass calls f with every member in document order instead: it
// only counts, and shadowed members make its counts an upper bound.
func (s *promptSegments) eachMemberSorted(obj jsonRaw, f func(name, value jsonRaw)) {
	if !s.fill {
		n := 0
		for it := obj.members(); ; n++ {
			if _, _, ok := it.nextMember(); !ok {
				break
			}
		}
		s.memberLive += n
		s.memberNeed = max(s.memberNeed, s.memberLive)
		for it := obj.members(); ; {
			name, value, ok := it.nextMember()
			if !ok {
				break
			}
			f(name, value)
		}
		s.memberLive -= n
		return
	}
	from := len(s.members)
	for it := obj.members(); ; {
		name, _, ok := it.nextMember()
		if !ok {
			break
		}
		s.members = append(s.members, uint32(cap(s.walkRoot)-cap(name)))
	}
	to := len(s.members)
	slices.SortStableFunc(s.members[from:to], s.compareNames)
	for i := from; i < to; i++ {
		if i+1 < to && s.compareNames(s.members[i], s.members[i+1]) == 0 {
			continue
		}
		f(s.memberAt(s.members[i]))
	}
	s.members = s.members[:from]
}

func (s *promptSegments) compareNames(a, b uint32) int {
	nameA, _ := s.memberAt(a)
	nameB, _ := s.memberAt(b)
	return strings.Compare(nameView(nameA, &s.nameA), nameView(nameB, &s.nameB))
}

// nameView is the decoded name: a view of the body, or of *buf.
func nameView(name jsonRaw, buf *[]byte) string {
	inner := []byte(name[1 : len(name)-1])
	if bytes.IndexByte(inner, '\\') >= 0 || !utf8.Valid(inner) {
		*buf = appendJSONUnquote((*buf)[:0], name)
		inner = *buf
	}
	return unsafe.String(unsafe.SliceData(inner), len(inner))
}

// memberAt is the member whose name literal starts at offset off of walkRoot.
func (s *promptSegments) memberAt(off uint32) (name, value jsonRaw) {
	b := s.walkRoot
	end := jsonStringEnd(b, int(off))
	i := jsonSkipSpace(b, jsonSkipSpace(b, end)+1)
	return b[off:end], b[i:jsonValueEnd(b, i)]
}
