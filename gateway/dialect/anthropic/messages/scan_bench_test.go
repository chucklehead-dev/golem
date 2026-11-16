package messages

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

// agentRequest is a coding agent's turn: a long system prompt, tool
// definitions and history (about 200 KB).
var agentRequest = func() []byte {
	var b bytes.Buffer
	b.WriteString(`{"model":"claude","max_tokens":32000,"stream":true,"system":[{"type":"text","text":`)
	fmt.Fprintf(&b, "%q", strings.Repeat("You are an agent for software engineering. Follow the instructions. ", 400))
	b.WriteString(`,"cache_control":{"type":"ephemeral"}}],"tools":[`)
	for i := range 40 {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"name":"tool_%d","description":%q,"input_schema":{"type":"object","properties":{"path":{"type":"string"},"content":{"type":"string"}},"required":["path"]}}`, i, strings.Repeat("Reads or writes a file. ", 20))
	}
	b.WriteString(`],"messages":[`)
	for i := range 120 {
		if i > 0 {
			b.WriteByte(',')
		}
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		fmt.Fprintf(&b, `{"role":%q,"content":[{"type":"text","text":%q}]}`, role, strings.Repeat("func main() { fmt.Println(\"hello\") }\n", 30))
	}
	b.WriteString(`]}`)
	return b.Bytes()
}()

// BenchmarkScan is the pass-through scan of a small request and of an
// agent's.
func BenchmarkScan(b *testing.B) {
	small := []byte(`{"model":"claude","max_tokens":1024,"stream":true,"system":"You are a helpful assistant.","messages":[{"role":"user","content":"` + strings.Repeat("Summarise the attached text. ", 30) + `"}]}`)
	for _, tc := range []struct {
		name string
		body []byte
	}{{"small", small}, {"agent", agentRequest}} {
		b.Run(tc.name, func(b *testing.B) {
			var r Request
			b.SetBytes(int64(len(tc.body)))
			b.ReportAllocs()
			for b.Loop() {
				if err := r.Scan(tc.body); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
