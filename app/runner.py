from __future__ import annotations

import logging
import queue
import threading
import os
from contextlib import contextmanager
from dataclasses import replace
from datetime import datetime
from pathlib import Path

from playwright.sync_api import sync_playwright

from run import open_persistent_context, run_auth_check, run_book, run_dry_run
from src.booking import BookingError, BookingBot
from src.diagnostics import Diagnostics
from src.twilio_utils import TwilioService

from . import db
from .config_builder import build_config
from .settings import artifacts_dir, max_concurrent_jobs


logger = logging.getLogger("buntzen_pass_bot.app.runner")


class WorkerPool:
    def __init__(self) -> None:
        self.queue: queue.Queue[int] = queue.Queue()
        self.started = False
        self.workers: list[threading.Thread] = []

    def start(self) -> None:
        if self.started:
            return
        self.started = True
        for index in range(max_concurrent_jobs()):
            thread = threading.Thread(target=self._worker, name=f"booking-worker-{index + 1}", daemon=True)
            thread.start()
            self.workers.append(thread)

    def enqueue(self, job_id: int) -> None:
        self.start()
        self.queue.put(job_id)

    def _worker(self) -> None:
        while True:
            job_id = self.queue.get()
            try:
                run_job(job_id)
            except Exception:
                logger.exception("Unhandled job failure for job %s", job_id)
                db.update_job(
                    job_id,
                    status="failed",
                    message="Unhandled worker failure. Check application logs.",
                    finished_at=db.utc_now(),
                    exit_code=99,
                )
            finally:
                self.queue.task_done()


worker_pool = WorkerPool()


def enqueue_job(instance_id: int, command: str, run_mode: str | None = None) -> int:
    instance = db.get_instance(instance_id)
    if instance is None:
        raise ValueError(f"Instance {instance_id} does not exist.")
    if command == "book" and db.active_job_exists(instance_id):
        raise ValueError("This instance already has a queued or running booking job.")
    chosen_mode = "dry-run" if command == "dry-run" else (run_mode or instance.run_mode)
    job_id = db.create_job(instance_id, command=command, run_mode=chosen_mode, target_date=instance.target_date)
    worker_pool.enqueue(job_id)
    return job_id


def run_job(job_id: int) -> None:
    os.environ.setdefault("APPDATA_DIR", str(Path("appdata").resolve()))
    job = db.get_job(job_id)
    if job is None:
        return
    instance = db.get_instance(job.instance_id)
    if instance is None:
        db.update_job(job_id, status="failed", message="Instance was deleted.", finished_at=db.utc_now(), exit_code=1)
        return

    job_dir = artifacts_dir() / f"job-{job_id}"
    job_dir.mkdir(parents=True, exist_ok=True)
    log_path = job_dir / "job.log"
    db.update_job(
        job_id,
        status="running",
        started_at=db.utc_now(),
        log_path=str(log_path),
        artifact_dir=str(job_dir),
        message="Job started.",
    )

    exit_code = 1
    message = "Job did not finish."
    try:
        with job_logging(log_path):
            logging.info("Starting %s job %s for %s", job.command, job_id, instance.name)
            config = build_config(instance, command=job.command)
            if job.command == "dry-run":
                config = replace(config, run_mode="dry-run")
            elif job.run_mode != config.run_mode:
                config = replace(config, run_mode=job.run_mode)

            diagnostics = Diagnostics(base_dir=job_dir)
            twilio = TwilioService.from_config(config)
            with sync_playwright() as playwright:
                context = open_persistent_context(playwright, config)
                context.set_default_timeout(config.default_timeout_ms)
                context.tracing.start(screenshots=True, snapshots=True, sources=True)
                page = context.pages[0] if context.pages else context.new_page()
                bot = BookingBot(
                    page=page,
                    context=context,
                    config=config,
                    twilio=twilio,
                    diagnostics=diagnostics,
                    interactive_manual=False,
                )
                try:
                    try:
                        if job.command == "auth-check":
                            exit_code = run_auth_check(bot)
                        elif job.command == "dry-run":
                            exit_code = run_dry_run(bot)
                        elif job.command == "book":
                            exit_code = run_book(bot)
                        else:
                            raise ValueError(f"Unknown job command: {job.command}")
                    except BookingError as exc:
                        logging.error("Booking error: %s", exc)
                        bot.capture_failure("booking-error")
                        exit_code = 5
                    finally:
                        trace_path = diagnostics.path_for("trace", "zip")
                        try:
                            context.tracing.stop(path=str(trace_path))
                        except Exception as exc:
                            logging.warning("Could not save trace: %s", exc)
                finally:
                    context.close()

            message = "Job completed." if exit_code == 0 else f"Job exited with code {exit_code}."
    except Exception as exc:
        logger.exception("Job %s failed", job_id)
        message = str(exc)
        exit_code = 1

    db.update_job(
        job_id,
        status="succeeded" if exit_code == 0 else "failed",
        message=message,
        finished_at=db.utc_now(),
        exit_code=exit_code,
    )


@contextmanager
def job_logging(path: Path):
    path.parent.mkdir(parents=True, exist_ok=True)
    handler = logging.FileHandler(path, encoding="utf-8")
    handler.setFormatter(logging.Formatter("%(asctime)s %(levelname)s %(name)s: %(message)s"))
    root = logging.getLogger()
    previous_level = root.level
    root.setLevel(logging.INFO)
    root.addHandler(handler)
    try:
        yield
    finally:
        root.removeHandler(handler)
        root.setLevel(previous_level)
        handler.close()


class Scheduler:
    def __init__(self) -> None:
        self.thread: threading.Thread | None = None
        self.stop_event = threading.Event()

    def start(self) -> None:
        worker_pool.start()
        if self.thread and self.thread.is_alive():
            return
        self.thread = threading.Thread(target=self._loop, name="booking-scheduler", daemon=True)
        self.thread.start()

    def _loop(self) -> None:
        while not self.stop_event.is_set():
            self.check_once()
            self.stop_event.wait(20)

    def check_once(self) -> None:
        for instance in db.list_instances():
            if not instance.enabled or not instance.schedule_enabled:
                continue
            try:
                config = build_config(instance, command="book")
            except Exception as exc:
                logger.warning("Skipping invalid scheduled instance %s: %s", instance.name, exc)
                continue
            now = datetime.now(config.timezone)
            release_window_end = config.release_at.timestamp() + config.poll_deadline_seconds + 300
            if now >= config.prep_at and now.timestamp() <= release_window_end:
                if not db.scheduled_job_exists(instance.id, instance.target_date):
                    logger.info("Queueing scheduled booking for %s", instance.name)
                    enqueue_job(instance.id, "book", run_mode=instance.run_mode)


scheduler = Scheduler()
