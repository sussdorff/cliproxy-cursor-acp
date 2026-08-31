#!/usr/bin/env python3
"""Install Claude Code, Codex, and Grok client routing for CLIProxyAPI."""

from __future__ import annotations

import argparse
import json
import shutil
from pathlib import Path

DEFAULT_GATEWAY_URL = "http://192.168.60.32:8317"
PROVIDER_HEADER = "[model_providers.cliproxy]"
SOURCE_LINE = 'test -r "$HOME/.config/cliproxy/env.sh" && . "$HOME/.config/cliproxy/env.sh"'
SOURCE_BLOCK = (
    "# CLIProxyAPI client for Claude Code / Codex / Grok. Cursor Agent stays native.\n"
    f"{SOURCE_LINE}\n"
)


def die(message: str) -> None:
    raise SystemExit(message)


def read_key(path: Path) -> str:
    key = path.read_text().strip()
    if not key:
        die(f"client key file is empty: {path}")
    return key


def key_mode(path: Path) -> str:
    return oct(path.stat().st_mode)[-3:]


def render(template: str, replacements: dict[str, str]) -> str:
    text = template
    for needle, value in replacements.items():
        text = text.replace(needle, value)
    return text


def write_mode(path: Path, content: str, mode: int) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(content)
    path.chmod(mode)


def install_key(src: Path, dest: Path) -> None:
    dest.parent.mkdir(parents=True, exist_ok=True)
    if src.resolve() != dest.resolve():
        shutil.copyfile(src, dest)
    dest.chmod(0o600)


def ensure_source(path: Path) -> None:
    if not path.exists():
        write_mode(path, SOURCE_BLOCK, 0o644)
        return
    text = path.read_text()
    if "cliproxy/env.sh" in text:
        return
    if not text.endswith("\n"):
        text += "\n"
    path.write_text(text + "\n" + SOURCE_BLOCK)


def insert_bashrc_source(path: Path) -> None:
    if not path.exists():
        write_mode(path, SOURCE_BLOCK, 0o644)
        return
    text = path.read_text()
    if "cliproxy/env.sh" in text:
        return
    needle = "# If not running interactively, don't do anything\n"
    if needle in text:
        path.write_text(text.replace(needle, SOURCE_BLOCK + "\n" + needle, 1))
        return
    if not text.endswith("\n"):
        text += "\n"
    path.write_text(text + "\n" + SOURCE_BLOCK)


def merge_claude_settings(path: Path, helper: str, gateway_url: str) -> None:
    data: dict[str, object] = {}
    if path.exists():
        loaded = json.loads(path.read_text() or "{}")
        if not isinstance(loaded, dict):
            die(f"Claude settings are not a JSON object: {path}")
        data = loaded
    env = data.get("env")
    if not isinstance(env, dict):
        env = {}
    env["ANTHROPIC_BASE_URL"] = gateway_url
    data["apiKeyHelper"] = helper
    data["env"] = env
    write_mode(path, json.dumps(data, indent=2) + "\n", 0o600)


def replace_or_append_block(text: str, header: str, block: str) -> str:
    if header in text:
        start = text.index(header)
        rest = text[start + len(header) :]
        next_header = rest.find("\n[")
        end = start + len(header) + (len(rest) if next_header < 0 else next_header)
        return text[:start].rstrip() + "\n" + block + text[end:]
    if text and not text.endswith("\n"):
        text += "\n"
    return text + "\n" + block


def patch_codex(path: Path, provider_block: str, key: str) -> None:
    text = path.read_text() if path.exists() else ""
    if 'model_provider = "cliproxy"' not in text:
        lines = text.splitlines(keepends=True)
        inserted = False
        out: list[str] = []
        for line in lines:
            out.append(line)
            if not inserted and line.startswith("model = "):
                out.append('model_provider = "cliproxy"\n')
                inserted = True
        if not inserted:
            out.insert(0, 'model_provider = "cliproxy"\n')
        text = "".join(out)
    block = provider_block.rstrip() + f'\nexperimental_bearer_token = "{key}"\n'
    text = replace_or_append_block(text, PROVIDER_HEADER, block)
    write_mode(path, text if text.endswith("\n") else text + "\n", 0o600)


