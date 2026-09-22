# Expected Finding: ADR-VIOLATION

**Fixture**: `fixtures/ADR-VIOLATION.json`
**Pattern**: ADR-VIOLATION — Conflicts with an Architecture Decision Record
**Expected severity**: Critical
**Expected finding excerpt**: The bead proposes reviving Bun + TypeScript skill scripts under `skills/audit-citations/scripts/` for the citation auditor. ADR-0002 Decision 1 states that installed `ccore audit-citations` is the runtime (Python). Reintroducing a Bun/TypeScript second runtime directly contradicts the accepted ADR decision.
**ADR reference**: `docs/adr/ADR-0002-citation-auditor-architecture.md` — Decision 1 (Runtime: installed `ccore audit-citations`)
**Anti-pattern code in finding**: [ADR-VIOLATION]
**Minimum required in output**: A Critical finding referencing ADR-VIOLATION and ADR-0002 Decision 1, citing bead text such as "Bun + TypeScript" or `skills/audit-citations/scripts/`, noting the conflict with the installed `ccore audit-citations` Python runtime. Finding must reference the specific ADR file and decision number.
