from __future__ import annotations

from dataclasses import dataclass
from datetime import date, datetime
from pathlib import Path
from typing import Any, Mapping, Optional
from urllib.parse import urlparse
from zoneinfo import ZoneInfo

from .errors import ProtocolError
from .pass_types import PASS_PREFERENCES
from .protocol import validate_start_has_no_secrets


COMMANDS = frozenset({"auth-check", "dry-run", "book"})
MODES = frozenset({"auto", "manual", "dry-run"})


@dataclass(frozen=True)
class ActionConfig:
    run_id: str
    command: str
    mode: str
    profile_dir: Path
    target_date: date
    timezone_name: str
    login_probe_url: str
    allowed_yodel_origins: frozenset[str]
    all_day_pass_url: Optional[str]
    half_day_pass_url: Optional[str]
    vehicle_keyword: str
    pass_order: tuple[str, ...]
    headless: bool
    browser_channel: Optional[str]
    executable_path: Optional[str]
    default_timeout_ms: int
    poll_deadline_seconds: float
    poll_min_seconds: float
    poll_max_seconds: float
    artifacts_dir: Optional[Path]
    release_at: Optional[datetime]
    auth_deadline_at: Optional[datetime]

    def allows_yodel_url(self, value: str) -> bool:
        try:
            return _canonical_https_origin(value) in self.allowed_yodel_origins
        except ValueError:
            return False

    @classmethod
    def from_start(cls, frame: Mapping[str, Any]) -> "ActionConfig":
        if frame.get("type") != "run.start":
            raise ProtocolError("first control frame must be run.start")
        validate_start_has_no_secrets(frame)
        config = frame.get("config")
        if not isinstance(config, Mapping):
            raise ProtocolError("run.start config must be an object")

        run_id = _required_text(frame, "run_id")
        command = _required_text(frame, "command")
        mode = _required_text(frame, "mode")
        if command not in COMMANDS:
            raise ProtocolError("unsupported action command")
        if mode not in MODES:
            raise ProtocolError("unsupported booking mode")
        if command == "dry-run":
            mode = "dry-run"
        elif command == "book" and mode not in {"auto", "manual"}:
            raise ProtocolError("book mode must be auto or manual")

        profile_dir = Path(_required_text(config, "profile_dir")).expanduser()
        if not profile_dir.is_absolute():
            raise ProtocolError("profile_dir must be absolute")
        target_date = _parse_date(_required_text(config, "target_date"))
        timezone_name = _required_text(config, "timezone")
        try:
            ZoneInfo(timezone_name)
        except Exception as exc:
            raise ProtocolError("config timezone is invalid") from exc

        raw_order = config.get("pass_order", [])
        if not isinstance(raw_order, list) or any(
            not isinstance(item, str) for item in raw_order
        ):
            raise ProtocolError("pass_order must be an array of pass keys")
        pass_order = tuple(raw_order)
        if len(set(pass_order)) != len(pass_order) or any(
            item not in PASS_PREFERENCES for item in pass_order
        ):
            raise ProtocolError(
                "pass_order contains duplicate or unsupported pass keys"
            )
        if command in {"dry-run", "book"} and not pass_order:
            raise ProtocolError("at least one pass is required")

        raw_origins = config.get("allowed_yodel_origins")
        if (
            not isinstance(raw_origins, list)
            or not raw_origins
            or any(not isinstance(item, str) for item in raw_origins)
        ):
            raise ProtocolError("allowed_yodel_origins must be a non-empty array")
        allowed_yodel_origins = frozenset(
            _validate_origin(item) for item in raw_origins
        )

        all_day_url = _optional_text(config, "all_day_pass_url")
        half_day_url = _optional_text(config, "half_day_pass_url")
        login_probe_url = (
            _optional_text(config, "login_probe_url") or all_day_url or half_day_url
        )
        if not login_probe_url:
            raise ProtocolError("login_probe_url or a pass URL is required")
        _validate_url(login_probe_url, "login_probe_url", allowed_yodel_origins)
        if all_day_url is not None:
            _validate_url(
                all_day_url, "all_day_pass_url", allowed_yodel_origins
            )
        elif "all_day" in pass_order:
            raise ProtocolError("all_day_pass_url is required for the selected action")
        if half_day_url is not None:
            _validate_url(
                half_day_url, "half_day_pass_url", allowed_yodel_origins
            )
        elif "afternoon" in pass_order or "morning" in pass_order:
            raise ProtocolError("half_day_pass_url is required for the selected action")

        vehicle_keyword = _required_text(config, "vehicle_keyword")
        headless = _optional_bool(config, "headless", False)
        default_timeout_ms = _bounded_number(
            config, "default_timeout_ms", 15_000, 1_000, 120_000
        )
        if int(default_timeout_ms) != default_timeout_ms:
            raise ProtocolError("default_timeout_ms must be a whole number")
        poll_deadline = _bounded_number(
            config, "poll_deadline_seconds", 120.0, 1.0, 900.0
        )
        poll_min = _bounded_number(config, "poll_min_seconds", 1.4, 0.05, 60.0)
        poll_max = _bounded_number(config, "poll_max_seconds", 3.6, 0.05, 60.0)
        if poll_min > poll_max:
            raise ProtocolError("poll_min_seconds cannot exceed poll_max_seconds")

        artifacts_value = _optional_text(config, "artifacts_dir")
        artifacts_dir = Path(artifacts_value).expanduser() if artifacts_value else None
        if artifacts_dir is not None and not artifacts_dir.is_absolute():
            raise ProtocolError("artifacts_dir must be absolute")

        release_value = _optional_text(config, "release_at")
        release_at = _parse_datetime(release_value) if release_value else None
        if release_at is not None and release_at.tzinfo is None:
            raise ProtocolError("release_at must include a UTC offset")
        auth_deadline_value = _optional_text(config, "auth_deadline_at")
        auth_deadline_at = (
            _parse_datetime(auth_deadline_value) if auth_deadline_value else None
        )
        if auth_deadline_at is not None and auth_deadline_at.tzinfo is None:
            raise ProtocolError("auth_deadline_at must include a UTC offset")
        if (
            release_at is not None
            and auth_deadline_at is not None
            and auth_deadline_at > release_at
        ):
            raise ProtocolError("auth_deadline_at cannot be after release_at")

        return cls(
            run_id=run_id,
            command=command,
            mode=mode,
            profile_dir=profile_dir,
            target_date=target_date,
            timezone_name=timezone_name,
            login_probe_url=login_probe_url,
            allowed_yodel_origins=allowed_yodel_origins,
            all_day_pass_url=all_day_url,
            half_day_pass_url=half_day_url,
            vehicle_keyword=vehicle_keyword,
            pass_order=pass_order,
            headless=headless,
            browser_channel=_optional_text(config, "browser_channel"),
            executable_path=_optional_text(config, "executable_path"),
            default_timeout_ms=int(default_timeout_ms),
            poll_deadline_seconds=float(poll_deadline),
            poll_min_seconds=float(poll_min),
            poll_max_seconds=float(poll_max),
            artifacts_dir=artifacts_dir,
            release_at=release_at,
            auth_deadline_at=auth_deadline_at,
        )


