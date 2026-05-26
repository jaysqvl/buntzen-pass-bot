from __future__ import annotations

from datetime import datetime
from pathlib import Path

from src.env_utils import BotConfig, _validate

from .db import Instance
from .settings import profiles_dir


def build_config(instance: Instance, command: str) -> BotConfig:
    config = BotConfig(
        user_data_dir=profiles_dir() / instance.profile_name,
        target_date=datetime.strptime(instance.target_date, "%Y-%m-%d").date(),
        schedule=True,
        slow_poll_until=datetime.strptime("06:59", "%H:%M").time(),
        start_time=datetime.strptime(instance.start_time, "%H:%M").time(),
        prep_minutes_before=instance.prep_minutes_before,
        auth_deadline_minutes_before=instance.auth_deadline_minutes_before,
        poll_deadline_seconds=instance.poll_deadline_seconds,
        poll_min_seconds=instance.poll_min_seconds,
        poll_max_seconds=instance.poll_max_seconds,
        run_mode=instance.run_mode,
        headless=instance.headless,
        browser_channel=instance.browser_channel or None,
        default_timeout_ms=15000,
        timezone_name=instance.timezone_name,
        all_day_pass_url=instance.all_day_pass_url,
        half_day_pass_url=instance.half_day_pass_url,
        vehicle_keyword=instance.vehicle_keyword,
        check_all_day=instance.check_all_day,
        check_morning=instance.check_morning,
        check_afternoon=instance.check_afternoon,
        yodel_email=instance.yodel_email or None,
        yodel_password=instance.yodel_password or None,
        twilio_account_sid=instance.twilio_account_sid,
        twilio_auth_token=instance.twilio_auth_token,
        twilio_otp_number=instance.twilio_otp_number,
        twilio_alert_to_number=instance.twilio_alert_to_number or None,
        twilio_alerts_enabled=instance.twilio_alerts_enabled,
        otp_timeout_seconds=instance.otp_timeout_seconds,
        otp_poll_interval_seconds=instance.otp_poll_interval_seconds,
    )
    Path(config.user_data_dir).mkdir(parents=True, exist_ok=True)
    _validate(config, command=command)
    return config
