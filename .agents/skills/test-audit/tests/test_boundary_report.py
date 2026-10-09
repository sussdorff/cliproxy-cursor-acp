"""Contract tests for the test-audit boundary report, driven through its CLI.

The fixture repositories under ``fixtures/`` are read, never executed.
"""

from __future__ import annotations

import json
import subprocess
import sys
from pathlib import Path

import pytest

SKILL_ROOT = Path(__file__).resolve().parents[1]
SCRIPT = SKILL_ROOT / "scripts" / "boundary_report.py"
FIXTURES = Path(__file__).resolve().parent / "fixtures"


def run_report(
    *args: str | Path, cwd: Path | None = None
) -> subprocess.CompletedProcess[str]:
    return subprocess.run(
        [sys.executable, str(SCRIPT), *(str(arg) for arg in args)],
        text=True,
        capture_output=True,
        check=False,
        cwd=cwd,
    )


def test_repository_without_declaration_reports_no_declared_units() -> None:
    result = run_report("--repo", FIXTURES / "undeclared")

    assert result.returncode == 0, result.stderr
    assert "no declared test units" in result.stdout


def test_repository_without_declaration_reports_no_units_as_json() -> None:
    result = run_report("--repo", FIXTURES / "undeclared", "--json")

    assert result.returncode == 0, result.stderr
    report = json.loads(result.stdout)
    assert report["declared"] is False
    assert report["modules"] == []
    assert report["ports"] == []


DECLARED = FIXTURES / "declared"


def declared_report() -> dict:
    result = run_report(
        "--repo", DECLARED, "--declaration", DECLARED / "test-units.json", "--json"
    )
    assert result.returncode == 0, result.stderr
    return json.loads(result.stdout)


def module_entry(report: dict, name: str) -> dict:
    (entry,) = [module for module in report["modules"] if module["name"] == name]
    return entry


def test_test_outside_a_module_importing_an_internal_is_a_boundary_bypass() -> None:
    proposals = module_entry(declared_report(), "proposals")

    bypasses = [f for f in proposals["findings"] if f["kind"] == "boundary-bypass"]
    assert [(f["test"], f["target"]) for f in bypasses] == [
        ("tests/proposals-bypass.test.ts", "src/proposals/internal/score.ts")
    ]


def test_test_importing_only_the_module_entry_is_clean() -> None:
    proposals = module_entry(declared_report(), "proposals")

    flagged = {f["test"] for f in proposals["findings"]}
    assert "tests/proposals-entry.test.ts" not in flagged


def test_tests_under_skipped_directories_are_not_read() -> None:
    proposals = module_entry(declared_report(), "proposals")

    flagged = {f["test"] for f in proposals["findings"]}
    assert not any(path.startswith("dist/") for path in flagged)


def test_test_inside_a_module_importing_an_internal_needs_a_stated_reason() -> None:
    proposals = module_entry(declared_report(), "proposals")

    unexplained = [
        (f["test"], f["target"])
        for f in proposals["findings"]
        if f["kind"] == "internal-without-reason"
    ]
    assert unexplained == [
        ("src/proposals/score.test.ts", "src/proposals/internal/score.ts")
    ]


def test_stated_exception_is_shown_with_its_reason_and_is_no_finding() -> None:
    proposals = module_entry(declared_report(), "proposals")

    assert [(e["test"], e["target"], e["reason"]) for e in proposals["stated_exceptions"]] == [
        (
            "src/proposals/rank.test.ts",
            "src/proposals/internal/rank.ts",
            "pins the tie-break order the entry cannot expose",
        )
    ]
    assert "src/proposals/rank.test.ts" not in {f["test"] for f in proposals["findings"]}


def port_entry(report: dict, name: str) -> dict:
    (entry,) = [port for port in report["ports"] if port["name"] == name]
    return entry


def test_adapter_run_through_the_contract_suite_is_covered() -> None:
    storage = port_entry(declared_report(), "storage")

    runs = {adapter["name"]: adapter["contract_runs"] for adapter in storage["adapters"]}
    assert runs["memory"] == ["tests/test_storage_memory.py"]


def test_adapter_without_a_contract_suite_run_is_a_finding() -> None:
    storage = port_entry(declared_report(), "storage")

    missing = [
        f["adapter"]
        for f in storage["findings"]
        if f["kind"] == "adapter-without-contract-run"
    ]
    assert missing == ["postgres"]


def test_test_importing_a_real_adapter_outside_a_contract_run_is_a_port_bypass() -> None:
    storage = port_entry(declared_report(), "storage")

    bypasses = [
        (f["test"], f["adapter"], f["target"])
        for f in storage["findings"]
        if f["kind"] == "port-bypass"
    ]
    assert bypasses == [
        ("tests/test_storage_postgres.py", "postgres", "pkg/storage/postgres.py")
    ]


