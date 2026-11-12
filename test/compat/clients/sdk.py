"""Drives the official Python SDKs through golem for test/compat.

usage: sdk.py <case> <base_url>; prints one JSON object describing what the
client saw. Kept free of golem knowledge: it is what a user's code does.
"""
import json
import os
import sys

import anthropic
import openai


def anthropic_client(base, retries=0):
    return anthropic.Anthropic(base_url=base, api_key=os.environ.get("GOLEM_KEY", "glm_dev_test"), max_retries=retries)


def openai_client(base, retries=0):
    return openai.OpenAI(base_url=base + "/v1", api_key=os.environ.get("GOLEM_KEY", "glm_dev_test"), max_retries=retries)


def run(case, base):
    if case == "anthropic_create":
        m = anthropic_client(base).messages.create(model="claude", max_tokens=64,
                                                    messages=[{"role": "user", "content": "hi"}])
        return {"text": m.content[0].text, "input": m.usage.input_tokens, "output": m.usage.output_tokens}
    if case == "anthropic_stream":
        with anthropic_client(base).messages.stream(model="claude", max_tokens=64,
                                                     messages=[{"role": "user", "content": "hi"}]) as s:
            text = "".join(s.text_stream)
            m = s.get_final_message()
        return {"text": text, "input": m.usage.input_tokens, "output": m.usage.output_tokens}
    if case == "openai_create":
        r = openai_client(base).chat.completions.create(model="gpt", messages=[{"role": "user", "content": "hi"}])
        return {"text": r.choices[0].message.content, "input": r.usage.prompt_tokens, "output": r.usage.completion_tokens}
    if case == "openai_stream":
        # What most code does: read each chunk's first choice, without asking for usage.
        text = ""
        for chunk in openai_client(base).chat.completions.create(
                model="gpt", stream=True, messages=[{"role": "user", "content": "hi"}]):
            text += chunk.choices[0].delta.content or ""
        return {"text": text}
    if case == "openai_stream_usage":
        text, usage = "", None
        for chunk in openai_client(base).chat.completions.create(
                model="gpt", stream=True, stream_options={"include_usage": True},
                messages=[{"role": "user", "content": "hi"}]):
            if chunk.choices:
                text += chunk.choices[0].delta.content or ""
            if chunk.usage:
                usage = chunk.usage
        return {"text": text, "input": usage.prompt_tokens, "output": usage.completion_tokens}
    if case == "anthropic_unknown_model":
        try:
            anthropic_client(base).messages.create(model="nope", max_tokens=8, messages=[{"role": "user", "content": "hi"}])
        except anthropic.APIStatusError as e:
            return {"status": e.status_code, "error_class": type(e).__name__, "message": str(e)}
    if case == "openai_unknown_model":
        try:
            openai_client(base).chat.completions.create(model="nope", messages=[{"role": "user", "content": "hi"}])
        except openai.APIStatusError as e:
            return {"status": e.status_code, "error_class": type(e).__name__, "code": e.code, "message": e.message}
    if case.startswith("retries_"):
        # base is a status server answering every request with the status; count attempts there.
        try:
            if case == "retries_anthropic":
                anthropic_client(base, retries=2).messages.create(model="m", max_tokens=8, messages=[{"role": "user", "content": "hi"}])
            else:
                openai_client(base, retries=2).chat.completions.create(model="m", messages=[{"role": "user", "content": "hi"}])
        except (anthropic.APIStatusError, openai.APIStatusError) as e:
            return {"status": e.status_code, "error_class": type(e).__name__}
    return {"error": "the case did not raise or is unknown"}


if __name__ == "__main__":
    try:
        print(json.dumps(run(sys.argv[1], sys.argv[2])))
    except Exception as e:  # report, do not crash: the Go side asserts on it
        print(json.dumps({"exception": type(e).__name__, "message": str(e)}))
