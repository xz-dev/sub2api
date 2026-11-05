package securityaudit

import "strings"

// The protocol extractors read the validated body in place (prompt_json.go)
// and emit each text into s (prompt_segments.go). They follow the legacy
// extractors over json-into-any maps (prompt_snapshot_legacy_test.go) one to
// one: a member that a map would hold is the last one of that name, and
// absent and null members are told apart where the legacy code did.

const segSystem uint8 = 0 // system and developer instructions

// protocolSegments emits the texts of root for protocol. It reports whether
// the texts are media prompts, which keep only the first of equal texts.
func protocolSegments(s *promptSegments, protocol string, root jsonRaw) (dedupe bool) {
	protocol = strings.ToLower(strings.TrimSpace(protocol))
	switch protocol {
	case "openai_chat_completions", "openai_chat", "chat_completions":
		messageSegments(s, root.field("messages"))
	case "anthropic_messages", "claude_messages", "messages":
		var f [2]jsonRaw
		root.fields(f[:], "system", "messages")
		instructionSegments(s, f[0])
		messageSegments(s, f[1])
	case "gemini", "gemini_generate_content":
		geminiRootSegments(s, root)
	case "openai_responses", "responses", "responses_websocket":
		var f [4]jsonRaw
		root.fields(f[:], "type", "input", "response", "instructions")
		if frameType := s.trimmed(f[0]); frameType != "" || protocol == "responses_websocket" {
			switch {
			case frameType != "response.create":
			case f[1] != nil && !f[1].isNull():
				instructionSegments(s, f[3])
				responsesSegments(s, f[1])
			case f[2].isObject():
				var r [2]jsonRaw
				f[2].fields(r[:], "instructions", "input")
				instructionSegments(s, r[0])
				responsesSegments(s, r[1])
			default:
				instructionSegments(s, f[3])
			}
			return false
		}
		instructionSegments(s, f[3])
		responsesSegments(s, f[1])
	case "openai_images", "grok_media", "media", "images":
		mediaPromptSegments(s, root)
		return true
	case "typesafe_systemone":
		systemOneSegments(s, root)
	default:
		// The first protocol shape that yields any segment, blank ones
		// included (the legacy len(segments) > 0); a shape that yields none
		// emitted nothing, so the next one starts clean.
		if messageSegments(s, root.field("messages")); s.emitted > 0 {
			return false
		}
		var f [2]jsonRaw
		root.fields(f[:], "instructions", "input")
		instructionSegments(s, f[0])
		if responsesSegments(s, f[1]); s.emitted > 0 {
			return false
		}
		if geminiRootSegments(s, root); s.emitted > 0 {
			return false
		}
		mediaPromptSegments(s, root)
		return true
	}
	return false
}

// clientRoles are the roles a client may set (isClientInstructionRole);
// chat and Anthropic messages take the first five.
var clientRoles = [...]string{"user", "system", "developer", "assistant", "tool", "model"}

// role is the legacy strings.ToLower(stringValue(v)) for a role: one of
// clientRoles, "" or "?".
func roleOf(s *promptSegments, v jsonRaw) string { return s.enum(v, clientRoles[:]...) }

// messageSegments: chat and Anthropic messages of a client role.
func messageSegments(s *promptSegments, messages jsonRaw) {
	for it := messages.elements(); ; {
		message, ok := it.next()
		if !ok {
			return
		}
		var f [2]jsonRaw
		message.fields(f[:], "role", "content")
		switch role := roleOf(s, f[0]); role {
		case "user", "system", "developer", "assistant", "tool":
			contentSegments(s, f[1], segmentKind(role == "user", role))
		}
	}
}

// contentSegments: a string content as it is, else the text parts of type
// text, input_text or output_text (or no type), else an object's text.
func contentSegments(s *promptSegments, content jsonRaw, kind uint8) {
	switch {
	case content.isString():
		s.add(content, kind)
	case content.isArray():
		for it := content.elements(); ; {
			part, ok := it.next()
			if !ok {
				return
			}
			var f [2]jsonRaw
			part.fields(f[:], "type", "text")
			switch s.enum(f[0], "text", "input_text", "output_text") {
			case "", "text", "input_text", "output_text":
				s.addNonBlank(f[1], kind)
			}
		}
	case content.isObject():
		s.addNonBlank(content.field("text"), kind)
	}
}

// instructionSegments: Responses instructions and the Anthropic system.
func instructionSegments(s *promptSegments, v jsonRaw) {
	if v.isString() {
		s.addNonBlank(v, segSystem)
		return
	}
	contentSegments(s, v, segSystem)
}

// responsesSegments: the Responses input, a string, one item or a list of
// strings and items. An item without a role is user text.
func responsesSegments(s *promptSegments, input jsonRaw) {
	switch {
	case input.isString():
		s.add(input, segUser)
	case input.isArray():
		for it := input.elements(); ; {
			item, ok := it.next()
			if !ok {
				return
			}
			if item.isString() {
				s.add(item, segUser)
				continue
			}
			var f [3]jsonRaw
			item.fields(f[:], "role", "content", "text")
			role := roleOf(s, f[0])
			if role == "?" {
				continue
			}
			kind := segmentKind(role == "" || role == "user", role)
			if f[1] != nil {
				contentSegments(s, f[1], kind)
			} else {
				s.addNonBlank(f[2], kind)
			}
		}
	case input.isObject():
		var f [2]jsonRaw
		input.fields(f[:], "role", "content")
		role := roleOf(s, f[0])
		if role == "?" {
			return
		}
		contentSegments(s, f[1], segmentKind(role == "" || role == "user", role))
	}
}