def write_repo(root: Path, files: dict[str, str], declaration: dict) -> Path:
    for relative, content in files.items():
        path = root / relative
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(content, encoding="utf-8")
    declaration_path = root / "test-units.json"
    declaration_path.write_text(json.dumps(declaration), encoding="utf-8")
    return declaration_path


def test_port_without_an_in_memory_adapter_is_a_finding(tmp_path: Path) -> None:
    declaration = write_repo(
        tmp_path,
        {
            "mail/port.py": "",
            "mail/contract.py": "",
            "mail/smtp.py": "",
            "tests/test_smtp.py": "from mail import contract, smtp\n",
        },
        {
            "ports": [
                {
                    "name": "mail",
                    "port": "mail/port.py",
                    "contract": "mail/contract.py",
                    "adapters": [{"name": "smtp", "path": "mail/smtp.py"}],
                }
            ]
        },
    )

    result = run_report("--repo", tmp_path, "--declaration", declaration, "--json")

    assert result.returncode == 0, result.stderr
    mail = port_entry(json.loads(result.stdout), "mail")
    assert [f["kind"] for f in mail["findings"]] == ["port-without-in-memory-adapter"]


VALID_FILES = {
    "lib/a/index.ts": "",
    "lib/a/inner/x.ts": "",
    "lib/b/index.ts": "",
    "lib/other.ts": "",
    "ports/p/port.ts": "",
    "ports/p/contract.ts": "",
    "ports/p/memory.ts": "",
}


def module(name: str, directory: str, entry: str) -> dict:
    return {"name": name, "dir": directory, "entry": entry}


def port(**overrides: object) -> dict:
    declared = {
        "name": "p",
        "port": "ports/p/port.ts",
        "contract": "ports/p/contract.ts",
        "adapters": [{"name": "memory", "path": "ports/p/memory.ts", "in_memory": True}],
    }
    declared.update(overrides)
    return declared


@pytest.mark.parametrize(
    ("declaration", "message"),
    [
        ({"modules": [module("a", "lib/a", "lib/other.ts")]}, "outside"),
        ({"modules": [module("a", "lib/missing", "lib/missing/index.ts")]}, "lib/missing"),
        ({"modules": [module("a", "lib/a", "lib/a/main.ts")]}, "lib/a/main.ts"),
        (
            {"modules": [module("a", "lib/a", "lib/a/index.ts"), module("a", "lib/b", "lib/b/index.ts")]},
            "duplicate",
        ),
        (
            {"modules": [module("a", "lib/a", "lib/a/index.ts"), module("x", "lib/a/inner", "lib/a/inner/x.ts")]},
            "nested",
        ),
        ({"modules": [{"name": "a", "dir": "lib/a"}]}, "entry"),
        ({"modules": [module("a", "../lib/a", "../lib/a/index.ts")]}, "repository-relative"),
        ({"ports": [port(contract="ports/p/missing.ts")]}, "ports/p/missing.ts"),
        ({"ports": [port(adapters=[])]}, "adapters"),
        (
            {"ports": [port(adapters=[{"name": "memory", "path": "ports/p/gone.ts", "in_memory": True}])]},
            "ports/p/gone.ts",
        ),
        ({"modules": {"name": "a"}}, "modules"),
        ({"aliases": ["@/"]}, "aliases"),
        ({"aliases": {"@/": "lib/missing"}}, "lib/missing"),
        ({"import_roots": "lib"}, "import_roots"),
        ({"import_roots": ["../lib"]}, "repository-relative"),
    ],
)
def test_invalid_declaration_fails_with_a_clear_message(
    tmp_path: Path, declaration: dict, message: str
) -> None:
    path = write_repo(tmp_path, VALID_FILES, declaration)

    result = run_report("--repo", tmp_path, "--declaration", path)

    assert result.returncode == 2
    assert "invalid declaration" in result.stderr
    assert message in result.stderr
    assert result.stdout == ""


@pytest.mark.parametrize(
    ("field", "inside"),
    [
        ("port", "lib/a/inner/x.ts"),
        ("contract", "lib/a/inner/x.ts"),
        ("contract", "lib/a/index.ts"),
    ],
)
def test_port_or_contract_inside_a_module_fails(
    tmp_path: Path, field: str, inside: str
) -> None:
    declaration = {
        "modules": [module("records", "lib/a", "lib/a/index.ts")],
        "ports": [port(**{field: inside})],
    }
    path = write_repo(tmp_path, VALID_FILES, declaration)

    result = run_report("--repo", tmp_path, "--declaration", path)

    assert result.returncode == 2
    assert "invalid declaration" in result.stderr
    for name in ("'p'", inside, "'records'"):
        assert name in result.stderr
    assert result.stdout == ""


