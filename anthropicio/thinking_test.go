package anthropicio

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/sausheong/harness/llm"
)

func thinkingEvent(t *testing.T, thinking, signature string) llm.ChatEvent {
	t.Helper()
	ev := llm.ChatEvent{Type: eventThinkingBlock}
	f := reflect.ValueOf(&ev).Elem().FieldByName("ThinkingBlock")
	if !f.IsValid() {
		t.Skip("thinking-block API requires the post-v0.3.2 harness workspace")
	}
	block := reflect.New(f.Type().Elem())
	block.Elem().FieldByName("Thinking").SetString(thinking)
	block.Elem().FieldByName("Signature").SetString(signature)
	f.Set(block)
	return ev
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
	// Skip cleanly against the last tagged harness; the workspace harness has
	// the field and exercises the full inbound continuation path.
	probe := reflect.ValueOf(llm.Message{}).FieldByName("ThinkingBlocks")
	if !probe.IsValid() {
		t.Skip("thinking-block API requires the post-v0.3.2 harness workspace")
	}
	dr, err := Decode([]byte(`{"max_tokens":1,"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"why","signature":"sig"},{"type":"text","text":"answer"}]}]}`))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	blocks := reflect.ValueOf(dr.Chat.Messages[0]).FieldByName("ThinkingBlocks")
	if blocks.Len() != 1 {
		t.Fatalf("thinking blocks=%d", blocks.Len())
	}
	got, _ := json.Marshal(blocks.Interface())
	if !strings.Contains(string(got), `"thinking":"why"`) || !strings.Contains(string(got), `"signature":"sig"`) {
		t.Fatalf("thinking block not preserved: %s", got)
	}
}
