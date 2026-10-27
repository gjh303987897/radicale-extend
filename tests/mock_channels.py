"""Synthetic SMTP and Telegram endpoints for GitHub Runner smoke tests."""
import json
import socketserver
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

messages = []
lock = threading.Lock()


class SMTP(socketserver.StreamRequestHandler):
    def handle(self):
        self.wfile.write(b"220 test SMTP\r\n")
        while line := self.rfile.readline():
            command = line.upper()
            if command.startswith((b"EHLO", b"HELO")):
                self.wfile.write(b"250 test\r\n")
            elif command.startswith(b"DATA"):
                self.wfile.write(b"354 Send data\r\n")
                data = bytearray()
                while (part := self.rfile.readline()) not in (b".\r\n", b""):
                    data.extend(part)
                with lock:
                    messages.append({"channel": "smtp", "bytes": len(data)})
                self.wfile.write(b"250 accepted\r\n")
            elif command.startswith(b"QUIT"):
                self.wfile.write(b"221 bye\r\n")
                break
            else:
                self.wfile.write(b"250 OK\r\n")


class HTTP(BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass

    def respond(self, data):
        body = json.dumps(data).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        with lock:
            self.respond(messages if self.path == "/messages" else {"ok": True})

    def do_POST(self):
        data = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        assert data["chat_id"] == "12345"
        with lock:
            messages.append({"channel": "telegram", "text": data["text"]})
        self.respond({"ok": True, "result": {"message_id": 1}})


threading.Thread(target=socketserver.ThreadingTCPServer(("0.0.0.0", 2525), SMTP).serve_forever, daemon=True).start()
ThreadingHTTPServer(("0.0.0.0", 8081), HTTP).serve_forever()