def test_module_entry_may_be_the_port_and_importing_it_is_no_bypass(
    tmp_path: Path,
) -> None:
    declaration = {
        "modules": [module("records", "lib/a", "lib/a/index.ts")],
        "ports": [port(port="lib/a/index.ts")],
    }
    files = {**VALID_FILES, "tests/caller.test.ts": 'import { A } from "../lib/a";\n'}
    path = write_repo(tmp_path, files, declaration)

    result = run_report("--repo", tmp_path, "--declaration", path, "--json")

    assert result.returncode == 0, result.stderr
    report = json.loads(result.stdout)
    assert module_entry(report, "records")["findings"] == []
    assert port_entry(report, "p")["port"] == "lib/a/index.ts"


@pytest.mark.parametrize(
    "reason_line",
    [
        "/* test-boundary-exception: pins the order */",
        "<!-- test-boundary-exception: pins the order -->",
        "<!-- test-boundary-exception: pins the order --!>",
        "<!-- test-boundary-exception: pins the order --!> */ ",
    ],
)
def test_stated_reason_drops_a_trailing_comment_terminator(
    tmp_path: Path, reason_line: str
) -> None:
    files = {
        **VALID_FILES,
        "lib/a/inner/x.test.ts": f'import {{ X }} from "./x";\n{reason_line}\n',
    }
    path = write_repo(
        tmp_path, files, {"modules": [module("a", "lib/a", "lib/a/index.ts")]}
    )

    result = run_report("--repo", tmp_path, "--declaration", path, "--json")

    assert result.returncode == 0, result.stderr
    (stated,) = module_entry(json.loads(result.stdout), "a")["stated_exceptions"]
    assert stated["reason"] == "pins the order"


def test_named_declaration_that_does_not_exist_fails(tmp_path: Path) -> None:
    result = run_report("--repo", tmp_path, "--declaration", tmp_path / "absent.json")

    assert result.returncode == 2
    assert "absent.json" in result.stderr


def test_text_report_groups_findings_under_their_module_and_port() -> None:
    result = run_report("--repo", DECLARED, "--declaration", DECLARED / "test-units.json")

    assert result.returncode == 0, result.stderr
    text = result.stdout
    module_at = text.index("module proposals")
    port_at = text.index("port storage")
    bypass_at = text.index("boundary-bypass: tests/proposals-bypass.test.ts")
    adapter_at = text.index("adapter-without-contract-run: postgres")
    assert module_at < bypass_at < port_at < adapter_at
    assert "4 findings" in text


@pytest.mark.parametrize(
    ("test_path", "source"),
    [
        ("tests/a.test.ts", 'import {\n  x,\n  y,\n} from "../lib/a/inner/x";\n'),
        ("tests/a.test.ts", 'import type { X } from "../lib/a/inner/x";\n'),
        ("tests/a.test.ts", 'export { x } from "../lib/a/inner/x";\n'),
        ("tests/a.test.ts", 'import "../lib/a/inner/x";\n'),
        ("tests/a.test.ts", 'const x = require("../lib/a/inner/x");\n'),
        ("tests/a.test.ts", 'vi.mock("../lib/a/inner/x", () => ({}));\n'),
        ("tests/a.test.ts", 'jest.mock("../lib/a/inner/x");\n'),
        ("tests/a.test.ts", 'import { x } from "../lib/a/inner/x.js";\n'),
        ("tests/test_a.py", "from lib.a.inner import x\n"),
        ("tests/test_a.py", "import lib.a.inner.x\n"),
        ("lib/b/b_test.py", "from ..a.inner import x\n"),
    ],
)
def test_each_supported_import_form_is_seen(
    tmp_path: Path, test_path: str, source: str
) -> None:
    files = {
        "lib/__init__.py": "",
        "lib/a/__init__.py": "",
        "lib/a/index.ts": "",
        "lib/a/inner/__init__.py": "",
        "lib/a/inner/x.ts": "",
        "lib/a/inner/x.py": "",
        "lib/b/__init__.py": "",
        test_path: source,
    }
    path = write_repo(
        tmp_path, files, {"modules": [module("a", "lib/a", "lib/a/index.ts")]}
    )

    result = run_report("--repo", tmp_path, "--declaration", path, "--json")

    assert result.returncode == 0, result.stderr
    (finding,) = module_entry(json.loads(result.stdout), "a")["findings"]
    assert finding["kind"] == "boundary-bypass"
    assert finding["test"] == test_path
    assert finding["target"].startswith("lib/a/inner/x.")


