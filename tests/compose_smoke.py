import argparse
import base64
import datetime
import json
import time
import urllib.error
import urllib.request
from pathlib import Path

parser = argparse.ArgumentParser()
parser.add_argument("--after-restart", action="store_true")
args = parser.parse_args()
token = Path(".cache/ci-secrets/mcp_token").read_text().strip()
password = Path(".cache/ci-secrets/dav_password").read_text().strip()


def get(url):
    with urllib.request.urlopen(url, timeout=5) as response:
        return json.load(response)


def wait_ready():
    for _ in range(60):
        try:
            if get("http://127.0.0.1:8080/readyz")["ready"]:
                return
        except (OSError, urllib.error.HTTPError):
            pass
        time.sleep(1)
    raise AssertionError("application not ready")


wait_ready()
if not args.after_restart:
    request = urllib.request.Request("http://127.0.0.1:5232/alice/work/", data=b'<c:mkcalendar xmlns:c="urn:ietf:params:xml:ns:caldav"/>', method="MKCALENDAR")
    request.add_header("Authorization", "Basic " + base64.b64encode(f"alice:{password}".encode()).decode())
    request.add_header("Content-Type", "application/xml")
    with urllib.request.urlopen(request, timeout=10) as response:
        assert response.status == 201
    start = datetime.datetime.now(datetime.timezone.utc) + datetime.timedelta(seconds=8)
    body = {"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": {"name": "create_event", "arguments": {"calendar": "/alice/work/", "fields": {"summary": "CI synthetic reminder", "start": start.isoformat(), "end": (start + datetime.timedelta(minutes=30)).isoformat()}}}}
    request = urllib.request.Request("http://127.0.0.1:8080/mcp", data=json.dumps(body).encode())
    request.add_header("Authorization", "Bearer " + token)
    request.add_header("Content-Type", "application/json")
    request.add_header("Accept", "application/json, text/event-stream")
    request.add_header("MCP-Protocol-Version", "2025-11-25")
    with urllib.request.urlopen(request, timeout=20) as response:
        result = json.load(response)
    assert "error" not in result and not result["result"].get("isError"), result
    for _ in range(45):
        messages = get("http://127.0.0.1:18081/messages")
        if len(messages) >= 2:
            break
        time.sleep(1)
else:
    time.sleep(12)
messages = get("http://127.0.0.1:18081/messages")
assert sorted(m["channel"] for m in messages) == ["smtp", "telegram"], messages
print("Compose smoke test passed", "after restart" if args.after_restart else "initial delivery")
