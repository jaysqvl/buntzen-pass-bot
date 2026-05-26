from __future__ import annotations

import re
from datetime import datetime
from pathlib import Path


class Diagnostics:
    def __init__(self, base_dir: str | Path = "artifacts") -> None:
        self.run_id = datetime.now().strftime("%Y%m%d-%H%M%S")
        self.base_dir = Path(base_dir) / self.run_id
        self.base_dir.mkdir(parents=True, exist_ok=True)

    def path_for(self, name: str, suffix: str) -> Path:
        safe_name = re.sub(r"[^A-Za-z0-9_.-]+", "-", name).strip("-") or "artifact"
        return self.base_dir / f"{safe_name}.{suffix.lstrip('.')}"

    def screenshot(self, page, name: str) -> Path | None:
        path = self.path_for(name, "png")
        try:
            page.screenshot(path=str(path), full_page=True)
            return path
        except Exception:
            return None

    def html(self, page, name: str) -> Path | None:
        path = self.path_for(name, "html")
        try:
            path.write_text(page.content(), encoding="utf-8")
            return path
        except Exception:
            return None
