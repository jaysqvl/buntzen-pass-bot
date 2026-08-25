#!/usr/bin/env python3
from __future__ import annotations

import ipaddress
import re
import subprocess
from pathlib import Path


ROOT = Path(__file__).resolve().parent.parent
SELF = Path(__file__).resolve().relative_to(ROOT).as_posix()
PRIVATE_NETWORKS = tuple(
    ipaddress.ip_network(cidr) for cidr in ("10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16")
)
IPV4 = re.compile(r"(?<![\d.])(?:\d{1,3}\.){3}\d{1,3}(?![\d.])")
NANP_PHONE = re.compile(
    r"(?<![A-Za-z0-9])"
    r"(?:\+?1[ \t.-]?)?"
    r"(?:\(\d{3}\)|\d{3})[ \t.-]?\d{3}[ \t.-]?\d{4}"
    r"(?![A-Za-z0-9])"
)
PRIVATE_HOME = re.compile(r"\b[a-z0-9-]+\.home\b", re.IGNORECASE)
LOCAL_TIMEZONE = re.compile(r"\bAmerica/[A-Za-z_+-]+\b")
ALLOWED_PRODUCT_TIMEZONES = {"America/Vancouver"}
MACOS_USER_PATH = re.compile(r"/Users/[^/\s]+")
EMAIL = re.compile(r"\b[A-Z0-9._%+-]+@([A-Z0-9.-]+\.[A-Z]{2,})\b", re.IGNORECASE)
ALLOWED_EMAIL_DOMAINS = {
    "example.com",
    "example.invalid",
    "example.test",
    "users.noreply.github.com",
}


def repository_files() -> list[Path]:
    result = subprocess.run(
        ["git", "ls-files", "--cached", "--others", "--exclude-standard", "-z"],
        cwd=ROOT,
        check=True,
        capture_output=True,
    )
    return [ROOT / name.decode() for name in result.stdout.split(b"\0") if name]


def read_text(path: Path) -> str | None:
    raw = path.read_bytes()
    if b"\0" in raw:
        return None
    try:
        return raw.decode("utf-8")
    except UnicodeDecodeError:
        return None


def findings_for(path: Path, text: str) -> set[str]:
    findings: set[str] = set()
    relative = path.relative_to(ROOT).as_posix() if path.is_relative_to(ROOT) else ""
    for candidate in IPV4.findall(text):
        try:
            address = ipaddress.ip_address(candidate)
        except ValueError:
            continue
        if any(address in network for network in PRIVATE_NETWORKS):
            findings.add("private IPv4 address")

    for candidate in NANP_PHONE.findall(text):
        # Dependency locks and the seccomp profile contain machine-generated,
        # standalone 10-digit integers. Keep detecting formatted numbers in
        # those files, but do not mistake an uninterrupted generated integer
        # for a phone number.
        generated_numeric_data = path.suffix == ".lock" or relative == "docker/seccomp_profile.json"
        formatted = any(character in candidate for character in "+().- ")
        if generated_numeric_data and not formatted:
            continue
        digits = "".join(character for character in candidate if character.isdigit())
        national = digits[1:] if len(digits) == 11 and digits.startswith("1") else digits
        if len(national) == 10 and not national.startswith("555"):
            findings.add("non-fictional North American phone number")

    if PRIVATE_HOME.search(text):
        findings.add("private .home hostname")
    if any(
        match.group(0) not in ALLOWED_PRODUCT_TIMEZONES
        for match in LOCAL_TIMEZONE.finditer(text)
    ):
        findings.add("deployment-local timezone")
    if MACOS_USER_PATH.search(text):
        findings.add("personal macOS path")
    if any(match.group(1).lower() not in ALLOWED_EMAIL_DOMAINS for match in EMAIL.finditer(text)):
        findings.add("non-example email address")

    if relative == ".env.example" and re.search(r"(?m)^WEB_PORT=(?!8080$)\d+$", text):
        findings.add("deployment-specific example port")
    if relative == "docker-compose.yml" and re.search(r"\$\{WEB_PORT:-(?!8080})\d+}", text):
        findings.add("deployment-specific Compose port")
    return findings


def main() -> int:
    for sample in ("000-123-4567", "(000) 123-4567", "+1 000-123-4567"):
        if "non-fictional North American phone number" not in findings_for(ROOT / "fixture", sample):
            raise RuntimeError("privacy guard failed its bare-phone-number self-check")
    if findings_for(ROOT / "fixture", "+1 555-010-0123"):
        raise RuntimeError("privacy guard rejected its fictional phone-number self-check")
    for sample in (
        "release 0.3.0 (2026-08-25)",
        "commit/0211639260c7e67ebd9fbd4b2771a1506bbe62a9",
    ):
        if findings_for(ROOT / "fixture", sample):
            raise RuntimeError("privacy guard mistook release metadata for a phone number")
    if findings_for(ROOT / "fixture", "America/Vancouver"):
        raise RuntimeError("privacy guard rejected Buntzen's public local timezone")
    if "deployment-local timezone" not in findings_for(
        ROOT / "fixture", "America/Example_City"
    ):
        raise RuntimeError("privacy guard failed its unrelated timezone self-check")

    failures: dict[str, set[str]] = {}
    for path in repository_files():
        relative = path.relative_to(ROOT).as_posix()
        if relative == SELF:
            continue
        if path.name == ".env" or path.suffix.lower() in {".db", ".sqlite", ".sqlite3", ".pem", ".key"}:
            failures.setdefault(relative, set()).add("sensitive file type")
            continue
        text = read_text(path)
        if text is None:
            continue
        findings = findings_for(path, text)
        if findings:
            failures[relative] = findings

    for relative, findings in sorted(failures.items()):
        print(f"privacy guard: {relative}: {', '.join(sorted(findings))}")
    return int(bool(failures))


if __name__ == "__main__":
    raise SystemExit(main())
