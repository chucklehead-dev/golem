package chat

import "testing"

var chunk = []byte(`{"id":"chatcmpl-AbCdEfGh123","object":"chat.completion.chunk","created":1731456000,"model":"gpt-4o-2024-08-06","system_fingerprint":"fp_abc123","choices":[{"index":0,"delta":{"content":" hello world"},"logprobs":null,"finish_reason":null}],"usage":null}`)

// BenchmarkChunkNullUsage is the cost of an ordinary chunk of a stream golem
// asked usage of: what every chunk but the last pays.
func BenchmarkChunkNullUsage(b *testing.B) {
	var u StreamUsage
	b.ReportAllocs()
	for b.Loop() {
		u.OnEvent(nil, chunk)
		_ = UsageOnly(chunk)
	}
}
