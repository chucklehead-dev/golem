// Drives the official TypeScript SDKs through golem for test/compat.
// usage: node sdk.mjs <case> <base_url>; prints one JSON object.
import Anthropic from "@anthropic-ai/sdk";
import OpenAI from "openai";

const [, , kase, base] = process.argv;
const anthropic = (retries = 0) => new Anthropic({ baseURL: base, apiKey: "glm_dev_test", maxRetries: retries });
const openai = (retries = 0) => new OpenAI({ baseURL: base + "/v1", apiKey: "glm_dev_test", maxRetries: retries });
const hi = [{ role: "user", content: "hi" }];

async function run() {
  switch (kase) {
    case "anthropic_create": {
      const m = await anthropic().messages.create({ model: "claude", max_tokens: 64, messages: hi });
      return { text: m.content[0].text, input: m.usage.input_tokens, output: m.usage.output_tokens };
    }
    case "anthropic_stream": {
      const s = anthropic().messages.stream({ model: "claude", max_tokens: 64, messages: hi });
      let text = "";
      s.on("text", (t) => (text += t));
      const m = await s.finalMessage();
      return { text, input: m.usage.input_tokens, output: m.usage.output_tokens };
    }
    case "openai_create": {
      const r = await openai().chat.completions.create({ model: "gpt", messages: hi });
      return { text: r.choices[0].message.content, input: r.usage.prompt_tokens, output: r.usage.completion_tokens };
    }
    case "openai_stream": {
      let text = "";
      for await (const chunk of await openai().chat.completions.create({ model: "gpt", stream: true, messages: hi })) {
        text += chunk.choices[0].delta.content ?? "";
      }
      return { text };
    }
    case "openai_stream_usage": {
      let text = "", usage;
      const s = await openai().chat.completions.create({ model: "gpt", stream: true, stream_options: { include_usage: true }, messages: hi });
      for await (const chunk of s) {
        if (chunk.choices.length) text += chunk.choices[0].delta.content ?? "";
        if (chunk.usage) usage = chunk.usage;
      }
      return { text, input: usage.prompt_tokens, output: usage.completion_tokens };
    }
    case "anthropic_unknown_model":
      try { await anthropic().messages.create({ model: "nope", max_tokens: 8, messages: hi }); }
      catch (e) { return { status: e.status, error_class: e.constructor.name, message: e.message }; }
      break;
    case "openai_unknown_model":
      try { await openai().chat.completions.create({ model: "nope", messages: hi }); }
      catch (e) { return { status: e.status, error_class: e.constructor.name, code: e.code, message: e.message }; }
      break;
    case "retries_anthropic":
      try { await anthropic(2).messages.create({ model: "m", max_tokens: 8, messages: hi }); }
      catch (e) { return { status: e.status, error_class: e.constructor.name }; }
      break;
    case "retries_openai":
      try { await openai(2).chat.completions.create({ model: "m", messages: hi }); }
      catch (e) { return { status: e.status, error_class: e.constructor.name }; }
      break;
  }
  return { error: "the case did not raise or is unknown" };
}

run().then((r) => console.log(JSON.stringify(r)), (e) => console.log(JSON.stringify({ exception: e.constructor.name, message: e.message })));
