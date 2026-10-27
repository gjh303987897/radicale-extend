import concurrent.futures
import json
import os
import sqlite3
import tempfile
import time
import unittest
from pathlib import Path
from contextlib import closing
from unittest.mock import patch

from radicale.hook import HookNotificationItem, HookNotificationItemTypes
from radicale_extend_hook import Hook


class Configuration:
    def __init__(self, dry=False):
        self.dry = dry

    def get(self, section, option):
        return self.dry


class HookTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.path = Path(self.temp.name) / "queue.db"
        with closing(sqlite3.connect(self.path)) as db, db:
            db.executescript("CREATE TABLE protocol(id INTEGER PRIMARY KEY,version INTEGER); INSERT INTO protocol VALUES(1,1); CREATE TABLE events(id INTEGER PRIMARY KEY,payload TEXT);")
        env = patch.dict(os.environ, {"RADICALE_EXTEND_QUEUE": str(self.path)})
        env.start()
        self.addCleanup(env.stop)

    def item(self):
        return HookNotificationItem(HookNotificationItemTypes.UPSERT, "/alice/calendar/a.ics", content_type="VCALENDAR", new_content="private calendar text", actor="alice")

    def count(self):
        with closing(sqlite3.connect(self.path)) as db, db:
            return db.execute("SELECT count(*) FROM events").fetchone()[0]

    def test_serialization_and_concurrent_enqueue(self):
        hook = Hook(Configuration())
        with concurrent.futures.ThreadPoolExecutor(max_workers=4) as pool:
            list(pool.map(lambda _: hook.notify(self.item()), range(20)))
        self.assertEqual(self.count(), 20)
        with closing(sqlite3.connect(self.path)) as db, db:
            payload = json.loads(db.execute("SELECT payload FROM events LIMIT 1").fetchone()[0])
        self.assertEqual(payload["version"], 1)
        self.assertEqual(payload["item"]["point"], "alice/calendar")

    def test_dryrun(self):
        Hook(Configuration(True)).notify(self.item())
        self.assertEqual(self.count(), 0)

    def test_uninitialized_and_version_mismatch(self):
        with patch.dict(os.environ, {"RADICALE_EXTEND_QUEUE": str(self.path.parent / "missing.db")}):
            with self.assertRaises(sqlite3.Error):
                Hook(Configuration())
            self.assertFalse((self.path.parent / "missing.db").exists())
        with closing(sqlite3.connect(self.path)) as db, db:
            db.execute("UPDATE protocol SET version=2")
        with self.assertRaises(RuntimeError):
            Hook(Configuration())

    def test_bounded_failure_does_not_log_payload(self):
        hook = Hook(Configuration())
        lock = sqlite3.connect(self.path)
        self.addCleanup(lock.close)
        lock.execute("BEGIN EXCLUSIVE")
        start = time.monotonic()
        with self.assertLogs("radicale", level="ERROR") as logs:
            hook.notify(self.item())
        self.assertLess(time.monotonic() - start, 1)
        self.assertNotIn("private calendar text", " ".join(logs.output))
        lock.rollback()


if __name__ == "__main__":
    unittest.main()
