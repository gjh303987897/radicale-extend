import subprocess
import sys
from pathlib import Path
import yaml

directory = Path(".cache")
directory.mkdir(exist_ok=True)
subprocess.run([sys.executable, "scripts/init_secrets.py", "--directory", str(directory / "ci-secrets")], check=True)
config = yaml.safe_load(Path("config.example.yaml").read_text())
config["sync_interval"] = "2s"
config["debounce"] = "0s"
config["reminder"]["default_before"] = "0s"
config["smtp"].update(enabled=True, host="mock-channels", port=2525, security="none", username="")
config["telegram"].update(enabled=True, chat_id="12345", api_url="http://mock-channels:8081")
config["calendars"] = [{"path": "/alice/work/", "changes": False}]
(directory / "ci-secrets/telegram_token").write_text("test-only\n")
# CI uses synthetic credentials only; permit the non-root container user to read them.
for file in (directory / "ci-secrets").iterdir():
    file.chmod(0o644)
(directory / "ci-secrets").chmod(0o755)
(directory / "ci.yaml").write_text(yaml.safe_dump(config))