def test_python_module_resolves_from_src_root(tmp_path: Path) -> None:
    files = {
        "src/app/core/__init__.py": "",
        "src/app/core/engine.py": "",
        "tests/test_engine.py": "from app.core.engine import run\n",
    }
    path = write_repo(
        tmp_path,
        files,
        {"modules": [module("core", "src/app/core", "src/app/core/__init__.py")]},
    )

    result = run_report("--repo", tmp_path, "--declaration", path, "--json")

    (finding,) = module_entry(json.loads(result.stdout), "core")["findings"]
    assert finding["target"] == "src/app/core/engine.py"


def test_agents_md_paragraph_names_the_declaration_without_an_argument() -> None:
    result = run_report("--repo", DECLARED, "--json")

    assert result.returncode == 0, result.stderr
    report = json.loads(result.stdout)
    assert report["declared"] is True
    assert report["declaration_source"] == "agents_md"
    assert report["declaration"] == str(DECLARED / "test-units.json")
    assert report["finding_count"] == declared_report()["finding_count"] == 4


def test_text_report_says_the_declaration_came_from_agents_md() -> None:
    result = run_report("--repo", DECLARED)

    assert result.returncode == 0, result.stderr
    first_line = result.stdout.splitlines()[0]
    assert first_line == (
        f"declared test units from {DECLARED / 'test-units.json'} (named in AGENTS.md)"
    )


def test_declaration_argument_is_reported_as_the_source() -> None:
    assert declared_report()["declaration_source"] == "argument"


def test_repository_without_the_paragraph_reports_source_none() -> None:
    result = run_report("--repo", FIXTURES / "undeclared", "--json")

    assert result.returncode == 0, result.stderr
    assert json.loads(result.stdout)["declaration_source"] == "none"


def test_undeclared_text_output_is_unchanged() -> None:
    result = run_report("--repo", FIXTURES / "undeclared")

    assert result.returncode == 0, result.stderr
    assert result.stdout == "no declared test units: the audit runs unchanged\n"


PARAGRAPH = (
    "## Test units\n\n"
    "This repository declares its test units in `{path}`. Tests observe a declared"
    " module only through its entry.\n"
)


def test_repository_without_agents_md_reports_no_declared_units(tmp_path: Path) -> None:
    (tmp_path / "tests").mkdir()
    (tmp_path / "tests" / "a.test.ts").write_text("", encoding="utf-8")

    result = run_report("--repo", tmp_path, "--json")

    assert result.returncode == 0, result.stderr
    report = json.loads(result.stdout)
    assert (report["declared"], report["declaration_source"]) == (False, "none")


def test_paragraph_naming_a_missing_file_fails(tmp_path: Path) -> None:
    (tmp_path / "AGENTS.md").write_text(
        PARAGRAPH.format(path="config/absent.json"), encoding="utf-8"
    )

    result = run_report("--repo", tmp_path)

    assert result.returncode == 2
    assert "invalid declaration" in result.stderr
    assert "config/absent.json" in result.stderr
    assert result.stdout == ""


def test_paragraph_naming_an_invalid_declaration_fails(tmp_path: Path) -> None:
    write_repo(tmp_path, VALID_FILES, {"modules": [module("a", "lib/a", "lib/other.ts")]})
    (tmp_path / "AGENTS.md").write_text(
        PARAGRAPH.format(path="test-units.json"), encoding="utf-8"
    )

    result = run_report("--repo", tmp_path)

    assert result.returncode == 2
    assert "outside" in result.stderr


@pytest.mark.parametrize("path", ["/etc/test-units.json", "../test-units.json"])
def test_paragraph_naming_a_path_outside_the_repository_fails(
    tmp_path: Path, path: str
) -> None:
    (tmp_path / "AGENTS.md").write_text(PARAGRAPH.format(path=path), encoding="utf-8")

    result = run_report("--repo", tmp_path)

    assert result.returncode == 2
    assert "repository-relative" in result.stderr


def test_first_paragraph_sentence_wins_and_may_be_wrapped(tmp_path: Path) -> None:
    write_repo(tmp_path, VALID_FILES, {"modules": [module("a", "lib/a", "lib/a/index.ts")]})
    (tmp_path / "AGENTS.md").write_text(
        "## Test units\n\nThis repository declares its test units\nin `test-units.json`.\n\n"
        + PARAGRAPH.format(path="config/absent.json"),
        encoding="utf-8",
    )

    result = run_report("--repo", tmp_path, "--json")

    assert result.returncode == 0, result.stderr
    assert json.loads(result.stdout)["declaration"] == str(tmp_path / "test-units.json")


