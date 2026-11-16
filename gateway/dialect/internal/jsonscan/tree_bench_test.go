package jsonscan

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

// BenchmarkObjects walks a coding agent's history (120 turns, each a text
// block and a tool call) as the Messages dialect's file-reference check
// does, the tool calls' input skipped.
func BenchmarkObjects(b *testing.B) {
	var v bytes.Buffer
	v.WriteByte('[')
	for i := range 120 {
		if i > 0 {
			v.WriteByte(',')
		}
		fmt.Fprintf(&v, `{"role":"user","content":[{"type":"text","text":%q},{"type":"tool_use","id":"t","name":"read","input":{"path":"a"}}]}`,
			strings.Repeat("func main() { fmt.Println(\"hello\") }\n", 30))
	}
	v.WriteByte(']')
	skip := func(name []byte) bool { return string(name) == "input" }
	n := 0
	visit := func(o *ObjectInfo) error {
		if o.Has("file_id") {
			n++
		}
		return nil
	}
	b.SetBytes(int64(v.Len()))
	b.ReportAllocs()
	for b.Loop() {
		if err := Objects(v.Bytes(), skip, visit); err != nil {
			b.Fatal(err)
		}
	}
}
