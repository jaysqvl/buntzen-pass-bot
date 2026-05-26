from __future__ import annotations

from dataclasses import asdict
from pathlib import Path
from urllib.parse import parse_qs

from fastapi import FastAPI, HTTPException, Request
from fastapi.responses import PlainTextResponse, RedirectResponse
from fastapi.staticfiles import StaticFiles
from fastapi.templating import Jinja2Templates

from . import db
from .db import DEFAULT_INSTANCE
from .runner import enqueue_job, scheduler


BASE_DIR = Path(__file__).resolve().parent
TEMPLATES_DIR = BASE_DIR / "templates"
STATIC_DIR = BASE_DIR / "static"

app = FastAPI(title="Buntzen Pass Bot")
app.mount("/static", StaticFiles(directory=STATIC_DIR), name="static")

templates = Jinja2Templates(directory=TEMPLATES_DIR)


@app.on_event("startup")
def startup() -> None:
    db.init_db()
    scheduler.start()


@app.get("/")
def index(request: Request):
    instances = db.list_instances()
    jobs = db.list_jobs(limit=8)
    active_jobs = [job for job in jobs if job.status in {"queued", "running"}]
    scheduled_instances = [item for item in instances if item.enabled and item.schedule_enabled]
    return render(
        request,
        "dashboard.html",
        {
            "active_page": "dashboard",
            "instances": instances,
            "jobs": jobs,
            "active_jobs": active_jobs,
            "scheduled_instances": scheduled_instances,
        },
    )


@app.get("/instances/new")
def new_instance(request: Request):
    return render(
        request,
        "instance_form.html",
        {
            "active_page": "instances",
            "title": "New Instance",
            "instance": None,
            "values": instance_values(None),
        },
    )


@app.post("/instances")
async def create_instance(request: Request):
    values = await read_form(request)
    db.save_instance(values)
    return RedirectResponse("/", status_code=303)


@app.get("/instances/{instance_id}")
def edit_instance(instance_id: int, request: Request):
    instance = db.get_instance(instance_id)
    if instance is None:
        raise HTTPException(status_code=404)
    return render(
        request,
        "instance_form.html",
        {
            "active_page": "instances",
            "title": f"Edit {instance.name}",
            "instance": instance,
            "values": instance_values(instance),
        },
    )


@app.post("/instances/{instance_id}")
async def update_instance(instance_id: int, request: Request):
    instance = db.get_instance(instance_id)
    if instance is None:
        raise HTTPException(status_code=404)
    values = await read_form(request)
    for secret in ("yodel_password", "twilio_auth_token"):
        if not values.get(secret):
            values[secret] = getattr(instance, secret)
    db.save_instance(values, instance_id=instance_id)
    return RedirectResponse("/", status_code=303)


@app.post("/instances/{instance_id}/delete")
def delete_instance(instance_id: int):
    db.delete_instance(instance_id)
    return RedirectResponse("/", status_code=303)


@app.post("/instances/{instance_id}/run/{command}")
def run_instance(instance_id: int, command: str):
    if command not in {"auth-check", "dry-run", "book"}:
        raise HTTPException(status_code=400, detail="Invalid command")
    try:
        job_id = enqueue_job(instance_id, command)
    except ValueError as exc:
        raise HTTPException(status_code=400, detail=str(exc)) from exc
    return RedirectResponse(f"/jobs/{job_id}", status_code=303)


@app.get("/jobs")
def jobs(request: Request):
    return render(
        request,
        "jobs.html",
        {
            "active_page": "jobs",
            "jobs": db.list_jobs(limit=100),
            "refresh_seconds": 10,
        },
    )


@app.get("/jobs/{job_id}")
def job_detail(job_id: int, request: Request):
    job = db.get_job(job_id)
    if job is None:
        raise HTTPException(status_code=404)
    return render(
        request,
        "job_detail.html",
        {
            "active_page": "jobs",
            "job": job,
            "refresh_seconds": 10 if job.status in {"queued", "running"} else None,
        },
    )


@app.get("/jobs/{job_id}/log", response_class=PlainTextResponse)
def job_log(job_id: int) -> str:
    job = db.get_job(job_id)
    if job is None or not job.log_path:
        raise HTTPException(status_code=404)
    path = Path(job.log_path)
    if not path.exists():
        return "Log file has not been created yet."
    return path.read_text(encoding="utf-8", errors="replace")[-20000:]


async def read_form(request: Request) -> dict[str, str]:
    body = (await request.body()).decode("utf-8")
    parsed = parse_qs(body, keep_blank_values=True)
    values = {key: items[-1] if items else "" for key, items in parsed.items()}
    for checkbox in (
        "enabled",
        "schedule_enabled",
        "headless",
        "check_all_day",
        "check_morning",
        "check_afternoon",
        "twilio_alerts_enabled",
    ):
        values[checkbox] = "1" if checkbox in parsed else "0"
    return values


def render(request: Request, template: str, context: dict):
    return templates.TemplateResponse(request, template, context)


def instance_values(instance: db.Instance | None) -> dict:
    values = dict(DEFAULT_INSTANCE)
    if instance is not None:
        values.update(asdict(instance))
    return values