@pytest.mark.parametrize(
    "agents_md",
    [
        pytest.param(
            "## Test units\n\n<!--\n" + PARAGRAPH.format(path="test-units.json") + "-->\n",
            id="html-comment",
        ),
        pytest.param(
            "## Test units\n\n<!--\n" + PARAGRAPH.format(path="test-units.json") + "--!>\n",
            id="html-comment-bang-close",
        ),
        pytest.param(
            "## Test units\n\nThe sentence reads:\n\n"
            "    This repository declares its test units in `test-units.json`.\n",
            id="space-indented-code",
        ),
        pytest.param(
            "## Test units\n\n"
            "\tThis repository declares its test units in `test-units.json`.\n",
            id="tab-indented-code",
        ),
        pytest.param(
            "## Test units\n\n```markdown\n"
            + PARAGRAPH.format(path="test-units.json")
            + "```\n",
            id="fenced-code",
        ),
        pytest.param(
            "## Test units\n\n> This repository declares its test units in"
            " `test-units.json`.\n",
            id="blockquote",
        ),
        pytest.param(
            "## Testing\n\nThis repository declares its test units in `test-units.json`.\n",
            id="other-section",
        ),
        pytest.param(
            "This repository declares its test units in `test-units.json`.\n",
            id="no-section",
        ),
        pytest.param(
            "## Test units\n\nThis repository declares its test units\n\nin"
            " `test-units.json`.\n",
            id="split-across-paragraphs",
        ),
    ],
)
def test_inactive_paragraph_declares_nothing(tmp_path: Path, agents_md: str) -> None:
    write_repo(tmp_path, VALID_FILES, {"modules": [module("a", "lib/a", "lib/a/index.ts")]})
    (tmp_path / "AGENTS.md").write_text(agents_md, encoding="utf-8")

    result = run_report("--repo", tmp_path, "--json")

    assert result.returncode == 0, result.stderr
    report = json.loads(result.stdout)
    assert (report["declared"], report["declaration_source"]) == (False, "none")


def test_comment_closed_with_bang_close_ends_before_the_real_paragraph(
    tmp_path: Path,
) -> None:
    write_repo(tmp_path, VALID_FILES, {"modules": [module("a", "lib/a", "lib/a/index.ts")]})
    (tmp_path / "AGENTS.md").write_text(
        "## Test units\n\n<!--\nThis repository declares its test units in `old.json`.\n"
        "--!>\n\nThis repository declares its test units in `test-units.json`.\n",
        encoding="utf-8",
    )

    result = run_report("--repo", tmp_path, "--json")

    assert result.returncode == 0, result.stderr
    report = json.loads(result.stdout)
    assert report["declaration_source"] == "agents_md"
    assert report["declaration"] == str(tmp_path / "test-units.json")


@pytest.mark.parametrize("indent", ["    ", "\t"])
def test_indented_example_before_the_real_paragraph_is_skipped(
    tmp_path: Path, indent: str
) -> None:
    write_repo(tmp_path, VALID_FILES, {"modules": [module("a", "lib/a", "lib/a/index.ts")]})
    (tmp_path / "AGENTS.md").write_text(
        "## Test units\n\nAn example:\n\n"
        f"{indent}This repository declares its test units in `old.json`.\n\n"
        "This repository declares its test units in\n"
        "    `test-units.json`. Tests observe a declared module only through its entry.\n",
        encoding="utf-8",
    )

    result = run_report("--repo", tmp_path, "--json")

    assert result.returncode == 0, result.stderr
    report = json.loads(result.stdout)
    assert report["declaration_source"] == "agents_md"
    assert report["declaration"] == str(tmp_path / "test-units.json")


def test_fenced_template_before_the_real_paragraph_is_skipped(tmp_path: Path) -> None:
    write_repo(tmp_path, VALID_FILES, {"modules": [module("a", "lib/a", "lib/a/index.ts")]})
    (tmp_path / "AGENTS.md").write_text(
        "## Test units\n\nThe template:\n\n~~~markdown\n"
        + PARAGRAPH.format(path="<declaration path>")
        + "~~~\n\n<!-- This repository declares its test units in `old.json`. -->\n\n"
        "This repository declares its test units in\n`test-units.json`. More text.\n",
        encoding="utf-8",
    )

    result = run_report("--repo", tmp_path, "--json")

    assert result.returncode == 0, result.stderr
    report = json.loads(result.stdout)
    assert report["declaration_source"] == "agents_md"
    assert report["declaration"] == str(tmp_path / "test-units.json")


