import base64
import os
import urllib.request
from pathlib import Path

username = os.environ.get("DAV_USERNAME", "alice")
password = Path("/run/secrets/dav_password").read_text().rstrip("\r\n")
request = urllib.request.Request("http://127.0.0.1:5232/", method="PROPFIND")
request.add_header("Depth", "0")
request.add_header("Authorization", "Basic " + base64.b64encode(f"{username}:{password}".encode()).decode())
with urllib.request.urlopen(request, timeout=3) as response:
    if response.status != 207:
        raise SystemExit(1)
