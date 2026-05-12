package kiro

// kiroCachePointType is the singleton discriminator Kiro's CodeWhisperer
// upstream recognises on a cachePoint marker. It mirrors the role of
// Anthropic's `cache_control: {"type":"ephemeral"}` flag — both tell the
// upstream "everything up to this point may be reused from the prefix
// cache, charged at the cheaper cacheReadInputTokens rate".
const kiroCachePointType = "default"

// addKiroCachePoints injects cachePoint markers into a freshly built
// Kiro payload so the CodeWhisperer upstream can re-use cached prefix
// tokens across multi-turn conversations.
//
// Layout mirrors the Anthropic gateway's combined
// addMessageCacheBreakpoints + applyToolsLastCacheBreakpoint strategy
// (see internal/service/gateway_messages_cache.go and
// internal/service/gateway_tool_rewrite.go), translated to Kiro's payload
// shape:
//
//  1. currentMessage.userInputMessage.cachePoint
//     ── equivalent to messages[-1] cache_control.
//
//  2. history's last user turn .cachePoint
//     ── equivalent to "second-to-last user turn" cache_control in the
//     full conversation (currentMessage being the last). Only inserted
//     when total turns (history + current) >= 4, mirroring Parrot's
//     `len(messages) >= 4` gate.
//
//  3. history's first user turn .cachePoint
//     ── equivalent to system-prompt-block cache_control. BuildKiroPayload
//     merges the Anthropic system prompt into the first user message in
//     history (or into currentMessage when history is empty), so the
//     longest stable cached prefix is anchored here.
//
//  4. tools[] tail-appended {"cachePoint": {"type":"default"}} element
//     ── equivalent to tools[-1].cache_control. Kiro represents tool
//     cache markers as separate entries in the tools array rather than
//     a field on toolSpecification, see KiroToolWrapper in
//     Kiro-account-manager/src/main/proxy/types.ts.
//
// The Kiro upstream reports cache utilisation via
// tokenUsage.cacheReadInputTokens / cacheWriteInputTokens which the
// response transformer surfaces back to clients. Older / non-Claude Kiro
// models that do not honour cachePoint simply ignore the field, so this
// is safe to apply unconditionally.
//
// The function mutates `payload` in place. It is a no-op on nil input or
// when the payload does not match the shape produced by BuildKiroPayload.
func addKiroCachePoints(payload map[string]any) {
	if payload == nil {
		return
	}
	state, _ := payload["conversationState"].(map[string]any)
	if state == nil {
		return
	}

	// (1) currentMessage.userInputMessage.cachePoint — always set.
	// (4) tools[] tail-appended cachePoint — only when tools[] is non-empty.
	if cm, _ := state["currentMessage"].(map[string]any); cm != nil {
		if uim, _ := cm["userInputMessage"].(map[string]any); uim != nil {
			uim["cachePoint"] = newKiroCachePoint()
			if ctx, _ := uim["userInputMessageContext"].(map[string]any); ctx != nil {
				if tools, ok := ctx["tools"].([]any); ok && len(tools) > 0 {
					ctx["tools"] = append(tools, map[string]any{
						"cachePoint": newKiroCachePoint(),
					})
				}
			}
		}
	}

	history, _ := state["history"].([]any)
	if len(history) == 0 {
		return
	}

	// (3) Mark the first user turn in history. BuildKiroPayload's
	// normaliseMessages guarantees the first message is a user turn, but
	// we still iterate defensively in case future refactors loosen that
	// invariant.
	firstUserIdx := -1
	for i, msg := range history {
		uim := userInputMessageOf(msg)
		if uim == nil {
			continue
		}
		uim["cachePoint"] = newKiroCachePoint()
		firstUserIdx = i
		break
	}

	// (2) Total turns >= 4 → also mark the history's last user turn
	// (i.e. the "second-to-last" user turn from the perspective of the
	// full conversation; currentMessage is always the last user turn).
	// Skip when the only user in history is the first one (firstUserIdx)
	// because the cachePoint is already set there.
	totalTurns := len(history) + 1 // include currentMessage
	if totalTurns < 4 {
		return
	}
	for i := len(history) - 1; i > firstUserIdx; i-- {
		uim := userInputMessageOf(history[i])
		if uim == nil {
			continue
		}
		uim["cachePoint"] = newKiroCachePoint()
		return
	}
}

// userInputMessageOf returns the `userInputMessage` sub-map of a history
// entry, or nil when the entry is not a user turn (i.e. it is an
// assistantResponseMessage or malformed).
func userInputMessageOf(entry any) map[string]any {
	m, ok := entry.(map[string]any)
	if !ok {
		return nil
	}
	uim, _ := m["userInputMessage"].(map[string]any)
	return uim
}

// newKiroCachePoint returns a fresh `{"type":"default"}` map. Each call
// returns a new map so cache points injected at different positions never
// share the same backing object (which would otherwise let a single
// mutation propagate to every breakpoint).
func newKiroCachePoint() map[string]any {
	return map[string]any{"type": kiroCachePointType}
}