def test_declaration_argument_overrides_the_paragraph(tmp_path: Path) -> None:
    write_repo(tmp_path, VALID_FILES, {"modules": [module("a", "lib/a", "lib/a/index.ts")]})
    (tmp_path / "AGENTS.md").write_text(
        PARAGRAPH.format(path="config/absent.json"), encoding="utf-8"
    )

    result = run_report(
        "--repo", tmp_path, "--declaration", "test-units.json", "--json"
    )

    assert result.returncode == 0, result.stderr
    report = json.loads(result.stdout)
    assert report["declaration_source"] == "argument"
    assert report["declaration"] == str(tmp_path / "test-units.json")


def test_relative_declaration_resolves_against_the_repository(tmp_path: Path) -> None:
    result = run_report(
        "--repo", DECLARED, "--declaration", "test-units.json", "--json", cwd=tmp_path
    )

    assert result.returncode == 0, result.stderr
    assert json.loads(result.stdout)["declared"] is True


PORT_FILES = {
    "ports/p/port.ts": "",
    "ports/p/contract.ts": "",
    "ports/p/memory.ts": "",
    "ports/p/pg/index.ts": "",
    "ports/p/pg/client.ts": "",
}

PG_PORT = {
    "ports": [
        port(
            adapters=[
                {"name": "memory", "path": "ports/p/memory.ts", "in_memory": True},
                {"name": "pg", "path": "ports/p/pg"},
            ]
        )
    ]
}


def port_report_for(root: Path, tests: dict[str, str]) -> dict:
    path = write_repo(root, {**PORT_FILES, **tests}, PG_PORT)
    result = run_report("--repo", root, "--declaration", path, "--json")
    assert result.returncode == 0, result.stderr
    return port_entry(json.loads(result.stdout), "p")


def test_contract_suite_runs_adapter_tests_and_callers_through_the_port_are_no_bypass(
    tmp_path: Path,
) -> None:
    p = port_report_for(
        tmp_path,
        {
            "tests/pg.contract.test.ts": (
                'import { run } from "../ports/p/contract";\n'
                'import { Pg } from "../ports/p/pg";\n'
            ),
            "tests/memory.contract.test.ts": (
                'import { run } from "../ports/p/contract";\n'
                'import { Memory } from "../ports/p/memory";\n'
            ),
            "ports/p/pg/client.test.ts": 'import { connect } from "./client";\n',
            "tests/caller.test.ts": (
                'import type { Store } from "../ports/p/port";\n'
                'import { Memory } from "../ports/p/memory";\n'
            ),
        },
    )

    assert p["findings"] == []
    runs = {adapter["name"]: adapter["contract_runs"] for adapter in p["adapters"]}
    assert runs == {
        "memory": ["tests/memory.contract.test.ts"],
        "pg": ["tests/pg.contract.test.ts"],
    }


def test_caller_importing_a_real_adapter_is_a_port_bypass(tmp_path: Path) -> None:
    p = port_report_for(
        tmp_path,
        {"tests/caller.test.ts": 'import { connect } from "../ports/p/pg/client";\n'},
    )

    bypasses = [
        (f["test"], f["adapter"], f["target"])
        for f in p["findings"]
        if f["kind"] == "port-bypass"
    ]
    assert bypasses == [("tests/caller.test.ts", "pg", "ports/p/pg/client.ts")]


def test_contract_import_with_several_adapters_is_ambiguous_and_covers_none(
    tmp_path: Path,
) -> None:
    p = port_report_for(
        tmp_path,
        {
            "tests/mixed.test.ts": (
                'import { run } from "../ports/p/contract";\n'
                'import { Memory } from "../ports/p/memory";\n'
                'import { Pg } from "../ports/p/pg";\n'
            )
        },
    )

    ambiguous = [
        (f["test"], f["adapters"])
        for f in p["findings"]
        if f["kind"] == "ambiguous-contract-run"
    ]
    assert ambiguous == [("tests/mixed.test.ts", ["memory", "pg"])]
    uncovered = [
        f["adapter"] for f in p["findings"] if f["kind"] == "adapter-without-contract-run"
    ]
    assert uncovered == ["memory", "pg"]


def unchecked_report(root: Path, files: dict[str, str], declaration: dict) -> dict:
    path = write_repo(root, files, declaration)
    result = run_report("--repo", root, "--declaration", path, "--json")
    assert result.returncode == 0, result.stderr
    return json.loads(result.stdout)


MODULE_A = {"modules": [module("a", "lib/a", "lib/a/index.ts")]}


