"""Radicale 3.9.0 adapter: bounded local enqueue, never network I/O."""
import json
import os
import sqlite3
from contextlib import closing
from pathlib import Path

from radicale.hook import BaseHook
from radicale.log import logger


class Hook(BaseHook):
    def __init__(self, configuration):
        super().__init__(configuration)
        self.path = Path(os.environ["RADICALE_EXTEND_QUEUE"]).resolve()
        # mode=rw forbids silently creating an uninitialized database.
        self.uri = self.path.as_uri() + "?mode=rw"
        with closing(sqlite3.connect(self.uri, uri=True, timeout=0.2)) as db:
            version = db.execute("SELECT version FROM protocol WHERE id=1").fetchone()
            if version != (1,):
                raise RuntimeError("unsupported Radicale extension queue protocol")
            db.execute("SELECT id FROM events LIMIT 0")

    @property
    def enabled(self):
        return True

    def notify(self, notification_item):
        if self.configuration.get("hook", "dryrun"):
            return
        try:
            payload = json.dumps(
                {"version": 1, "item": json.loads(notification_item.to_json())},
                ensure_ascii=False,
            )
            with closing(sqlite3.connect(self.uri, uri=True, timeout=0.2)) as db, db:
                db.execute("INSERT INTO events(payload) VALUES(?)", (payload,))
        except (sqlite3.Error, ValueError, TypeError):
            # Never log the raw payload or block DAV on notification failure.
            logger.error("Radicale extension enqueue failed; periodic DAV reconciliation required")
