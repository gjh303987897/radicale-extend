"""Offline checks; intentionally never executes docker or docker compose."""
import re
from pathlib import Path
import yaml

root = Path(__file__).resolve().parents[1]
compose = yaml.safe_load((root / "compose.yaml").read_text())
assert set(compose["services"]) == {"init", "radicale", "app"}
assert compose["services"]["app"]["depends_on"]["radicale"]["condition"] == "service_healthy"
for name in ["ci.yml", "publish.yml"]:
    workflow = yaml.load((root / ".github/workflows" / name).read_text(), Loader=yaml.BaseLoader)
    for job in workflow["jobs"].values():
        for step in job.get("steps", []):
            if "uses" in step and not step["uses"].startswith("./"):
                assert re.fullmatch(r"[\w./-]+@[0-9a-f]{40}", step["uses"]), step["uses"]
publish = yaml.load((root / ".github/workflows/publish.yml").read_text(), Loader=yaml.BaseLoader)
assert set(publish["on"]) == {"workflow_dispatch"}
for filename in ["config.example.yaml", "tests/compose.ci.yaml"]:
    yaml.safe_load((root / filename).read_text())
print("Offline deployment/workflow checks passed; no Docker runtime validation performed.")