def test_unresolved_and_alias_specifiers_are_reported_as_unchecked(tmp_path: Path) -> None:
    report = unchecked_report(
        tmp_path,
        {
            "lib/a/index.ts": "",
            "tests/a.test.ts": (
                'import { x } from "../lib/missing";\n'
                'import { y } from "@/a/inner/x";\n'
                'import { z } from "~/a";\n'
                'import { w } from "#internal/a";\n'
                'import React from "react";\n'
                'import { q } from "@scope/pkg";\n'
            ),
            "tests/test_a.py": "from .missing import thing\nimport requests\n",
        },
        MODULE_A,
    )

    assert report["unchecked"] == [
        {"test": "tests/a.test.ts", "specifiers": ["../lib/missing", "@/a/inner/x", "~/a", "#internal/a"]},
        {"test": "tests/test_a.py", "specifiers": [".missing"]},
    ]
    assert report["unchecked_count"] == 5


def test_text_report_counts_unchecked_specifiers(tmp_path: Path) -> None:
    path = write_repo(
        tmp_path,
        {"lib/a/index.ts": "", "tests/a.test.ts": 'import { y } from "@/a/inner/x";\n'},
        MODULE_A,
    )

    result = run_report("--repo", tmp_path, "--declaration", path)

    assert result.returncode == 0, result.stderr
    assert "unchecked: tests/a.test.ts: @/a/inner/x" in result.stdout
    assert "1 unchecked specifier" in result.stdout


def test_declared_alias_makes_a_ts_specifier_resolvable(tmp_path: Path) -> None:
    report = unchecked_report(
        tmp_path,
        {
            "lib/a/index.ts": "",
            "lib/a/inner/x.ts": "",
            "tests/a.test.ts": 'import { y } from "@/a/inner/x";\n',
        },
        {**MODULE_A, "aliases": {"@/": "lib/"}},
    )

    assert report["unchecked"] == []
    (finding,) = module_entry(report, "a")["findings"]
    assert (finding["kind"], finding["target"]) == ("boundary-bypass", "lib/a/inner/x.ts")


def test_declared_import_root_makes_a_python_import_resolvable(tmp_path: Path) -> None:
    report = unchecked_report(
        tmp_path,
        {
            "app/backend/svc/__init__.py": "",
            "app/backend/svc/core/__init__.py": "",
            "app/backend/svc/core/engine.py": "",
            "tests/test_engine.py": "from svc.core.engine import run\n",
        },
        {
            "modules": [
                module("core", "app/backend/svc/core", "app/backend/svc/core/__init__.py")
            ],
            "import_roots": ["app/backend"],
        },
    )

    (finding,) = module_entry(report, "core")["findings"]
    assert finding["target"] == "app/backend/svc/core/engine.py"


def test_contract_file_not_named_like_a_test_counts_as_a_run(tmp_path: Path) -> None:
    p = port_report_for(
        tmp_path, {"ports/p/contract.ts": 'import { Pg } from "./pg";\n'}
    )

    runs = {adapter["name"]: adapter["contract_runs"] for adapter in p["adapters"]}
    assert runs["pg"] == ["ports/p/contract.ts"]


def test_contract_file_importing_several_adapters_is_ambiguous(tmp_path: Path) -> None:
    p = port_report_for(
        tmp_path,
        {
            "ports/p/contract.ts": (
                'import { Memory } from "./memory";\nimport { Pg } from "./pg";\n'
            )
        },
    )

    ambiguous = [
        (f["test"], f["adapters"])
        for f in p["findings"]
        if f["kind"] == "ambiguous-contract-run"
    ]
    assert ambiguous == [("ports/p/contract.ts", ["memory", "pg"])]


def test_unparseable_and_undecodable_test_files_are_unchecked(tmp_path: Path) -> None:
    path = write_repo(
        tmp_path,
        {"lib/a/index.ts": "", "tests/test_broken.py": "def (:\n"},
        MODULE_A,
    )
    (tmp_path / "tests" / "binary.test.ts").write_bytes(b'import "\xff\xfe";\n')

    result = run_report("--repo", tmp_path, "--declaration", path, "--json")

    assert result.returncode == 0, result.stderr
    report = json.loads(result.stdout)
    reasons = {entry["test"]: entry["specifiers"] for entry in report["unchecked"]}
    assert set(reasons) == {"tests/binary.test.ts", "tests/test_broken.py"}
    assert reasons["tests/test_broken.py"][0].startswith("unparseable: ")
    assert reasons["tests/binary.test.ts"][0].startswith("unreadable: ")
    assert report["unchecked_count"] == 2


