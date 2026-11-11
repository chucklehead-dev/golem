# Tangles the literate models with lmt and checks them.
# Needs quint (npm i -g @informalsystems/quint) and lmt (go install github.com/driusan/lmt@latest).
QUINT ?= quint
LMT ?= lmt

.PHONY: all check tangle verify-tangled
all: check

publication.qnt: publication.md
	$(LMT) publication.md

tangle: publication.qnt

check: publication.qnt
	QUINT=$(QUINT) ./check.sh

# CI: the committed .qnt must match the markdown.
verify-tangled:
	cp publication.qnt /tmp/publication.qnt.committed
	$(LMT) publication.md
	diff -u /tmp/publication.qnt.committed publication.qnt
