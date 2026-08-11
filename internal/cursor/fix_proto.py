"""Prepare agent.proto for Go codegen by removing colliding flattened enums."""

from __future__ import annotations

import re
from pathlib import Path

proto_path = Path(__file__).with_name("proto_src") / "agent" / "v1" / "agent.proto"
text = proto_path.read_text(encoding="utf-8")

if "go_package" not in text:
    text = text.replace(
        "package agent.v1;",
        'package agent.v1;\n\noption go_package = "github.com/router-for-me/CLIProxyAPI/v7/internal/cursor/proto/agent/v1;agentv1";',
        1,
    )

nested_names: set[str] = set()
for match in re.finditer(r"message\s+(\w+)\s*\{", text):
    msg = match.group(1)
    start = match.end()
    depth = 1
    i = start
    while i < len(text) and depth > 0:
        if text[i] == "{":
            depth += 1
        elif text[i] == "}":
            depth -= 1
        i += 1
    body = text[start : i - 1]
    for enum_match in re.finditer(r"(?m)^\s*enum\s+(\w+)\s*\{", body):
        nested_names.add(f"{msg}_{enum_match.group(1)}")

removed: list[str] = []


def replacer(match: re.Match[str]) -> str:
    name = match.group(1)
    if name in nested_names:
        removed.append(name)
        return ""
    return match.group(0)


# Only strip top-level enum blocks (column 0).
pattern = re.compile(r"(?ms)^enum\s+(\w+)\s*\{.*?\n\}\n*")
new_text = pattern.sub(replacer, text)
proto_path.write_text(new_text, encoding="utf-8", newline="\n")
print("removed", len(removed), "top-level enums")
for name in removed:
    print(" -", name)
