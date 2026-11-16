# golem: contributing

How to build golem, what to run before a commit, and how its code and
comments are written. What testing exists is in [testing.md](testing.md);
the code's map is the [orientation](walkthroughs/README.md).

## Building

golem builds with Go 1.27.4 or later, the version `go.mod` states (1.27.4
fixes a net/http vulnerability reachable from the gateway's request path).
With `GOTOOLCHAIN=auto`, Go's default, an older `go` downloads it. The
Dockerfile's `GO_VERSION` and CI (`go-version-file: go.mod`) follow
`go.mod`; change them together.

## Before a commit

- `gofmt -l .` prints nothing; `go build ./...` and `go vet ./...` pass.
- `staticcheck ./...` is clean, except the ST1005 findings on error strings
  that start with the proper noun "Entra".
- `go test -short ./...`. The Valkey tests need a server
  (`GOLEM_VALKEY_ADDR=127.0.0.1:6379`); without one they skip.
- After editing a config kind's field comments in `lib/api/v1alpha1`,
  regenerate the chart's CRDs: `GOLEM_UPDATE_CRDS=1 go test ./internal/crdgen/`.
- `helm lint deploy/charts/*` after a chart change.
- `model/invariants.sh -check` after changing a model's invariants or code
  annotated with them (see Model invariants, below).

`internal/doccheck` fails when a Go comment cites an ADR that is now a
stub, or a doc's section number (see below). `internal/archtest` fails on
an import that crosses the layers.

## Model invariants

A model's invariants are the conjuncts of its `safe` val. A new one needs
three things, or CI's `model/invariants.sh -check` fails:

- its name in the model's `model/<model>.invariants.yaml`, and a one-line
  comment directly above the val saying what it means (the table shows
  that first line only);
- `//quint:enforces <name>` on the function, type or statement whose logic
  makes it hold, and `//quint:checks <name>` on each Test or Fuzz function
  that fails if it breaks, in a package `model/invariants.sh` scans for
  that model;
- the tables written again: `model/invariants.sh` (needs quint and the
  gotogether CLI at go.mod's version; the script says how to install it).

The tables carry line numbers, so moving annotated code also needs the
script run again.

## Writing comments

Comments say what the code does and why, in plain sentences of at most
about 25 words, with at most one colon or semicolon a sentence. Shorter is
better.

- No section or issue numbers. Sections are renumbered whenever a doc is
  edited, and an issue is closed. Name the doc, or the section's title, if
  the reader needs it: "(operations.md, Emergency revocation)".
- Cite an ADR by the record that holds the decision now. A superseded or
  consolidated record is a one-line stub (docs/adr/README.md lists them).
  Put the citation at the end of a sentence, in parentheses, never in the
  middle.
- Plans and history go in docs, or in a `TODO(ADR nnnn):` or `TODO:` line,
  not in the description of what the code does now.
- Never a letter for a concept ("B" for the spend boundary), and never a
  word the code also uses for something else. "Lease" is only the
  snapshot's authorization lease; "slice" is only Go's.
- A doc comment starts with the name it documents, in a full sentence:
  "ErrStale means ...", not "ErrStale: ...".

For example, a struct's fields:

```go
// fence is the fence this replica claimed at start; claimed reports
// whether that claim succeeded. deposed becomes true, permanently, once
// another replica holds the fence.
```

The [glossary](walkthroughs/README.md#glossary) has the words the code
uses for golem's concepts.

## JSON

New code uses `encoding/json/v2` and `encoding/json/jsontext`: strict
decoding (`RejectUnknownMembers`), token-level scans, `omitzero`. The v1
`encoding/json` stays only where a dependency needs it, as
`sigs.k8s.io/yaml` does for the config kinds, and in code not yet moved.
Moving existing code is its own change, with its tests.
