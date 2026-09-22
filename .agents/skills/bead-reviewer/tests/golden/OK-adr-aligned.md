# Expected Finding: OK-adr-aligned

**Fixture**: `fixtures/OK-adr-aligned.json`
**Pattern**: (none — negative fixture)
**Expected severity**: n/a
**Expected outcome**: NO Pass 2 Critical or HIGH finding.

This fixture is the boundary case for ADR-VIOLATION. The bead extends the citation auditor (the subject of ADR-0002) with a new pattern module. The description explicitly invokes ADR-0002 Decision 1 (installed `ccore audit-citations`, Python runtime), Decision 2 (thinned marketplace skill plus ccore), Decision 5 (URL candidates), and Decision 7 (CitationPattern interface), and the proposed work aligns with all of them.

The fixture deliberately mentions ADR-0002 multiple times to test that the reviewer does NOT pattern-match "mentions ADR-0002" → "violates ADR-0002". The fixture also lives in the same problem domain as the ADR-VIOLATION fixture, so this pair is the most informative regression guard.

**Minimum required in output**: Pass 2 reports "ADR check: CLEAN — no ADR violations detected." ADR-VIOLATION MUST NOT be flagged. The reviewer should recognise that:
- Installed `ccore audit-citations` / Python matches Decision 1 (does NOT conflict).
- A ccore-owned pattern module matches Decision 2 (does NOT conflict).
- The CitationPattern interface usage matches Decision 7 (does NOT conflict).

**Failure mode this fixture guards against**: Drift that causes the LLM to flag any bead in the citation-auditor domain as an ADR-0002 violation regardless of whether it actually conflicts with a specific decision.