@pytest.mark.parametrize("kind", ["directory", "invalid-utf8"])
def test_declaration_that_cannot_be_read_fails_with_a_message(
    tmp_path: Path, kind: str
) -> None:
    declaration = tmp_path / "test-units.json"
    if kind == "directory":
        declaration.mkdir()
    else:
        declaration.write_bytes(b'{"modules": "\xff"}')

    result = run_report("--repo", tmp_path, "--declaration", declaration)

    assert result.returncode == 2
    assert "invalid declaration" in result.stderr
    assert "Traceback" not in result.stderr


ADAPTERS_IN_MODULE = FIXTURES / "adapters-in-module"


def adapters_in_module_report() -> dict:
    result = run_report("--repo", ADAPTERS_IN_MODULE, "--json")
    assert result.returncode == 0, result.stderr
    return json.loads(result.stdout)


def test_contract_runs_of_adapters_inside_a_module_are_no_module_finding() -> None:
    records = module_entry(adapters_in_module_report(), "records")

    flagged = {f["test"] for f in records["findings"]}
    assert "tests/records-memory.contract.test.ts" not in flagged
    assert "tests/records-sqlite.contract.test.ts" not in flagged


def test_adapter_own_test_inside_a_module_is_no_module_finding() -> None:
    records = module_entry(adapters_in_module_report(), "records")

    assert "src/records/sqlite/client.test.ts" not in {
        f["test"] for f in records["findings"]
    }
    assert records["stated_exceptions"] == []


def test_caller_outside_the_module_importing_an_adapter_is_still_a_boundary_bypass() -> None:
    records = module_entry(adapters_in_module_report(), "records")

    assert [(f["kind"], f["test"], f["target"]) for f in records["findings"]] == [
        ("boundary-bypass", "tests/caller.test.ts", "src/records/memory.ts")
    ]


def test_port_rules_hold_for_adapters_inside_a_module() -> None:
    records = port_entry(adapters_in_module_report(), "records")

    assert records["findings"] == []
    runs = {adapter["name"]: adapter["contract_runs"] for adapter in records["adapters"]}
    assert runs == {
        "memory": ["tests/records-memory.contract.test.ts"],
        "sqlite": ["tests/records-sqlite.contract.test.ts"],
    }


def test_contract_file_importing_adapters_inside_a_module_is_no_module_finding(
    tmp_path: Path,
) -> None:
    path = write_repo(
        tmp_path,
        {
            "lib/store/index.ts": "",
            "lib/store/memory.ts": "",
            "lib/store/disk.ts": "",
            "ports/store/port.ts": "",
            "ports/store/contract.ts": (
                'import { Memory } from "../../lib/store/memory";\n'
                'import { Disk } from "../../lib/store/disk";\n'
            ),
        },
        {
            "modules": [module("store", "lib/store", "lib/store/index.ts")],
            "ports": [
                {
                    "name": "store",
                    "port": "ports/store/port.ts",
                    "contract": "ports/store/contract.ts",
                    "adapters": [
                        {"name": "memory", "path": "lib/store/memory.ts", "in_memory": True},
                        {"name": "disk", "path": "lib/store/disk.ts"},
                    ],
                }
            ],
        },
    )

    result = run_report("--repo", tmp_path, "--declaration", path, "--json")

    assert result.returncode == 0, result.stderr
    assert module_entry(json.loads(result.stdout), "store")["findings"] == []


def test_test_inside_one_adapter_importing_another_adapter_is_a_module_finding(
    tmp_path: Path,
) -> None:
    path = write_repo(
        tmp_path,
        {
            "lib/store/index.ts": "",
            "lib/store/memory.ts": "",
            "lib/store/disk/index.ts": "",
            "lib/store/disk/disk.test.ts": 'import { Memory } from "../memory";\n',
            "ports/store/port.ts": "",
            "ports/store/contract.ts": "",
        },
        {
            "modules": [module("store", "lib/store", "lib/store/index.ts")],
            "ports": [
                {
                    "name": "store",
                    "port": "ports/store/port.ts",
                    "contract": "ports/store/contract.ts",
                    "adapters": [
                        {"name": "memory", "path": "lib/store/memory.ts", "in_memory": True},
                        {"name": "disk", "path": "lib/store/disk"},
                    ],
                }
            ],
        },
    )

    result = run_report("--repo", tmp_path, "--declaration", path, "--json")

    assert result.returncode == 0, result.stderr
    findings = module_entry(json.loads(result.stdout), "store")["findings"]
    assert [(f["kind"], f["test"]) for f in findings] == [
        ("internal-without-reason", "lib/store/disk/disk.test.ts")
    ]