def upsert_toml_section(text: str, header: str, section: str) -> str:
    if header in text:
        start = text.index(header)
        rest = text[start + len(header) :]
        next_header = rest.find("\n[")
        end = start + len(header) + (len(rest) if next_header < 0 else next_header)
        return text[:start].rstrip() + "\n" + section + text[end:]
    if text and not text.endswith("\n"):
        text += "\n"
    return text + "\n" + section


def patch_grok(path: Path, grok_template: str) -> None:
    text = path.read_text() if path.exists() else ""
    sections = {
        "[endpoints]": _extract_section(grok_template, "[endpoints]"),
        "[models]": _extract_section(grok_template, "[models]"),
        '[model."grok-4.6"]': _extract_section(grok_template, '[model."grok-4.6"]'),
        '[model."grok-4.5"]': _extract_section(grok_template, '[model."grok-4.5"]'),
    }
    for header, section in sections.items():
        text = upsert_toml_section(text, header, section)
    write_mode(path, text if text.endswith("\n") else text + "\n", 0o600)


def _extract_section(template: str, header: str) -> str:
    start = template.index(header)
    rest = template[start + len(header) :]
    next_header = rest.find("\n[")
    end = start + len(header) + (len(rest) if next_header < 0 else next_header)
    section = template[start:end].strip() + "\n"
    return section


def apply(
    *,
    home: Path,
    templates: Path,
    gateway_url: str,
    key_file: Path,
    client_name: str,
) -> None:
    if not key_file.is_file():
        die(f"client key file does not exist: {key_file}")
    if key_mode(key_file) != "600":
        die(f"client key file must have mode 0600: {key_file}")
    key = read_key(key_file)
    dest_key_name = f"{client_name}.key"
    dest_key = home / ".config" / "cliproxy" / dest_key_name
    helper = str(home / ".config" / "cliproxy" / "print-key.sh")
    replacements = {
        "GATEWAY_URL": gateway_url.rstrip("/"),
        "KEY_FILE": dest_key_name,
        "API_KEY_HELPER": helper,
    }

    install_key(key_file, dest_key)
    write_mode(
        home / ".config" / "cliproxy" / "env.sh",
        render((templates / "env.sh").read_text(), replacements),
        0o600,
    )
    write_mode(
        home / ".config" / "cliproxy" / "print-key.sh",
        render((templates / "print-key.sh").read_text(), replacements),
        0o700,
    )
    merge_claude_settings(
        home / ".claude" / "settings.local.json",
        helper,
        replacements["GATEWAY_URL"],
    )
    patch_codex(
        home / ".codex" / "config.toml",
        render((templates / "codex.provider.toml").read_text(), replacements),
        key,
    )
    patch_grok(
        home / ".grok" / "config.toml",
        render((templates / "grok.cliproxy.toml").read_text(), replacements),
    )
    insert_bashrc_source(home / ".bashrc")
    ensure_source(home / ".profile")
    ensure_source(home / ".zshenv")
    print("applied CLIProxyAPI client routing")


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--home", default=str(Path.home()))
    parser.add_argument("--templates", required=True)
    parser.add_argument("--gateway-url", default=DEFAULT_GATEWAY_URL)
    parser.add_argument("--key-file", required=True)
    parser.add_argument("--client-name", required=True)
    args = parser.parse_args()
    apply(
        home=Path(args.home).expanduser(),
        templates=Path(args.templates).expanduser(),
        gateway_url=args.gateway_url,
        key_file=Path(args.key_file).expanduser(),
        client_name=args.client_name,
    )


if __name__ == "__main__":
    main()
