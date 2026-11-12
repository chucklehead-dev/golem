package proxy

import "encoding/json/jsontext"

func appendQuoted(b []byte, s string) []byte {
	b, _ = jsontext.AppendQuote(b, s)
	return b
}
