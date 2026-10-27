"""Generate deployment credentials locally; never overwrites existing secrets."""
import argparse
import os
import secrets
from pathlib import Path

import bcrypt

parser = argparse.ArgumentParser()
parser.add_argument("--username", default="alice")
parser.add_argument("--directory", default="secrets")
args = parser.parse_args()
if not args.username or any(c in args.username for c in ":/\\\n\r"):
    parser.error("username cannot contain path separators, colons or newlines")
directory = Path(args.directory)
directory.mkdir(mode=0o700, parents=True, exist_ok=True)
directory.chmod(0o700)
names = ["mcp_token", "dav_password", "radicale_users", "smtp_password", "telegram_token"]
if any((directory / name).exists() for name in names):
    parser.error("secret files already exist; refusing to overwrite")
password = secrets.token_urlsafe(24)
values = [secrets.token_urlsafe(32), password, args.username + ":" + bcrypt.hashpw(password.encode(), bcrypt.gensalt()).decode(), "", ""]
for name, value in zip(names, values):
    # Compose file-backed secrets preserve the host file UID. The enclosing
    # directory is private (0700); individual files must be readable by UID 10001.
    fd = os.open(directory / name, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o644)
    with os.fdopen(fd, "w", encoding="utf-8") as f:
        f.write(value + "\n")
    (directory / name).chmod(0o644)
print(f"Credentials created in {directory}. DAV username: {args.username}. Read dav_password and mcp_token locally when configuring clients.")
