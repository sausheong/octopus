package anthropicio

import (
	"reflect"

	"github.com/sausheong/harness/llm"
)

// Harness added thinking blocks after v0.3.2. Reflection keeps llmrouter
// source-compatible with that last tag while fully supporting the newer
// workspace API; these helpers can become direct field access after the next
// harness release is tagged.
const eventThinkingBlock llm.EventType = 6

func appendThinkingBlock(msg *llm.Message, thinking, signature string) bool {
	v := reflect.ValueOf(msg).Elem().FieldByName("ThinkingBlocks")
	if !v.IsValid() || !v.CanSet() || v.Kind() != reflect.Slice {
		return false
	}
	elem := reflect.New(v.Type().Elem()).Elem()
	if f := elem.FieldByName("Thinking"); f.IsValid() && f.CanSet() {
		f.SetString(thinking)
	}
	if f := elem.FieldByName("Signature"); f.IsValid() && f.CanSet() {
		f.SetString(signature)
	}
	v.Set(reflect.Append(v, elem))
	return true
}

func eventThinking(ev llm.ChatEvent) (thinking, signature string, ok bool) {
	v := reflect.ValueOf(ev).FieldByName("ThinkingBlock")
	if !v.IsValid() || v.Kind() != reflect.Pointer || v.IsNil() {
		return "", "", false
	}
	v = v.Elem()
	t := v.FieldByName("Thinking")
	if !t.IsValid() || t.Kind() != reflect.String {
		return "", "", false
	}
	s := v.FieldByName("Signature")
	if s.IsValid() && s.Kind() == reflect.String {
		signature = s.String()
	}
	return t.String(), signature, true
}
