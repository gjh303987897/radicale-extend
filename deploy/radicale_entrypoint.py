"""Fail startup if the adapter is unavailable; Radicale otherwise falls back to none."""
import os
import sys
from radicale_extend_hook import Hook


class Configuration:
    def get(self, section, option):
        return False


Hook(Configuration())
os.execv(sys.executable, [sys.executable, "-m", "radicale", "--config", "/etc/radicale/config"])
