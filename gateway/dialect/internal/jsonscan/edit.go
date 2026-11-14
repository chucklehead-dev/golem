package jsonscan

// Edit replaces body[Span.Start:Span.End] with With; an empty span inserts.
type Edit struct {
	Span Span
	With []byte
}

// Apply appends body with edits applied to dst. Edits must not overlap.
func Apply(dst, body []byte, edits ...Edit) []byte {
	// An insertion sort: a request has a few edits, and slices.SortFunc's
	// comparison made the compiler leak them, so a caller's stack buffers
	// (a quoted model name, the inserted members) were heap allocations.
	for i := 1; i < len(edits); i++ {
		for j := i; j > 0 && edits[j].Span.Start < edits[j-1].Span.Start; j-- {
			edits[j], edits[j-1] = edits[j-1], edits[j]
		}
	}
	dst = dst[:0]
	at := 0
	for _, e := range edits {
		dst = append(dst, body[at:e.Span.Start]...)
		dst = append(dst, e.With...)
		at = e.Span.End
	}
	return append(dst, body[at:]...)
}

// Int reads a JSON integer value.
func Int(v []byte) (int64, bool) {
	if len(v) == 0 {
		return 0, false
	}
	var n int64
	neg := v[0] == '-'
	if neg {
		v = v[1:]
	}
	if len(v) == 0 || len(v) > 18 {
		return 0, false
	}
	for _, c := range v {
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int64(c-'0')
	}
	if neg {
		n = -n
	}
	return n, true
}
