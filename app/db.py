from __future__ import annotations

import json
import sqlite3
import threading
from dataclasses import dataclass
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

from .settings import database_path


_LOCK = threading.RLock()


def utc_now() -> str:
    return datetime.now(timezone.utc).isoformat()


@dataclass(frozen=True)
class Instance:
    id: int
    name: str
    enabled: bool
    schedule_enabled: bool
    profile_name: str
    target_date: str
    start_time: str
    timezone_name: str
    run_mode: str
    headless: bool
    browser_channel: str
    all_day_pass_url: str
    half_day_pass_url: str
    vehicle_keyword: str
    check_all_day: bool
    check_morning: bool
    check_afternoon: bool
    yodel_email: str
    yodel_password: str
    twilio_account_sid: str
    twilio_auth_token: str
    twilio_otp_number: str
    twilio_alert_to_number: str
    twilio_alerts_enabled: bool
    prep_minutes_before: int
    auth_deadline_minutes_before: int
    poll_deadline_seconds: int
    poll_min_seconds: float
    poll_max_seconds: float
    otp_timeout_seconds: int
    otp_poll_interval_seconds: float
    created_at: str
    updated_at: str


@dataclass(frozen=True)
class Job:
    id: int
    instance_id: int
    instance_name: str
    command: str
    run_mode: str
    status: str
    target_date: str
    message: str
    log_path: str
    artifact_dir: str
    created_at: str
    started_at: str | None
    finished_at: str | None
    exit_code: int | None


INSTANCE_FIELDS = (
    "id",
    "name",
    "enabled",
    "schedule_enabled",
    "profile_name",
    "target_date",
    "start_time",
    "timezone_name",
    "run_mode",
    "headless",
    "browser_channel",
    "all_day_pass_url",
    "half_day_pass_url",
    "vehicle_keyword",
    "check_all_day",
    "check_morning",
    "check_afternoon",
    "yodel_email",
    "yodel_password",
    "twilio_account_sid",
    "twilio_auth_token",
    "twilio_otp_number",
    "twilio_alert_to_number",
    "twilio_alerts_enabled",
    "prep_minutes_before",
    "auth_deadline_minutes_before",
    "poll_deadline_seconds",
    "poll_min_seconds",
    "poll_max_seconds",
    "otp_timeout_seconds",
    "otp_poll_interval_seconds",
    "created_at",
    "updated_at",
)


DEFAULT_INSTANCE = {
    "enabled": 1,
    "schedule_enabled": 0,
    "profile_name": "",
    "target_date": "2026-06-18",
    "start_time": "07:00",
    "timezone_name": "America/Vancouver",
    "run_mode": "manual",
    "headless": 1,
    "browser_channel": "",
    "all_day_pass_url": "https://yodelportal.com/buntzen-lake",
    "half_day_pass_url": "https://yodelportal.com/buntzen-lake",
    "vehicle_keyword": "",
    "check_all_day": 1,
    "check_morning": 0,
    "check_afternoon": 0,
    "yodel_email": "",
    "yodel_password": "",
    "twilio_account_sid": "",
    "twilio_auth_token": "",
    "twilio_otp_number": "",
    "twilio_alert_to_number": "",
    "twilio_alerts_enabled": 1,
    "prep_minutes_before": 30,
    "auth_deadline_minutes_before": 5,
    "poll_deadline_seconds": 120,
    "poll_min_seconds": 1.4,
    "poll_max_seconds": 3.6,
    "otp_timeout_seconds": 120,
    "otp_poll_interval_seconds": 3.0,
}


def connect() -> sqlite3.Connection:
    path = database_path()
    path.parent.mkdir(parents=True, exist_ok=True)
    conn = sqlite3.connect(path, check_same_thread=False)
    conn.row_factory = sqlite3.Row
    conn.execute("PRAGMA journal_mode=WAL")
    conn.execute("PRAGMA foreign_keys=ON")
    return conn


