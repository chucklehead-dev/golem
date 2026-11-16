package responses

import (
	"encoding/json"
	"testing"

	"github.com/chucklehead-dev/golem/gateway/canon"
)

func TestScan(t *testing.T) {
	for _, c := range []struct {
		name, body string
		check      func(*Request) bool
	}{
		{"routing", `{"model":"gpt","stream":true,"max_output_tokens":100,"input":"hi"}`,
			func(r *Request) bool {
				return string(r.Model) == "gpt" && r.Stream && r.MaxTokens == 100 && r.Stateful == ""
			}},
		{"store true", `{"model":"gpt","store":true}`, func(r *Request) bool { return r.StoreTrue }},
		{"previous response", `{"model":"gpt","previous_response_id":"resp_1"}`, func(r *Request) bool { return r.Stateful == "previous_response_id" }},
		{"previous response null", `{"model":"gpt","previous_response_id":null}`, func(r *Request) bool { return r.Stateful == "" }},
		{"conversation", `{"model":"gpt","conversation":"conv_1"}`, func(r *Request) bool { return r.Stateful == "conversation" }},
		{"background", `{"model":"gpt","background":true}`, func(r *Request) bool { return r.Stateful == "background" }},
		{"stored prompt", `{"model":"gpt","prompt":{"id":"pmpt_1"}}`, func(r *Request) bool { return r.Stateful == "prompt" }},
		{"file", `{"model":"gpt","input":[{"role":"user","content":[{"type":"input_file","file_id":"file-1"}]}]}`,
			func(r *Request) bool { return r.Stateful == "input (a file_id)" }},
		{"file, escaped member name", `{"model":"gpt","input":[{"role":"user","content":[{"type":"input_file","file\u005fid":"file-1"}]}]}`,
			func(r *Request) bool { return r.Stateful == "input (a file_id)" }},
		{"image file, nested in a tool's output", `{"model":"gpt","input":[{"type":"function_call_output","call_id":"c","output":[{"type":"input_image","file_id":"file-2"}]}]}`,
			func(r *Request) bool { return r.Stateful == "input (a file_id)" }},
		{"file in instructions", `{"model":"gpt","instructions":[{"role":"developer","content":[{"type":"input_file","file_id":"file-1"}]}]}`,
			func(r *Request) bool { return r.Stateful == "instructions (a file_id)" }},
		{"file_id null, an image by URL", `{"model":"gpt","input":[{"role":"user","content":[{"type":"input_image","file_id":null,"image_url":"https://x/i.png"}]}]}`,
			func(r *Request) bool { return r.Stateful == "" }},
		{"file_id only in text", `{"model":"gpt","input":[{"role":"user","content":"what is a \"file_id\":\"x\"?"}]}`,
			func(r *Request) bool { return r.Stateful == "" }},
		{"item reference", `{"model":"gpt","input":[{"role":"user","content":"hi"},{"type":"item_reference","id":"msg_1"}]}`,
			func(r *Request) bool { return r.Stateful == "input (an item_reference)" }},
		{"item reference, escaped", `{"model":"gpt","input":[{"t\u0079pe":"item\u005freference","id":"msg_1"}]}`,
			func(r *Request) bool { return r.Stateful == "input (an item_reference)" }},
		{"item reference by id alone", `{"model":"gpt","input":[{"id":"msg_1"}]}`,
			func(r *Request) bool { return r.Stateful == "input (an item_reference)" }},
		{"an echoed item with its id", `{"model":"gpt","input":[{"type":"message","role":"assistant","id":"msg_1","content":[{"type":"output_text","text":"hi"}]}]}`,
			func(r *Request) bool { return r.Stateful == "" }},
		{"server tools", `{"model":"gpt","tools":[{"type":"function","name":"f"},{"type":"web_search"}]}`,
			func(r *Request) bool { return r.ServerTools && r.Stateful == "" }},
		{"file search", `{"model":"gpt","tools":[{"type":"file_search","vector_store_ids":["vs_1"]}]}`,
			func(r *Request) bool { return r.ServerTools && r.Stateful != "" }},
		{"unknown", `{"model":"gpt","novel":1}`, func(r *Request) bool { return r.Unknown == 1 }},
	} {
		var r Request
		if err := r.Scan([]byte(c.body)); err != nil || !c.check(&r) {
			t.Errorf("%s: %v %+v", c.name, err, r)
		}
	}
	var r Request
	if err := r.Scan([]byte(`{"model":"gpt","store":"no"}`)); err == nil {
		t.Error("a store that is not a boolean passed as not storing")
	}
	if err := r.Scan([]byte(`{"input":"hi"}`)); err != ErrNoModel {
		t.Errorf("%v", err)
	}
}

// TestRewrite: the model is the deployment's and store false in every
// form: absent (the provider's default is true) added, null and false set
// false.
func TestRewrite(t *testing.T) {
	for _, body := range []string{
		`{"model":"gpt","input":"hi"}`,
		`{"store":null,"model":"gpt","input":"hi"}`,
		`{"model":"gpt","store":false}`,
	} {
		var r Request
		if err := r.Scan([]byte(body)); err != nil {
			t.Fatal(err)
		}
		out := r.Rewrite(nil, []byte(body), "gpt-5")
		var got map[string]any
		if err := json.Unmarshal(out, &got); err != nil {
			t.Fatalf("%s → %s: %v", body, out, err)
		}
		if got["model"] != "gpt-5" || got["store"] != false {
			t.Errorf("%s → %s", body, out)
		}
	}
}

func TestUsage(t *testing.T) {
	var u StreamUsage
	u.OnEvent([]byte("response.created"), []byte(`{"type":"response.created","response":{"usage":null}}`))
	if u.Final {
		t.Fatal("a non-terminal event is final")
	}
	u.OnEvent([]byte("response.completed"), []byte(`{"type":"response.completed","response":{"id":"r","usage":{"input_tokens":100,"input_tokens_details":{"cached_tokens":40},"output_tokens":20,"output_tokens_details":{"reasoning_tokens":5},"total_tokens":120}}}`))
	if want := (canon.Usage{Input: 60, CacheRead: 40, Output: 20, Reasoning: 5}); !u.Final || u.Usage != want || u.Err != nil {
		t.Fatalf("%+v %v", u.Usage, u.Err)
	}
	var w StreamUsage
	w.Response([]byte(`{"id":"r","output":[],"usage":{"input_tokens":3,"output_tokens":4}}`))
	if !w.Final || w.Usage.Input != 3 || w.Usage.Output != 4 {
		t.Fatalf("%+v", w)
	}
	var p StreamUsage
	p.Response([]byte(`{"usage":{"input_tokens":3}}`))
	if p.Final || p.Err == nil {
		t.Fatal("a partial report suppressed the estimate")
	}
}