// geminiRootSegments: system instructions, contents, instances, and the same
// for each of a batch's requests.
func geminiRootSegments(s *promptSegments, root jsonRaw) {
	var f [6]jsonRaw
	root.fields(f[:], "systemInstruction", "system_instruction", "contents", "content", "instances", "requests")
	geminiRequestSegments(s, f[:5])
	for it := f[5].elements(); ; {
		request, ok := it.next()
		if !ok {
			return
		}
		var r [5]jsonRaw
		request.fields(r[:], "systemInstruction", "system_instruction", "contents", "content", "instances")
		geminiRequestSegments(s, r[:])
	}
}

func geminiRequestSegments(s *promptSegments, f []jsonRaw) {
	geminiSystemSegments(s, f[0])
	geminiSystemSegments(s, f[1])
	geminiContentsSegments(s, f[2], false)
	geminiContentsSegments(s, f[3], false)
	for it := f[4].elements(); ; {
		instance, ok := it.next()
		if !ok {
			return
		}
		s.addNonBlank(instance.field("prompt"), segUser)
	}
}

// geminiContentsSegments: the text parts of one content or a list of them,
// of a client role (none is user); system marks them all as system text.
func geminiContentsSegments(s *promptSegments, v jsonRaw, system bool) {
	if v.isObject() {
		geminiContentSegments(s, v, system)
		return
	}
	for it := v.elements(); ; {
		content, ok := it.next()
		if !ok {
			return
		}
		geminiContentSegments(s, content, system)
	}
}

func geminiContentSegments(s *promptSegments, content jsonRaw, system bool) {
	if !content.isObject() {
		return
	}
	var f [2]jsonRaw
	content.fields(f[:], "role", "parts")
	role := roleOf(s, f[0])
	if role == "?" {
		return
	}
	kind := segmentKind(role == "" || role == "user", role)
	if system {
		kind = segSystem
	}
	geminiPartsSegments(s, f[1], kind)
}

func geminiPartsSegments(s *promptSegments, parts jsonRaw, kind uint8) {
	for it := parts.elements(); ; {
		part, ok := it.next()
		if !ok {
			return
		}
		s.addNonBlank(part.field("text"), kind)
	}
}

func geminiSystemSegments(s *promptSegments, v jsonRaw) {
	switch {
	case v.isString():
		s.addNonBlank(v, segSystem)
	case v.isObject():
		if parts := v.field("parts"); parts.isArray() {
			geminiPartsSegments(s, parts, segSystem)
			return
		}
		contentSegments(s, v, segSystem)
	case v.isArray():
		geminiContentsSegments(s, v, true)
	}
}

// mediaPromptSegments: every string under a media prompt key that is not a
// media payload, objects walked in sorted key order (the caller keeps the
// first of equal texts).
func mediaPromptSegments(s *promptSegments, root jsonRaw) {
	if root.isObject() {
		s.walkRoot = root
		mediaWalk(s, root, nil)
	}
}

func mediaWalk(s *promptSegments, v, key jsonRaw) {
	switch {
	case v.isObject():
		s.eachMemberSorted(v, func(name, value jsonRaw) { mediaWalk(s, value, name) })
	case v.isArray():
		for it := v.elements(); ; {
			item, ok := it.next()
			if !ok {
				return
			}
			mediaWalk(s, item, key)
		}
	case v.isString():
		if key == nil || !isMediaPromptKey(s.view(key)) || looksLikeMediaPayload(s.view(v)) {
			return
		}
		s.add(v, segUser)
	}
}

// systemOneSegments: every client-controlled text of a TypeSafe System One
// request: question IDs, every question field except the validated type,
// unknown top-level extension fields, and the evaluated state. Object keys
// are text too (Jev reads the whole JSON), so they are collected with values.
// The state comes last so it is the prioritized segment, and keys are visited
// in sorted order to keep the prompt hash stable across requests.
func systemOneSegments(s *promptSegments, root jsonRaw) {
	if !root.isObject() {
		return
	}
	s.walkRoot = root
	var f [2]jsonRaw
	root.fields(f[:], "questions", "state")
	if !f[0].isObject() {
		stringLeaves(s, f[0])
	}
	s.eachMemberSorted(f[0], func(id, question jsonRaw) {
		s.add(id, segUser)
		if !question.isObject() {
			stringLeaves(s, question)
			return
		}
		s.eachMemberSorted(question, func(field, value jsonRaw) {
			switch {
			case jsonNameIs(field, "type"):
				return
			case jsonNameIs(field, "instructions"), jsonNameIs(field, "criteria"):
			default:
				s.add(field, segUser)
			}
			stringLeaves(s, value)
		})
	})
	s.eachMemberSorted(root, func(field, value jsonRaw) {
		for _, skip := range [...]string{"model", "stream", "state", "questions"} {
			if jsonNameIs(field, skip) {
				return
			}
		}
		s.add(field, segUser)
		stringLeaves(s, value)
	})
	stringLeaves(s, f[1])
}

// stringLeaves: every string of v, and every object key before its value.
func stringLeaves(s *promptSegments, v jsonRaw) {
	switch {
	case v.isString():
		s.add(v, segUser)
	case v.isArray():
		for it := v.elements(); ; {
			item, ok := it.next()
			if !ok {
				return
			}
			stringLeaves(s, item)
		}
	case v.isObject():
		s.eachMemberSorted(v, func(name, value jsonRaw) {
			s.add(name, segUser)
			stringLeaves(s, value)
		})
	}
}
