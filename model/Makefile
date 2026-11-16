# Tangles the literate models with lmt and checks them.
# Needs quint (npm i -g @informalsystems/quint) and lmt (go install github.com/driusan/lmt@latest).
QUINT ?= quint
LMT ?= lmt
MODELS = publication exposure attempts

.PHONY: all check tangle verify-tangled invariants check-invariants
all: check

%.qnt: %.md
	$(LMT) $<

tangle: $(MODELS:=.qnt)

# Each check script starts with quint typecheck.
check: tangle
	QUINT=$(QUINT) ./check.sh
	QUINT=$(QUINT) ./check-exposure.sh
	QUINT=$(QUINT) ./check-attempts.sh

# CI: the committed .qnt files must match the markdown.
verify-tangled:
	@for m in $(MODELS); do cp $$m.qnt /tmp/$$m.qnt.committed; done
	$(LMT) $(MODELS:=.md)
	@for m in $(MODELS); do diff -u /tmp/$$m.qnt.committed $$m.qnt; done

# The invariant tables in docs/walkthroughs/README.md (gotogether invariants):
# invariants writes them, check-invariants fails when one is stale or an
# invariant lacks its //quint:enforces or //quint:checks (CI).
invariants:
	./invariants.sh

check-invariants:
	./invariants.sh -check
