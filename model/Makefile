# Tangles the literate models with lmt and checks them.
# Needs quint (npm i -g @informalsystems/quint) and lmt (go install github.com/driusan/lmt@latest).
QUINT ?= quint
LMT ?= lmt
MODELS = publication accounting freshness exposure

.PHONY: all check tangle verify-tangled
all: check

%.qnt: %.md
	$(LMT) $<

tangle: $(MODELS:=.qnt)

check: tangle
	QUINT=$(QUINT) ./check.sh
	QUINT=$(QUINT) ./check-accounting.sh
	QUINT=$(QUINT) ./check-freshness.sh
	QUINT=$(QUINT) ./check-exposure.sh

# CI: the committed .qnt files must match the markdown.
verify-tangled:
	@for m in $(MODELS); do cp $$m.qnt /tmp/$$m.qnt.committed; done
	$(LMT) $(MODELS:=.md)
	@for m in $(MODELS); do diff -u /tmp/$$m.qnt.committed $$m.qnt; done