def _required_text(values: Mapping[str, Any], key: str) -> str:
    value = values.get(key)
    if not isinstance(value, str) or not value.strip():
        raise ProtocolError(f"{key} must be a non-empty string")
    return value.strip()


def _optional_text(values: Mapping[str, Any], key: str) -> Optional[str]:
    value = values.get(key)
    if value is None:
        return None
    if not isinstance(value, str):
        raise ProtocolError(f"{key} must be a string or null")
    return value.strip() or None


def _optional_bool(values: Mapping[str, Any], key: str, default: bool) -> bool:
    value = values.get(key, default)
    if not isinstance(value, bool):
        raise ProtocolError(f"{key} must be a boolean")
    return value


def _bounded_number(
    values: Mapping[str, Any],
    key: str,
    default: float,
    minimum: float,
    maximum: float,
) -> int | float:
    value = values.get(key, default)
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        raise ProtocolError(f"{key} must be a number")
    if not minimum <= value <= maximum:
        raise ProtocolError(f"{key} is outside the supported range")
    return value


def _parse_date(value: str) -> date:
    try:
        return date.fromisoformat(value)
    except ValueError as exc:
        raise ProtocolError("target_date must use YYYY-MM-DD") from exc


def _parse_datetime(value: str) -> datetime:
    normalized = value[:-1] + "+00:00" if value.endswith("Z") else value
    try:
        return datetime.fromisoformat(normalized)
    except ValueError as exc:
        raise ProtocolError("scheduled timestamps must use RFC3339") from exc


def _validate_url(
    value: Optional[str], key: str, allowed_origins: frozenset[str]
) -> None:
    if not value:
        raise ProtocolError(f"{key} is required for the selected action")
    try:
        origin = _canonical_https_origin(value)
    except ValueError as exc:
        raise ProtocolError(f"{key} must be an absolute HTTPS URL") from exc
    if origin not in allowed_origins:
        raise ProtocolError(f"{key} must use an approved Yodel origin")


def _validate_origin(value: str) -> str:
    parsed = urlparse(value.strip())
    if parsed.path or parsed.params or parsed.query or parsed.fragment:
        raise ProtocolError("allowed_yodel_origins contains an invalid origin")
    try:
        return _canonical_https_origin(value)
    except ValueError as exc:
        raise ProtocolError(
            "allowed_yodel_origins contains an invalid origin"
        ) from exc


def _canonical_https_origin(value: str) -> str:
    parsed = urlparse(value.strip())
    if (
        parsed.scheme.lower() != "https"
        or not parsed.netloc
        or parsed.username is not None
        or parsed.password is not None
    ):
        raise ValueError("invalid HTTPS URL")
    hostname = (parsed.hostname or "").lower().rstrip(".")
    if not hostname or "*" in hostname:
        raise ValueError("invalid HTTPS host")
    try:
        port = parsed.port
    except ValueError as exc:
        raise ValueError("invalid HTTPS port") from exc
    if port is not None and not 1 <= port <= 65535:
        raise ValueError("invalid HTTPS port")
    if ":" in hostname:
        hostname = f"[{hostname}]"
    if port is not None and port != 443:
        hostname = f"{hostname}:{port}"
    return f"https://{hostname}"
