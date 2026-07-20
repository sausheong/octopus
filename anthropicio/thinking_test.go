package anthropicio

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/sausheong/harness/llm"
)

func thinkingEvent(t *testing.T, thinking, signature string) llm.ChatEvent {
	t.Helper()
	return llm.ChatEvent{Type: llm.EventThinkingBlock, ThinkingBlock: &llm.ThinkingBlock{
		Thinking: thinking, Signature: signature,
	}}
}

func TestThinkingBlockRoundTripsThroughAnthropicEncoders(t *testing.T) {
	ev := thinkingEvent(t, "private reasoning", "sig_123")

	w := &bufWriter{}
	if err := EncodeSSE(w, "m", feed(ev, llm.ChatEvent{Type: llm.EventDone})); err != nil {
		t.Fatalf("EncodeSSE: %v", err)
	}
	for _, want := range []string{`"type":"thinking"`, `"thinking":"private reasoning"`, `"signature":"sig_123"`} {
		if !strings.Contains(w.b.String(), want) {
			t.Errorf("SSE missing %s: %s", want, w.b.String())
		}
	}

	out, err := CollectMessage("m", feed(ev, llm.ChatEvent{Type: llm.EventDone}))
	if err != nil {
		t.Fatalf("CollectMessage: %v", err)
	}
	for _, want := range []string{`"type":"thinking"`, `"thinking":"private reasoning"`, `"signature":"sig_123"`} {
		if !strings.Contains(string(out), want) {
			t.Errorf("buffered response missing %s: %s", want, out)
		}
	}
}

func TestDecodePreservesThinkingBlock(t *testing.T) {
	dr, err := Decode([]byte(`{"max_tokens":1,"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"why","signature":"sig"},{"type":"text","text":"answer"}]}]}`))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	blocks := dr.Chat.Messages[0].ThinkingBlocks
	if len(blocks) != 1 {
		t.Fatalf("thinking blocks=%d", len(blocks))
	}
	got, _ := json.Marshal(blocks)
	if !strings.Contains(string(got), `"thinking":"why"`) || !strings.Contains(string(got), `"signature":"sig"`) {
		t.Fatalf("thinking block not preserved: %s", got)
	}
}
