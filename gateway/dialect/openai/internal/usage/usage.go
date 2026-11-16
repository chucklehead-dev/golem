// Package usage reads OpenAI's usage object, which Chat Completions and
// Responses shape alike under different member names.
package usage

import (
	"github.com/chucklehead-dev/golem/gateway/canon"
	"github.com/chucklehead-dev/golem/gateway/dialect/internal/jsonscan"
)

// Names are a dialect's usage members: the input and output counts, and the
// details objects holding the cached input and the reasoning output.
type Names struct{ Input, Output, InputDetails, OutputDetails string }

// Read reads the usage member of a JSON object (a chunk or a whole
// response); seen is false if it has none, or a null one (an ordinary
// chunk). OpenAI's input count includes the cached tokens; canon.Usage.Input
// does not. bad is the dialect's error for a count that is not a
// non-negative integer, or a partial report (a count absent or null), which
// must not suppress estimation.
func Read(body []byte, n Names, bad error) (got canon.Usage, seen bool, err error) {
	input, output := false, false
	err = jsonscan.Object(body, func(name []byte, d *jsonscan.Decoder) error {
		if string(name) != "usage" {
			return d.SkipValue()
		}
		v, _, err := d.ReadValue()
		if err != nil || v.Kind() != '{' {
			return err
		}
		seen = true
		return jsonscan.Object(v, func(name []byte, d *jsonscan.Decoder) error {
			var err error
			switch string(name) {
			case n.Input:
				input, err = count(d, &got.Input, bad)
			case n.Output:
				output, err = count(d, &got.Output, bad)
			case n.InputDetails:
				err = detail(d, "cached_tokens", &got.CacheRead, bad)
			case n.OutputDetails:
				err = detail(d, "reasoning_tokens", &got.Reasoning, bad)
			default:
				err = d.SkipValue()
			}
			return err
		})
	})
	if err == nil && seen && (!input || !output) {
		err = bad
	}
	if err != nil {
		return canon.Usage{}, false, err
	}
	got.Input -= min(got.CacheRead, got.Input)
	return got, seen, nil
}

// count reads a token count into dst: a non-negative integer (present), or
// null (not reported: present false, dst unchanged).
func count(d *jsonscan.Decoder, dst *int64, bad error) (present bool, err error) {
	v, _, err := d.ReadValue()
	if err != nil || v.Kind() == 'n' {
		return false, err
	}
	n, ok := jsonscan.Int(v)
	if !ok || n < 0 {
		return false, bad
	}
	*dst = n
	return true, nil
}

// detail reads one count from a usage details object, if it is one; a null
// count is only not reported.
func detail(d *jsonscan.Decoder, field string, dst *int64, bad error) error {
	v, _, err := d.ReadValue()
	if err != nil || v.Kind() != '{' {
		return err
	}
	return jsonscan.Object(v, func(name []byte, d *jsonscan.Decoder) error {
		if string(name) != field {
			return d.SkipValue()
		}
		_, err := count(d, dst, bad)
		return err
	})
}