def init_db() -> None:
    with _LOCK, connect() as conn:
        conn.executescript(
            """
            CREATE TABLE IF NOT EXISTS instances (
                id INTEGER PRIMARY KEY AUTOINCREMENT,
                name TEXT NOT NULL UNIQUE,
                enabled INTEGER NOT NULL DEFAULT 1,
                schedule_enabled INTEGER NOT NULL DEFAULT 0,
                profile_name TEXT NOT NULL,
                target_date TEXT NOT NULL,
                start_time TEXT NOT NULL,
                timezone_name TEXT NOT NULL,
                run_mode TEXT NOT NULL,
                headless INTEGER NOT NULL DEFAULT 1,
                browser_channel TEXT NOT NULL DEFAULT '',
                all_day_pass_url TEXT NOT NULL,
                half_day_pass_url TEXT NOT NULL,
                vehicle_keyword TEXT NOT NULL,
                check_all_day INTEGER NOT NULL DEFAULT 1,
                check_morning INTEGER NOT NULL DEFAULT 0,
                check_afternoon INTEGER NOT NULL DEFAULT 0,
                yodel_email TEXT NOT NULL DEFAULT '',
                yodel_password TEXT NOT NULL DEFAULT '',
                twilio_account_sid TEXT NOT NULL DEFAULT '',
                twilio_auth_token TEXT NOT NULL DEFAULT '',
                twilio_otp_number TEXT NOT NULL DEFAULT '',
                twilio_alert_to_number TEXT NOT NULL DEFAULT '',
                twilio_alerts_enabled INTEGER NOT NULL DEFAULT 1,
                prep_minutes_before INTEGER NOT NULL DEFAULT 30,
                auth_deadline_minutes_before INTEGER NOT NULL DEFAULT 5,
                poll_deadline_seconds INTEGER NOT NULL DEFAULT 120,
                poll_min_seconds REAL NOT NULL DEFAULT 1.4,
                poll_max_seconds REAL NOT NULL DEFAULT 3.6,
                otp_timeout_seconds INTEGER NOT NULL DEFAULT 120,
                otp_poll_interval_seconds REAL NOT NULL DEFAULT 3.0,
                created_at TEXT NOT NULL,
                updated_at TEXT NOT NULL
            );

            CREATE TABLE IF NOT EXISTS jobs (
                id INTEGER PRIMARY KEY AUTOINCREMENT,
                instance_id INTEGER NOT NULL,
                command TEXT NOT NULL,
                run_mode TEXT NOT NULL,
                status TEXT NOT NULL,
                target_date TEXT NOT NULL,
                message TEXT NOT NULL DEFAULT '',
                log_path TEXT NOT NULL DEFAULT '',
                artifact_dir TEXT NOT NULL DEFAULT '',
                created_at TEXT NOT NULL,
                started_at TEXT,
                finished_at TEXT,
                exit_code INTEGER,
                metadata_json TEXT NOT NULL DEFAULT '{}',
                FOREIGN KEY(instance_id) REFERENCES instances(id) ON DELETE CASCADE
            );
            """
        )


def instance_from_row(row: sqlite3.Row) -> Instance:
    data = {field: row[field] for field in INSTANCE_FIELDS}
    for key in (
        "enabled",
        "schedule_enabled",
        "headless",
        "check_all_day",
        "check_morning",
        "check_afternoon",
        "twilio_alerts_enabled",
    ):
        data[key] = bool(data[key])
    return Instance(**data)


def job_from_row(row: sqlite3.Row) -> Job:
    return Job(
        id=row["id"],
        instance_id=row["instance_id"],
        instance_name=row["instance_name"],
        command=row["command"],
        run_mode=row["run_mode"],
        status=row["status"],
        target_date=row["target_date"],
        message=row["message"],
        log_path=row["log_path"],
        artifact_dir=row["artifact_dir"],
        created_at=row["created_at"],
        started_at=row["started_at"],
        finished_at=row["finished_at"],
        exit_code=row["exit_code"],
    )


def list_instances() -> list[Instance]:
    with _LOCK, connect() as conn:
        rows = conn.execute("SELECT * FROM instances ORDER BY name").fetchall()
    return [instance_from_row(row) for row in rows]


def get_instance(instance_id: int) -> Instance | None:
    with _LOCK, connect() as conn:
        row = conn.execute("SELECT * FROM instances WHERE id = ?", (instance_id,)).fetchone()
    return instance_from_row(row) if row else None


