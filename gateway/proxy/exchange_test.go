package proxy

import (
	"bytes"
	"errors"
	"net/http/httptest"
	"testing"
)

// TestKeptNotSpliced: a whole response that outgrows the kept copy keeps
// none of what follows, though a later read would fit: the head and a tail
// together could read as a body with another usage. Its usage is unread,
// and the call is estimated.
func TestKeptNotSpliced(t *testing.T) {
	ex := getExchange()
	defer ex.release()
	ex.startWrites(httptest.NewRecorder(), 0)
	ex.kept, ex.overflow = ex.kept[:0], false
	head := append([]byte(`{"pad":"`), bytes.Repeat([]byte("x"), maxRetained-100)...)
	for _, b := range [][]byte{head, bytes.Repeat([]byte("x"), 200), []byte(`","usage":{"input_tokens":1,"output_tokens":1}}`)} {
		ex.forward(b, false, false)
	}
	if !ex.overflow || !bytes.Equal(ex.kept, head) {
		t.Fatalf("kept %d bytes (overflow %v): want the head alone", len(ex.kept), ex.overflow)
	}
	ex.readWhole(AnthropicMessages)
	if ex.msgUsage.Final || !errors.Is(ex.msgUsage.Err, errResponseUnread) {
		t.Fatalf("usage %+v: a response larger than its copy has its usage unread", ex.msgUsage)
	}
}