def save_instance(values: dict[str, Any], instance_id: int | None = None) -> int:
    now = utc_now()
    data = dict(DEFAULT_INSTANCE)
    data.update(values)
    if not data.get("profile_name"):
        data["profile_name"] = slugify(data["name"])

    bool_fields = {
        "enabled",
        "schedule_enabled",
        "headless",
        "check_all_day",
        "check_morning",
        "check_afternoon",
        "twilio_alerts_enabled",
    }
    for field in bool_fields:
        data[field] = 1 if truthy(data.get(field)) else 0

    mutable_fields = [field for field in INSTANCE_FIELDS if field not in {"id", "created_at", "updated_at"}]
    with _LOCK, connect() as conn:
        if instance_id:
            assignments = ", ".join(f"{field} = ?" for field in mutable_fields)
            params = [data.get(field, "") for field in mutable_fields]
            params.extend([now, instance_id])
            conn.execute(
                f"UPDATE instances SET {assignments}, updated_at = ? WHERE id = ?",
                params,
            )
            return instance_id

        fields = mutable_fields + ["created_at", "updated_at"]
        placeholders = ", ".join("?" for _ in fields)
        params = [data.get(field, "") for field in mutable_fields] + [now, now]
        cursor = conn.execute(
            f"INSERT INTO instances ({', '.join(fields)}) VALUES ({placeholders})",
            params,
        )
        return int(cursor.lastrowid)


def delete_instance(instance_id: int) -> None:
    with _LOCK, connect() as conn:
        conn.execute("DELETE FROM instances WHERE id = ?", (instance_id,))


def create_job(instance_id: int, command: str, run_mode: str, target_date: str) -> int:
    now = utc_now()
    with _LOCK, connect() as conn:
        cursor = conn.execute(
            """
            INSERT INTO jobs (instance_id, command, run_mode, status, target_date, created_at)
            VALUES (?, ?, ?, 'queued', ?, ?)
            """,
            (instance_id, command, run_mode, target_date, now),
        )
        return int(cursor.lastrowid)


def update_job(job_id: int, **values: Any) -> None:
    if not values:
        return
    fields = ", ".join(f"{key} = ?" for key in values)
    with _LOCK, connect() as conn:
        conn.execute(
            f"UPDATE jobs SET {fields} WHERE id = ?",
            [*values.values(), job_id],
        )


def get_job(job_id: int) -> Job | None:
    with _LOCK, connect() as conn:
        row = conn.execute(
            """
            SELECT jobs.*, instances.name AS instance_name
            FROM jobs
            JOIN instances ON instances.id = jobs.instance_id
            WHERE jobs.id = ?
            """,
            (job_id,),
        ).fetchone()
    return job_from_row(row) if row else None


def list_jobs(limit: int = 50) -> list[Job]:
    with _LOCK, connect() as conn:
        rows = conn.execute(
            """
            SELECT jobs.*, instances.name AS instance_name
            FROM jobs
            JOIN instances ON instances.id = jobs.instance_id
            ORDER BY jobs.id DESC
            LIMIT ?
            """,
            (limit,),
        ).fetchall()
    return [job_from_row(row) for row in rows]


def active_job_exists(instance_id: int) -> bool:
    with _LOCK, connect() as conn:
        row = conn.execute(
            """
            SELECT 1 FROM jobs
            WHERE instance_id = ?
              AND command = 'book'
              AND status IN ('queued', 'running')
            LIMIT 1
            """,
            (instance_id,),
        ).fetchone()
    return row is not None


def scheduled_job_exists(instance_id: int, target_date: str) -> bool:
    with _LOCK, connect() as conn:
        row = conn.execute(
            """
            SELECT 1 FROM jobs
            WHERE instance_id = ?
              AND command = 'book'
              AND target_date = ?
              AND status IN ('queued', 'running', 'succeeded')
            LIMIT 1
            """,
            (instance_id, target_date),
        ).fetchone()
    return row is not None


def append_job_metadata(job_id: int, values: dict[str, Any]) -> None:
    with _LOCK, connect() as conn:
        row = conn.execute("SELECT metadata_json FROM jobs WHERE id = ?", (job_id,)).fetchone()
        metadata = json.loads(row["metadata_json"] if row else "{}")
        metadata.update(values)
        conn.execute("UPDATE jobs SET metadata_json = ? WHERE id = ?", (json.dumps(metadata), job_id))


def truthy(value: Any) -> bool:
    if isinstance(value, bool):
        return value
    if value is None:
        return False
    return str(value).lower() in {"1", "true", "yes", "on"}


def slugify(value: str) -> str:
    slug = "".join(ch.lower() if ch.isalnum() else "-" for ch in value.strip())
    slug = "-".join(part for part in slug.split("-") if part)
    return slug or "profile"


def db_location() -> Path:
    return database_path()
