#!/usr/bin/env python3
"""Native C-ABI v1 smoke test for the OpenDesign CPA plugin.

    python3 scripts/smoke-abi.py dist/open-design.so [--json-report PATH]

Loads the c-shared library with ctypes exactly like CPA's dlopen host, supplies
a fake host callback table (host.auth.save/list, host.http.do/do_stream/
stream_read/stream_close, host.stream.emit/close), and drives plugin.register,
management, auth.parse, model.for_auth, executor.execute and
executor.execute_stream against a loopback fake upstream.

Safety: only a fake fixture key (sk-amr-test-fixture) is used; the only network
endpoint contacted is a 127.0.0.1 fake server. Temporary data lives in a
mkdtemp directory that is removed with shutil.rmtree (never `rm -rf`).
No SSH, production config, real credential or real inference is touched.
"""
from __future__ import annotations

import argparse
import base64
import ctypes
import json
import shutil
import os
import select
import socket
import ssl
import subprocess
import sys
import urllib.request
import tempfile
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

FIXTURE_KEY = "sk-amr-test-fixture"
PLUGIN_ID = "open-design"
API = "/v0/management/" + PLUGIN_ID
CONSOLE = "/v0/resource/plugins/" + PLUGIN_ID + "/console"


def require(condition, message):
    if not condition:
        raise AssertionError(message)


# ---------------------------------------------------------------- fake upstream
class FakeUpstream:
    """Loopback OpenAI-compatible upstream; records every request it sees."""

    def __init__(self):
        self.requests = []
        self.fail = {}          # path suffix -> (status, body)
        self.server = None
        self.port = None

    def start(self):
        outer = self

        class Handler(BaseHTTPRequestHandler):
            protocol_version = "HTTP/1.1"

            def log_message(self, *_args):
                pass

            def _record(self, body):
                outer.requests.append({
                    "method": self.command,
                    "path": self.path,
                    "authorization": self.headers.get("Authorization"),
                    "content_type": self.headers.get("Content-Type"),
                    "body": body,
                })

            def _buffer(self, status, payload, ctype="application/json"):
                self.send_response(status)
                self.send_header("Content-Type", ctype)
                self.send_header("Content-Length", str(len(payload)))
                self.end_headers()
                self.wfile.write(payload)

            def do_GET(self):
                self._record(b"")
                for suffix, (status, payload) in outer.fail.items():
                    if self.path.endswith(suffix):
                        self._buffer(status, payload.encode())
                        return
                if self.path.endswith("/models"):
                    body = {"object": "list", "data": [
                        {"id": "deepseek-v4.1-flash", "object": "model", "owned_by": "opendesign"},
                        {"id": "glm-5.3-flash", "object": "model", "owned_by": "opendesign"},
                    ]}
                    self._buffer(200, json.dumps(body).encode())
                    return
                self._buffer(404, b'{"error":{"message":"not found"}}')

            def do_POST(self):
                length = int(self.headers.get("Content-Length") or 0)
                raw = self.rfile.read(length) if length else b""
                self._record(raw)
                for suffix, (status, payload) in outer.fail.items():
                    if self.path.endswith(suffix):
                        self._buffer(status, payload.encode())
                        return
                if not self.path.endswith("/chat/completions"):
                    self._buffer(404, b'{"error":{"message":"not found"}}')
                    return
                try:
                    req = json.loads(raw or b"{}")
                except json.JSONDecodeError:
                    req = {}
                if req.get("stream"):
                    frames = [
                        {"id": "cc-1", "object": "chat.completion.chunk", "model": "deepseek-v4.1-flash",
                         "choices": [{"index": 0, "delta": {"role": "assistant"}, "finish_reason": None}]},
                        {"id": "cc-1", "object": "chat.completion.chunk", "model": "deepseek-v4.1-flash",
                         "choices": [{"index": 0, "delta": {"content": "Hello"}, "finish_reason": None}]},
                        {"id": "cc-1", "object": "chat.completion.chunk", "model": "deepseek-v4.1-flash",
                         "choices": [{"index": 0, "delta": {"content": " from OpenDesign"}, "finish_reason": None}]},
                        {"id": "cc-1", "object": "chat.completion.chunk", "model": "deepseek-v4.1-flash",
                         "choices": [{"index": 0, "delta": {}, "finish_reason": "stop"}],
                         "usage": {"prompt_tokens": 21, "completion_tokens": 5, "total_tokens": 26}},
                    ]
                    # Deliberately split one frame across two TCP writes to exercise
                    # the plugin's own SSE reassembly.
                    blob = "".join("data: " + json.dumps(f) + "\n\n" for f in frames) + "data: [DONE]\n\n"
                    cut = blob.index("Hello") + 2
                    chunks = [blob[:cut].encode(), blob[cut:].encode()]
                    self.send_response(200)
                    self.send_header("Content-Type", "text/event-stream")
                    self.send_header("Cache-Control", "no-cache")
                    self.send_header("Transfer-Encoding", "chunked")
                    self.end_headers()
                    for piece in chunks:
                        self.wfile.write(b"%x\r\n%s\r\n" % (len(piece), piece))
                        self.wfile.flush()
                        time.sleep(0.01)
                    self.wfile.write(b"0\r\n\r\n")
                    self.wfile.flush()
                    return
                body = {
                    "id": "cc-nonstream", "object": "chat.completion", "model": "deepseek-v4.1-flash",
                    "choices": [{"index": 0, "message": {
                        "role": "assistant",
                        "content": "Hello",
                        "tool_calls": [{"id": "call_1", "type": "function",
                                        "function": {"name": "lookup", "arguments": '{"q":"x"}'}}],
                    }, "finish_reason": "tool_calls"}],
                    "usage": {"prompt_tokens": 12, "completion_tokens": 7, "total_tokens": 19},
                }
                self._buffer(200, json.dumps(body).encode())

        self.server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self.port = self.server.server_address[1]
        threading.Thread(target=self.server.serve_forever, daemon=True).start()
        return self

    def stop(self):
        if self.server is not None:
            self.server.shutdown()
            self.server.server_close()


# --------------------------------------------------------------- C ABI plumbing
class Buffer(ctypes.Structure):
    _fields_ = [("ptr", ctypes.c_void_p), ("length", ctypes.c_size_t)]


# Plugin-side: int (*)(char*, uint8_t*, size_t, cliproxy_buffer*)
CallFn = ctypes.CFUNCTYPE(ctypes.c_int, ctypes.c_char_p,
                          ctypes.POINTER(ctypes.c_uint8), ctypes.c_size_t,
                          ctypes.POINTER(Buffer))
# Host-side: int (*)(void*, const char*, const uint8_t*, size_t, cliproxy_buffer*)
# The leading void* is host_ctx; CPA passes it on every callback.
HostCallFn = ctypes.CFUNCTYPE(ctypes.c_int, ctypes.c_void_p, ctypes.c_char_p,
                              ctypes.POINTER(ctypes.c_uint8), ctypes.c_size_t,
                              ctypes.POINTER(Buffer))
FreeFn = ctypes.CFUNCTYPE(None, ctypes.c_void_p, ctypes.c_size_t)
ShutdownFn = ctypes.CFUNCTYPE(None)


class PluginAPI(ctypes.Structure):
    _fields_ = [("abi_version", ctypes.c_uint32), ("call", CallFn),
                ("free_buffer", FreeFn), ("shutdown", ShutdownFn)]


class FakeHost:
    """Host side of the C ABI: implements the callback function table."""

    def __init__(self, upstream, auth_dir):
        self.upstream = upstream
        self.auth_dir = Path(auth_dir)
        self.auth_dir.mkdir(parents=True, exist_ok=True)
        self.calls = []
        self.callback_failures = []  # any exception raised inside dispatch()
        self.operations = {}         # host.http.operation_open id -> record
        self.cancelled_operations = []
        self.next_operation = 0
        self.upstream_streams = {}   # host stream id -> list of byte chunks
        self.client_streams = {}     # plugin output stream id -> emitted records
        self.closed_upstreams = []
        self.closed_clients = []
        self.next_stream = 0
        self.auth_files = {}
        self._keepalive = []         # keep ctypes buffers alive across the call

    # -- fake upstream transport (mirrors CPA host.http.* semantics) --
    def _http(self, method, url, headers, body):
        require(url.startswith("https://amr-link.open-design.ai/v1/"),
                "plugin must call the pinned OpenDesign origin: %s" % url)
        path = url.split("amr-link.open-design.ai", 1)[1]
        # Redirect the pinned hostname to the loopback fake without touching DNS.
        # Loopback-only test: this is our own HTTP client, not the plugin's.
        conn = socket.create_connection(("127.0.0.1", self.upstream.port), timeout=5)
        try:
            hdrs = {"Host": "amr-link.open-design.ai", "Connection": "close"}
            for name, values in (headers or {}).items():
                hdrs[name] = values[0] if isinstance(values, list) else values
            payload = body or b""
            request = "%s %s HTTP/1.1\r\n" % (method, path)
            for name, value in hdrs.items():
                request += "%s: %s\r\n" % (name, value)
            if payload:
                request += "Content-Length: %d\r\n" % len(payload)
            request += "\r\n"
            conn.sendall(request.encode() + payload)
            raw = b""
            while b"\r\n\r\n" not in raw:
                piece = conn.recv(65536)
                if not piece:
                    break
                raw += piece
            head, _, rest = raw.partition(b"\r\n\r\n")
            lines = head.split(b"\r\n")
            status = int(lines[0].split()[1])
            # CPA's host.http.* returns http.Header: map[string][]string.
            resp_headers = {}
            for line in lines[1:]:
                name, _, value = line.partition(b":")
                resp_headers.setdefault(name.decode().strip().lower(), []).append(
                    value.decode().strip())
            if ",".join(resp_headers.get("transfer-encoding", [])).lower() == "chunked":
                while not rest.endswith(b"0\r\n\r\n"):
                    piece = conn.recv(65536)
                    if not piece:
                        break
                    rest += piece
                rest = _dechunk(rest)
            else:
                length = int((resp_headers.get("content-length") or ["0"])[0] or 0)
                while len(rest) < length:
                    piece = conn.recv(65536)
                    if not piece:
                        break
                    rest += piece
            return status, resp_headers, rest
        finally:
            conn.close()

    # -- host callback table --
    def callback(self, _ctx, method, request, request_len, response):
        # ctypes swallows exceptions raised out of a CFUNCTYPE callback (it only
        # prints "Exception ignored"), which would let a broken fake host look
        # like a pass. Catch everything, answer with a host_error envelope, and
        # remember the failure so the scenario can require() on it.
        try:
            name = ctypes.string_at(method).decode()
            raw = bytes(bytearray(request[:request_len])) if request_len else b""
            payload = json.loads(raw) if raw else {}
            result = self.dispatch(name, payload)
            envelope = {"ok": True, "result": result}
            code = 0
        except Exception as exc:  # noqa: BLE001 - must not escape into ctypes
            self.callback_failures.append({
                "method": name if "name" in locals() else "<undecodable>",
                "error": "%s: %s" % (type(exc).__name__, exc),
            })
            envelope = {"ok": False, "error": {"code": "host_error",
                                               "message": "%s: %s" % (type(exc).__name__, exc),
                                               "http_status": 500}}
            code = 1
        data = json.dumps(envelope).encode()
        holder = ctypes.create_string_buffer(data, len(data))
        self._keepalive.append(holder)
        response[0].ptr = ctypes.cast(holder, ctypes.c_void_p)
        response[0].length = len(data)
        return code

    def _named(self, status, headers, body=None):
        """host.http.do result exactly as CPA marshals pluginapi.HTTPResponse.

        Note the asymmetry: this one has NO json tags, so the wire keys are
        PascalCase, while do_stream/stream_read use explicit snake_case tags.
        """
        return {"StatusCode": status, "Headers": headers, "Body": body}

    def dispatch(self, name, payload):
        self.calls.append({"method": name, "payload": payload})
        if name == "host.http.operation_open":
            # CPA binds the operation to host_callback_id and returns an opaque
            # numeric id; quiesce later cancels through host.http.cancel.
            self.next_operation += 1
            operation_id = str(self.next_operation)
            self.operations[operation_id] = {
                "host_callback_id": payload.get("host_callback_id") or "",
                "cancelled": False,
            }
            return {"operation_id": operation_id}
        if name == "host.http.cancel":
            operation_id = str(payload.get("operation_id") or "")
            require(operation_id in self.operations,
                    "host.http.cancel for unknown operation %r" % operation_id)
            self.operations[operation_id]["cancelled"] = True
            self.cancelled_operations.append(operation_id)
            return {}
        if name == "host.auth.save":
            file_name = payload["name"]
            require(file_name.endswith(".json"), "host.auth.save name must end with .json")
            require("/" not in file_name, "host.auth.save name must be a bare file name")
            doc = payload["json"]
            path = self.auth_dir / file_name
            path.write_text(json.dumps(doc))
            self.auth_files[file_name] = doc
            return {"name": file_name, "path": str(path)}
        if name == "host.auth.list":
            files = []
            for file_name, doc in self.auth_files.items():
                files.append({"id": doc.get("id", ""), "name": file_name,
                              "provider": doc.get("type", ""),
                              "label": doc.get("label", ""),
                              "disabled": bool(doc.get("disabled"))})
            return {"files": files}
        if name == "host.http.do":
            status, headers, body = self._http(payload.get("method", "GET"),
                                               payload["url"], payload.get("headers"),
                                               payload.get("body") and
                                               base64.b64decode(payload["body"]))
            # CPA marshals pluginapi.HTTPResponse with NO json tags, so the wire
            # is {"StatusCode":..,"Headers":..,"Body":..}. --allow-snake-case-host
            # downgrades this to the plugin's own field names so the remaining
            # ABI surface can still be exercised while a casing bug is open.
            return self._named(status, headers, body=base64.b64encode(body).decode())
        if name == "host.http.do_stream":
            status, headers, body = self._http(payload.get("method", "POST"),
                                               payload["url"], payload.get("headers"),
                                               payload.get("body") and
                                               base64.b64decode(payload["body"]))
            self.next_stream += 1
            stream_id = "upstream-%d" % self.next_stream
            self.upstream_streams[stream_id] = body
            # rpcHostHTTPStreamResponse has explicit `json:"status_code"` tags.
            return {"status_code": status, "headers": headers, "stream_id": stream_id}
        if name == "host.http.stream_read":
            stream_id = payload["stream_id"]
            require(stream_id in self.upstream_streams,
                    "stream_read on unknown stream %s" % stream_id)
            body = self.upstream_streams[stream_id]
            self.upstream_streams[stream_id] = b""
            # rpcHostHTTPStreamReadResponse is snake_case too.
            return {"payload": base64.b64encode(body).decode() if body else "",
                    "done": not body}
        if name == "host.http.stream_close":
            self.closed_upstreams.append(payload["stream_id"])
            self.upstream_streams.pop(payload["stream_id"], None)
            return {}
        if name == "host.stream.emit":
            stream_id = payload["stream_id"]
            self.client_streams.setdefault(stream_id, []).append(
                base64.b64decode(payload.get("payload") or ""))
            return {}
        if name == "host.stream.close":
            self.closed_clients.append({"stream_id": payload["stream_id"],
                                        "error": payload.get("error")})
            return {}
        raise AssertionError("plugin called unexpected host method %r" % name)


def env_text(envelope):
    return json.dumps(envelope, sort_keys=True)


# ------------------------------------------------------- real CPA host (offline)
# Stock CPA builds that load native plugins are found locally at
#   /tmp/cpa-v8.0.48-audit.vMdl5Y/cli-proxy-api                 (v8.0.48, commit dbb70621)
#   /tmp/cpa-audit-gny4ig42/v8.0.48/CLIProxyAPI-8.0.48/        (matching source tree)
# This mode boots that binary against a throwaway config and only touches
# non-egress paths: dlopen/register, management routes, host.auth.save. The
# /upstream-models and chat-completions calls would reach the real
# amr-link.open-design.ai origin, so they are deliberately skipped here and stay
# covered by the ctypes mode above with a loopback upstream.
CPA_DEFAULT = "/tmp/cpa-v8.0.48-audit.vMdl5Y/cli-proxy-api"


def run_cpa(artifact, binary):
    import subprocess
    import urllib.request

    binary = Path(binary).resolve()
    require(binary.is_file(), "CPA binary not found: %s" % binary)
    work = Path(tempfile.mkdtemp(prefix="opendesign-cpa-"))
    evidence = {}
    process = None
    try:
        (work / "plugins/linux/amd64").mkdir(parents=True)
        shutil.copyfile(artifact, work / "plugins/linux/amd64/open-design.so")
        (work / "auths").mkdir()
        with socket.socket() as probe:
            probe.bind(("127.0.0.1", 0))
            port = probe.getsockname()[1]
        management_key = "smoke-management-fixture-key"
        client_key = "smoke-client-fixture-key"
        config = "\n".join([
            "config-version: 8",
            "server:",
            "  host: 127.0.0.1",
            "  port: %d" % port,
            "management:",
            "  secret-key: %s" % management_key,
            "  allow-remote: false",
            "  disable-control-panel: true",
            "  disable-auto-update-panel: true",
            "access:",
            "  api-keys:",
            "    - %s" % client_key,
            "oauth:",
            "  auth-dir: %s" % (work / "auths"),
            "routing:",
            "  retry:",
            "    request-retry: 0",
            "plugins:",
            "  enabled: true",
            "  dir: %s" % (work / "plugins"),
            "  configs:",
            "    open-design:",
            "      enabled: true",
            "      data_dir: %s" % (work / "data"),
            "",
        ])
        (work / "config.yaml").write_text(config)
        (work / "config.yaml").chmod(0o600)

        log = (work / "cpa.log").open("wb")
        process = subprocess.Popen(
            [str(binary), "-config", str(work / "config.yaml"), "-local-model"],
            cwd=str(work), stdout=log, stderr=log, start_new_session=True)
        opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))

        def request(path, body=None, method=None):
            data = json.dumps(body).encode() if body is not None else None
            req = urllib.request.Request(
                "http://127.0.0.1:%d%s" % (port, path), data=data,
                method=method,
                headers={"Authorization": "Bearer " + management_key,
                         "Content-Type": "application/json"})
            with opener.open(req, timeout=10) as resp:
                raw = resp.read()
                return resp.status, raw, dict(resp.headers)

        deadline = time.monotonic() + 60
        plugins = None
        while time.monotonic() < deadline:
            if process.poll() is not None:
                raise AssertionError("CPA exited early; see %s" % (work / "cpa.log"))
            try:
                status, raw, _ = request("/v0/management/plugins")
                if status == 200:
                    plugins = json.loads(raw)["plugins"]
                    if any(x["id"] == PLUGIN_ID and x["registered"] for x in plugins):
                        break
            except Exception:  # noqa: BLE001 - startup race
                pass
            time.sleep(0.25)
        else:
            raise AssertionError("CPA never registered the plugin; see %s" % (work / "cpa.log"))

        entry = next(x for x in plugins if x["id"] == PLUGIN_ID)
        evidence["host_registration"] = {
            "plugin_id": entry["id"], "registered": entry["registered"],
            "effective_enabled": entry.get("effective_enabled"),
            "version": entry.get("version")}

        status, raw, headers = request(API + "/status")
        require(status == 200, "real host status returned %s" % status)
        body = json.loads(raw)
        require(body["version"] == "0.1.0", body)
        require(FIXTURE_KEY not in raw.decode(), "status leaked the api key")
        evidence["host_status"] = {"version": body["version"],
                                   "base_url": body["base_url"],
                                   "cpa_version": headers.get("X-Cpa-Version")}

        status, raw, _ = request(API + "/credentials",
                                 {"label": "fixture", "api_key": FIXTURE_KEY})
        require(status == 201, "real host credential create returned %s: %s" % (status, raw))
        require(FIXTURE_KEY not in raw.decode(), "credential response leaked the key")
        credential_id = json.loads(raw)["id"]
        saved = list((work / "auths").glob("*.json"))
        require(saved, "host.auth.save did not write into auth-dir")
        require(any(FIXTURE_KEY in f.read_text() for f in saved),
                "credential did not land in the CPA auth-dir")
        evidence["host_auth_save"] = {"files": [f.name for f in saved],
                                      "contains_key": True}

        status, raw, _ = request(API + "/models", {"models": [
            {"id": "deepseek-flash", "upstream_id": "deepseek-v4.1-flash"}]})
        require(status == 200, "real host models POST returned %s: %s" % (status, raw))
        status, raw, _ = request(API + "/status")
        require(json.loads(raw)["models"] == [
            {"id": "deepseek-flash", "upstream_id": "deepseek-v4.1-flash"}], raw)
        evidence["host_models"] = json.loads(raw)["models"]

        status, raw, _ = request(API + "/credentials?id=" + credential_id, method="DELETE")
        require(status == 200, "real host credential delete returned %s: %s" % (status, raw))
        require(not list((work / "auths").glob("*.json")),
                "credential file survived deletion")
        evidence["host_credential_delete"] = {"auth_dir_empty": True}
        return evidence
    except AssertionError as exc:
        evidence.setdefault("status", "FAIL")
        evidence.setdefault("failure", str(exc)[:600])
        return evidence
    finally:
        if process is not None and process.poll() is None:
            process.terminate()
            try:
                process.wait(timeout=20)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait(timeout=10)
        shutil.rmtree(work, ignore_errors=True)


# ------------------------------------------- real CPA host + local fake origin
# End-to-end mode. The plugin keeps its pinned origin
# https://amr-link.open-design.ai/v1 (no core test hook); the host is steered
# onto a throwaway local origin purely through CPA configuration:
#   requests.proxy-url -> loopback HTTP CONNECT proxy -> fake TLS origin
#   SSL_CERT_FILE      -> test CA trusted only by the CPA child process
# No /etc/hosts, no system trust store and no system proxy are touched.

class LocalProxy:
    """Loopback HTTP CONNECT proxy that only dials allow-listed destinations."""

    def __init__(self, allow=("amr-link.open-design.ai:443",)):
        self.allow = set(allow)
        self.connects = []
        self.refused = []
        self.server = None

    def start(self):
        outer = self

        class Handler(BaseHTTPRequestHandler):
            protocol_version = "HTTP/1.1"

            def log_message(self, *_args):
                pass

            def do_CONNECT(self):
                target = self.path
                if target not in outer.allow:
                    outer.refused.append(target)
                    self.send_response(403)
                    self.send_header("Content-Length", "0")
                    self.end_headers()
                    return
                outer.connects.append(target)
                require(outer.origin_port is not None, "proxy has no fake origin port")
                upstream = socket.create_connection(("127.0.0.1", outer.origin_port), timeout=10)
                self.send_response(200, "Connection Established")
                self.end_headers()
                self.wfile.flush()
                self._pipe(upstream)

            def do_GET(self):
                outer.refused.append("plain-http:" + self.path)
                self.send_response(403)
                self.send_header("Content-Length", "0")
                self.end_headers()

            def do_POST(self):
                outer.refused.append("plain-http:" + self.path)
                self.send_response(403)
                self.send_header("Content-Length", "0")
                self.end_headers()

            def _pipe(self, upstream):
                client = self.connection
                sockets = [client, upstream]
                try:
                    while True:
                        readable, _, errored = select.select(sockets, [], sockets, 30)
                        if errored or not readable:
                            return
                        for sock in readable:
                            try:
                                data = sock.recv(65536)
                            except OSError:
                                return
                            if not data:
                                return
                            (upstream if sock is client else client).sendall(data)
                finally:
                    upstream.close()

        self.server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self.port = self.server.server_address[1]
        threading.Thread(target=self.server.serve_forever, daemon=True).start()
        return self

    def allow_more(self, target):
        self.allow.add(target)

    def stop(self):
        if self.server is not None:
            self.server.shutdown()
            self.server.server_close()


class TlsFakeOrigin:
    """OpenAI-shaped fake upstream behind a private-CA TLS socket."""

    def __init__(self, cert, key):
        self.cert = Path(cert)
        self.key = Path(key)
        self.requests = []
        self.server = None
        # When set, GET /models answers 307 pointing at this absolute URL so the
        # real host's redirect policy can be observed on the wire.
        self.redirect_to = None
        # When set, /chat/completions answers this (status, body) instead.
        self.fail = None

    def start(self):
        outer = self

        class Handler(FakeUpstreamHandler):
            owner = outer

        self.server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self.port = self.server.server_address[1]
        context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
        context.load_cert_chain(str(self.cert), str(self.key))
        self.server.socket = context.wrap_socket(self.server.socket, server_side=True)
        threading.Thread(target=self.server.serve_forever, daemon=True).start()
        return self

    def stop(self):
        if self.server is not None:
            self.server.shutdown()
            self.server.server_close()


class FakeUpstreamHandler(BaseHTTPRequestHandler):
    """Shared OpenAI-shaped responses; records what the real CPA host sent."""

    protocol_version = "HTTP/1.1"
    owner = None

    def log_message(self, *_args):
        pass

    def _send(self, status, payload, ctype="application/json", extra=None):
        self.send_response(status)
        self.send_header("Content-Type", ctype)
        self.send_header("Content-Length", str(len(payload)))
        for name, value in (extra or {}).items():
            self.send_header(name, value)
        self.end_headers()
        self.wfile.write(payload)

    def _record(self, raw):
        self.owner.requests.append({
            "method": self.command,
            "host": self.headers.get("Host"),
            "path": self.path,
            "authorization": self.headers.get("Authorization"),
            "user_agent": self.headers.get("User-Agent"),
            "body": raw,
            "tls": isinstance(self.connection, ssl.SSLSocket),
        })

    def do_GET(self):
        self._record(b"")
        if (self.path.endswith("/models") and self.owner.redirect_to
                and (self.headers.get("Host") or "").startswith("amr-link")):
            self._send(307, b'{"error":{"message":"moved"}}',
                       extra={"Location": self.owner.redirect_to})
            return
        if self.path.endswith("/models"):
            self._send(200, json.dumps({"object": "list", "data": [
                {"id": "deepseek-v4.1-flash", "object": "model", "owned_by": "opendesign"},
                {"id": "glm-5.3-flash", "object": "model", "owned_by": "opendesign"},
            ]}).encode())
            return
        self._send(404, b'{"error":{"message":"not found"}}')

    def do_POST(self):
        length = int(self.headers.get("Content-Length") or 0)
        raw = self.rfile.read(length) if length else b""
        self._record(raw)
        if self.owner.fail is not None and self.path.endswith("/chat/completions"):
            status, body = self.owner.fail
            self._send(status, body.encode() if isinstance(body, str) else body)
            return
        if not self.path.endswith("/chat/completions"):
            self._send(404, b'{"error":{"message":"not found"}}')
            return
        req = json.loads(raw or b"{}")
        if req.get("stream"):
            frames = [
                {"id": "cc-1", "object": "chat.completion.chunk", "model": "deepseek-v4.1-flash",
                 "choices": [{"index": 0, "delta": {"role": "assistant"}, "finish_reason": None}]},
                {"id": "cc-1", "object": "chat.completion.chunk", "model": "deepseek-v4.1-flash",
                 "choices": [{"index": 0, "delta": {"content": "Hello"}, "finish_reason": None}]},
                {"id": "cc-1", "object": "chat.completion.chunk", "model": "deepseek-v4.1-flash",
                 "choices": [{"index": 0, "delta": {}, "finish_reason": "stop"}],
                 "usage": {"prompt_tokens": 21, "completion_tokens": 5, "total_tokens": 26}},
            ]
            blob = "".join("data: " + json.dumps(f) + "\n\n" for f in frames) + "data: [DONE]\n\n"
            payload = blob.encode()
            self.send_response(200)
            self.send_header("Content-Type", "text/event-stream")
            self.send_header("Cache-Control", "no-cache")
            self.send_header("Content-Length", str(len(payload)))
            self.end_headers()
            self.wfile.write(payload)
            return
        body = {
            "id": "cc-nonstream", "object": "chat.completion", "model": "deepseek-v4.1-flash",
            "choices": [{"index": 0, "message": {
                "role": "assistant", "content": "Hello",
                "tool_calls": [{"id": "call_1", "type": "function",
                                "function": {"name": "lookup", "arguments": '{"q":"x"}'}}],
            }, "finish_reason": "tool_calls"}],
            "usage": {"prompt_tokens": 12, "completion_tokens": 7, "total_tokens": 19},
        }
        self._send(200, json.dumps(body).encode())


def make_test_ca(work, san="amr-link.open-design.ai,redirect-probe.example.test"):
    """Create a throwaway CA + leaf cert for the TLS fake origin (openssl only)."""
    work = Path(work)
    ca_key, ca_crt = work / "ca.key", work / "ca.crt"
    leaf_key, leaf_csr, leaf_crt = work / "origin.key", work / "origin.csr", work / "origin.crt"
    ext = work / "origin.ext"
    ext.write_text(
        "subjectAltName=%s\n"
        "basicConstraints=CA:FALSE\n"
        "keyUsage=digitalSignature,keyEncipherment\n"
        "extendedKeyUsage=serverAuth\n"
        % ",".join("DNS:" + n.strip() for n in san.split(",")))
    quiet = {"check": True, "capture_output": True}
    subprocess.run(["openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "2",
                    "-subj", "/CN=opendesign smoke test CA",
                    "-keyout", str(ca_key), "-out", str(ca_crt)], **quiet)
    subprocess.run(["openssl", "req", "-newkey", "rsa:2048", "-nodes",
                    "-subj", "/CN=%s" % san,
                    "-keyout", str(leaf_key), "-out", str(leaf_csr)], **quiet)
    subprocess.run(["openssl", "x509", "-req", "-in", str(leaf_csr), "-CA", str(ca_crt),
                    "-CAkey", str(ca_key), "-CAcreateserial", "-days", "2",
                    "-extfile", str(ext), "-out", str(leaf_crt)], **quiet)
    return ca_crt, leaf_crt, leaf_key


def run_cpa_live(artifact, binary):
    """Boot real CPA against a loopback CONNECT proxy + private-CA TLS fake origin."""
    binary = Path(binary).resolve()
    require(binary.is_file(), "CPA binary not found: %s" % binary)
    work = Path(tempfile.mkdtemp(prefix="opendesign-cpa-live-"))
    evidence = {}
    process = None
    proxy = origin = None
    log = None
    try:
        ca_crt, leaf_crt, leaf_key = make_test_ca(work)
        origin = TlsFakeOrigin(leaf_crt, leaf_key)
        proxy = LocalProxy()
        proxy.origin_port = None
        origin.start()
        proxy.origin_port = origin.port
        proxy.start()

        (work / "plugins/linux/amd64").mkdir(parents=True)
        shutil.copyfile(artifact, work / "plugins/linux/amd64/open-design.so")
        with socket.socket() as probe:
            probe.bind(("127.0.0.1", 0))
            port = probe.getsockname()[1]
        management_key = "smoke-management-fixture-key"
        client_key = "smoke-client-fixture-key"
        config = "\n".join([
            "config-version: 8",
            "server:",
            "  host: 127.0.0.1",
            "  port: %d" % port,
            "management:",
            "  secret-key: %s" % management_key,
            "  allow-remote: false",
            "  disable-control-panel: true",
            "  disable-auto-update-panel: true",
            "requests:",
            "  proxy-url: http://127.0.0.1:%d" % proxy.port,
            "access:",
            "  api-keys:",
            "    - %s" % client_key,
            "oauth:",
            "  auth-dir: %s" % (work / "auths"),
            "routing:",
            "  retry:",
            "    request-retry: 0",
            "plugins:",
            "  enabled: true",
            "  dir: %s" % (work / "plugins"),
            "  configs:",
            "    open-design:",
            "      enabled: true",
            "      data_dir: %s" % (work / "data"),
            "",
        ])
        (work / "config.yaml").write_text(config)
        (work / "config.yaml").chmod(0o600)

        env = {k: v for k, v in os.environ.items()
               if k.lower() not in ("http_proxy", "https_proxy", "all_proxy",
                                    "no_proxy", "ssl_cert_file", "ssl_cert_dir")}
        # Only this child process trusts the throwaway CA.
        env["SSL_CERT_FILE"] = str(ca_crt)
        env["NO_PROXY"] = "127.0.0.1,localhost"
        env["no_proxy"] = "127.0.0.1,localhost"
        log = (work / "cpa.log").open("wb")
        process = subprocess.Popen(
            [str(binary), "-config", str(work / "config.yaml"), "-local-model"],
            cwd=str(work), stdout=log, stderr=log, start_new_session=True, env=env)
        opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))

        def request(path, body=None, method=None, timeout=60):
            # /v0/management/* needs the management key; /v1/* needs a client
            # access.api-keys entry. Mixing them yields CPA's own 401.
            data = json.dumps(body).encode() if body is not None else None
            token = management_key if path.startswith("/v0/") else client_key
            req = urllib.request.Request(
                "http://127.0.0.1:%d%s" % (port, path), data=data, method=method,
                headers={"Authorization": "Bearer " + token,
                         "Content-Type": "application/json"})
            with opener.open(req, timeout=timeout) as resp:
                return resp.status, resp.read(), dict(resp.headers)

        deadline = time.monotonic() + 60
        while time.monotonic() < deadline:
            if process.poll() is not None:
                raise AssertionError("CPA exited early; log: %s" % (work / "cpa.log"))
            try:
                status, raw, _ = request("/v0/management/plugins")
                if status == 200 and any(
                        x["id"] == PLUGIN_ID and x["registered"]
                        for x in json.loads(raw)["plugins"]):
                    break
            except Exception:  # noqa: BLE001 - startup race
                pass
            time.sleep(0.25)
        else:
            raise AssertionError("CPA never registered the plugin; log: %s" % (work / "cpa.log"))

        stage_failures = []

        # ---------------- standard UI flow ----------------
        # 1. add the key first, exactly like the console does.
        status, raw, _ = request(API + "/credentials",
                                 {"label": "fixture", "api_key": FIXTURE_KEY})
        require(status == 201, "credential create returned %s: %s" % (status, raw))
        credential_id = json.loads(raw)["id"]
        require(FIXTURE_KEY not in raw.decode(), "credential response leaked the key")
        evidence["ui_flow"] = {"credential_create": {"status": status,
                                                     "leaked_key": False}}

        # 2. discover upstream models through the real host HTTP path.
        status, raw, _ = request(API + "/upstream-models?credential_id=" + credential_id)
        require(status == 200, "discover returned %s: %s" % (status, raw[:300]))
        discovered = [m["id"] for m in json.loads(raw)["data"]]
        require(discovered == ["deepseek-v4.1-flash", "glm-5.3-flash"], discovered)
        evidence["ui_flow"]["discover"] = {"ids": discovered}

        # 3. save the mapping AFTER the key exists; CPA must refresh the
        #    registration so the alias becomes routable without a restart.
        status, raw, _ = request(API + "/models", {"models": [
            {"id": "deepseek-flash", "upstream_id": "deepseek-v4.1-flash"}]})
        require(status == 200, "models POST returned %s: %s" % (status, raw))

        def client_models(timeout=30, want=("deepseek-flash",)):
            """Poll GET /v1/models until the alias shows up (or not)."""
            deadline = time.monotonic() + timeout
            seen = None
            while time.monotonic() < deadline:
                status, raw, _ = request("/v1/models", method="GET")
                if status == 200:
                    seen = sorted(m["id"] for m in json.loads(raw).get("data", []))
                    if all(w in seen for w in want) or not want:
                        return seen
                time.sleep(0.25)
            return seen

        seen = client_models()
        evidence["ui_flow"]["alias_after_mapping"] = seen
        if not seen or "deepseek-flash" not in seen:
            stage_failures.append(
                "alias not routable after adding the mapping (host registration "
                "not refreshed): /v1/models=%s" % seen)

        # ---- GET /models through host.http.do over the CONNECT proxy ----
        try:
            status, raw, _ = request(API + "/upstream-models?credential_id=" + credential_id)
        except urllib.error.HTTPError as exc:
            status, raw = exc.code, exc.read()
        if status != 200:
            # Distinguish "our fake upstream failed" from "the plugin mangled a
            # good upstream response". If the body is the origin's own JSON, the
            # loopback path already worked and the failure is host-result decoding.
            hit = [r for r in origin.requests if r["path"].endswith("/models")]
            evidence["host_upstream_models"] = {
                "status": "BLOCKED_BY_CORE_BUG" if hit and b'"object"' in raw else "FAIL",
                "surfaced_status": status,
                "surfaced_body_head": raw.decode(errors="replace")[:200],
                "origin_was_reached": bool(hit),
                "origin_returned_200": bool(hit and hit[-1]["tls"]),
                "origin_saw_bearer": bool(hit and hit[-1]["authorization"]),
                "via_connect_proxy": proxy.connects,
            }
        else:
            require([m["id"] for m in json.loads(raw)["data"]] ==
                    ["deepseek-v4.1-flash", "glm-5.3-flash"], raw)
            models_hit = [r for r in origin.requests if r["path"].endswith("/models")]
            require(models_hit and models_hit[-1]["tls"], "upstream not reached over TLS")
            require(models_hit[-1]["authorization"] == "Bearer " + FIXTURE_KEY,
                    "GET /models lost the bearer key")
            evidence["host_upstream_models"] = {
                "status": "PASS",
                "ids": [m["id"] for m in json.loads(raw)["data"]],
                "url_origin": "amr-link.open-design.ai:443",
                "tls": True,
                "authorization_present": True,
                "via_connect_proxy": "amr-link.open-design.ai:443" in proxy.connects,
            }

        # ---- 2. POST /chat/completions, non-streaming, 200 + tools + usage ----
        try:
            status, raw, _ = request("/v1/chat/completions", {
                "model": "deepseek-flash",
                "messages": [{"role": "user", "content": "hi"}],
                "tools": [{"type": "function", "function": {
                    "name": "lookup", "parameters": {"type": "object"}}}],
                "stream": False,
            })
            sent_hits = [r for r in origin.requests
                         if r["path"].endswith("/chat/completions")]
            sent = json.loads(sent_hits[-1]["body"]) if sent_hits else {}
            if status == 200:
                body = json.loads(raw)
                require(body["model"] == "deepseek-flash", body)
                require(body["usage"]["total_tokens"] == 19, body)
                require(body["choices"][0]["message"]["tool_calls"][0]["function"]["name"]
                        == "lookup", body["choices"])
                require(sent.get("model") == "deepseek-v4.1-flash", sent)
                require("tools" in sent, "tools dropped")
                evidence["host_chat_completions"] = {
                    "status": "PASS", "upstream_model": sent["model"],
                    "alias": body["model"], "tools_forwarded": True,
                    "usage": body["usage"]}
            else:
                evidence["host_chat_completions"] = {
                    "status": "BLOCKED_BY_CORE_BUG" if sent_hits else "FAIL",
                    "surfaced_status": status,
                    "surfaced_body_head": raw.decode(errors="replace")[:200],
                    "origin_was_reached": bool(sent_hits),
                    "origin_saw_upstream_model": sent.get("model") if sent else None,
                    "origin_saw_tools": "tools" in sent}
        except urllib.error.HTTPError as exc:
            evidence["host_chat_completions"] = {
                "status": "FAIL", "surfaced_status": exc.code,
                "surfaced_body_head": exc.read().decode(errors="replace")[:200]}

        # ---- 3. POST /chat/completions, streaming SSE ----
        try:
            status, raw, _ = request("/v1/chat/completions", {
                "model": "deepseek-flash",
                "messages": [{"role": "user", "content": "hi"}],
                "stream": True,
            })
            frames = [json.loads(line[6:].strip()) for line in raw.decode().splitlines()
                      if line.startswith("data: ") and line[6:].strip() not in ("", "[DONE]")]
            require(status == 200, "stream returned %s" % status)
            require(len(frames) >= 3, "expected >=3 SSE frames, got %r" % raw[:300])
            require(any(f["choices"][0].get("delta", {}).get("content") == "Hello"
                        for f in frames), frames)
            require(frames[-1]["usage"]["total_tokens"] == 26, frames[-1])
            require(all(f["model"] == "deepseek-flash" for f in frames),
                    "stream lost the alias")
            evidence["host_chat_stream"] = {"status": "PASS", "frames": len(frames),
                                            "alias_restored": True,
                                            "usage": frames[-1]["usage"]}
        except (urllib.error.HTTPError, AssertionError) as exc:
            detail = exc.read().decode(errors="replace") if isinstance(
                exc, urllib.error.HTTPError) else str(exc)
            evidence["host_chat_stream"] = {"status": "FAIL",
                                            "detail": detail[:200]}

        # ---- 4. upstream 401 / 429 status preserved end to end ----
        # ---- 5. upstream 401 / 429, each in its own isolated CPA process ----
        # A real 401 or 429 makes CPA cool the credential down, so a second
        # request in the same process would be answered by the host
        # ("auth_unavailable") and prove nothing about the plugin. Each error
        # case therefore gets a fresh host, credential and model mapping.
        evidence["host_http_status"] = {}
        for code in (429, 401):
            case = run_cpa_live_error(binary, artifact, code)
            evidence["host_http_status"][str(code)] = case

        # ---- 6. disable -> enable must resync the runtime ----
        # If the registration never came up there is nothing to withdraw, so these
        # legs are reported as skipped instead of inventing a verdict.
        registration_live = bool(seen and "deepseek-flash" in seen)
        if registration_live:
            status, raw, _ = request(API + "/credentials",
                                     {"id": credential_id, "disabled": True})
            require(status == 200, "disable returned %s: %s" % (status, raw))
        else:
            status = 0
        if not registration_live:
            evidence["ui_flow"]["disable_enable_delete"] = {
                "status": "SKIPPED",
                "reason": "alias never became routable; see ui_flow_failures"}
        else:
            disabled_models = client_models(timeout=15, want=())
            evidence["ui_flow"]["after_disable"] = {
                "models": disabled_models,
                "alias_withdrawn": "deepseek-flash" not in (disabled_models or []),
            }
            if "deepseek-flash" in (disabled_models or []):
                stage_failures.append("disabled credential still advertises the "
                                      "alias: %s" % disabled_models)

        if registration_live:
            status, raw, _ = request(API + "/credentials",
                                     {"id": credential_id, "disabled": False})
            require(status == 200, "enable returned %s: %s" % (status, raw))
            seen = client_models()
            evidence["ui_flow"]["after_enable"] = seen
            if not seen or "deepseek-flash" not in seen:
                stage_failures.append(
                    "alias did not come back after re-enabling: %s" % seen)
            try:
                code_, body_, _ = request("/v1/chat/completions", {
                    "model": "deepseek-flash",
                    "messages": [{"role": "user", "content": "hi"}],
                    "stream": False})
            except urllib.error.HTTPError as exc:
                code_, body_ = exc.code, exc.read()
            evidence["ui_flow"]["after_enable_callable"] = code_ == 200
            if code_ != 200:
                stage_failures.append(
                    "after re-enable the alias is not callable: %s %s"
                    % (code_, body_.decode(errors="replace")[:160]))

        status, raw, _ = request(API + "/credentials?id=" + credential_id, method="DELETE")
        require(status == 200, "deleted credential returned %s: %s" % (status, raw))
        require(not list((work / "auths").glob("*.json")), "credential file survived delete")
        after_delete = client_models(timeout=15, want=())
        try:
            code_, body_, _ = request("/v1/chat/completions", {
                "model": "deepseek-flash",
                "messages": [{"role": "user", "content": "hi"}],
                "stream": False})
        except urllib.error.HTTPError as exc:
            code_, body_ = exc.code, exc.read()
        unreachable = {"status": code_,
                       "body_head": body_.decode(errors="replace")[:120]}
        evidence["host_credential_delete"] = {
            "auth_dir_empty": True,
            "models_after_delete": after_delete,
            "alias_withdrawn": "deepseek-flash" not in (after_delete or []),
            "call_after_delete": unreachable,
        }
        if "deepseek-flash" in (after_delete or []):
            stage_failures.append(
                "delete_model_list: credential removed and no longer callable, but "
                "CPA still advertises the alias in /v1/models: %s" % after_delete)
        if unreachable["status"] == 200:
            stage_failures.append(
                "deleted credential is still callable: %s" % unreachable)

        # ---- host HTTP redirect: is Authorization re-sent across origins? ----
        evidence["host_http_redirect"] = probe_redirect(
            request, origin, proxy, credential_id, proxy.allow_more)

        # ---- final verdict: a blocked or skipped leg can never become PASS ----
        blockers = sorted(name for name in (
            "host_upstream_models", "host_chat_completions", "host_chat_stream")
            if evidence.get(name, {}).get("status") == "FAIL")
        for code, case in (evidence.get("host_http_status") or {}).items():
            if case.get("status") != "PASS":
                blockers.append("host_http_status." + code)
        if not registration_live:
            blockers.append("ui_flow.registration")
        if stage_failures:
            evidence["ui_flow_failures"] = stage_failures
            blockers.append("ui_flow")
        require(not blockers, "live legs failed: %s" % blockers)
        evidence["status"] = "PASS"
        return evidence
    except AssertionError as exc:
        evidence.setdefault("status", "FAIL")
        evidence.setdefault("failure", str(exc)[:600])
        return evidence
    finally:
        if process is not None and process.poll() is None:
            process.terminate()
            try:
                process.wait(timeout=20)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait(timeout=10)
        shutil.rmtree(work, ignore_errors=True)


def run_cpa_live_error(binary, artifact, code):
    """One upstream error status, in a throwaway CPA process.

    CPA cools a credential down after a real 401/429, so statuses cannot share a
    process. Each call boots its own host, credential and mapping, and asserts
    the request actually reached the fake upstream before judging the status.
    """
    return _live_case(binary, artifact, fail_status=code)


def _live_case(binary, artifact, fail_status=None):
    work = Path(tempfile.mkdtemp(prefix="opendesign-cpa-case-"))
    process = proxy = origin = log = None
    result = {"status": "FAIL"}
    try:
        ca_crt, leaf_crt, leaf_key = make_test_ca(work)
        origin = TlsFakeOrigin(leaf_crt, leaf_key)
        proxy = LocalProxy()
        origin.start()
        proxy.origin_port = origin.port
        proxy.start()
        if fail_status is not None:
            origin.fail = (fail_status, json.dumps(
                {"error": {"message": "denied %d" % fail_status,
                           "type": "auth_error" if fail_status == 401 else "rate_limit"}}))
        (work / "plugins/linux/amd64").mkdir(parents=True)
        shutil.copyfile(artifact, work / "plugins/linux/amd64/open-design.so")
        with socket.socket() as probe:
            probe.bind(("127.0.0.1", 0))
            port = probe.getsockname()[1]
        management_key, client_key = "smoke-case-mgmt-key", "smoke-case-client-key"
        (work / "config.yaml").write_text("\n".join([
            "config-version: 8", "server:", "  host: 127.0.0.1", "  port: %d" % port,
            "management:", "  secret-key: %s" % management_key,
            "  allow-remote: false", "  disable-control-panel: true",
            "  disable-auto-update-panel: true",
            "requests:", "  proxy-url: http://127.0.0.1:%d" % proxy.port,
            "access:", "  api-keys:", "    - %s" % client_key,
            "oauth:", "  auth-dir: %s" % (work / "auths"),
            "routing:", "  retry:", "    request-retry: 0",
            "plugins:", "  enabled: true", "  dir: %s" % (work / "plugins"),
            "  configs:", "    %s:" % PLUGIN_ID, "      enabled: true",
            "      data_dir: %s" % (work / "data"), ""]))
        (work / "config.yaml").chmod(0o600)
        env = {k: v for k, v in os.environ.items()
               if k.lower() not in ("http_proxy", "https_proxy", "all_proxy",
                                    "no_proxy", "ssl_cert_file", "ssl_cert_dir")}
        env["SSL_CERT_FILE"] = str(ca_crt)
        env["NO_PROXY"] = env["no_proxy"] = "127.0.0.1,localhost"
        log = (work / "cpa.log").open("wb")
        process = subprocess.Popen(
            [str(binary), "-config", str(work / "config.yaml"), "-local-model"],
            cwd=str(work), stdout=log, stderr=log, start_new_session=True, env=env)
        opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))

        def request(path, body=None, method=None, timeout=60):
            data = json.dumps(body).encode() if body is not None else None
            token = management_key if path.startswith("/v0/") else client_key
            req = urllib.request.Request(
                "http://127.0.0.1:%d%s" % (port, path), data=data, method=method,
                headers={"Authorization": "Bearer " + token,
                         "Content-Type": "application/json"})
            with opener.open(req, timeout=timeout) as resp:
                return resp.status, resp.read(), dict(resp.headers)

        deadline = time.monotonic() + 60
        while time.monotonic() < deadline:
            if process.poll() is not None:
                raise AssertionError("case CPA exited early")
            try:
                status, raw, _ = request("/v0/management/plugins")
                if status == 200 and any(x["id"] == PLUGIN_ID and x["registered"]
                                         for x in json.loads(raw)["plugins"]):
                    break
            except Exception:  # noqa: BLE001 - startup race
                pass
            time.sleep(0.25)
        else:
            raise AssertionError("case CPA never registered the plugin")

        status, raw, _ = request(API + "/credentials",
                                 {"label": "fixture", "api_key": FIXTURE_KEY})
        require(status == 201, "case credential create %s" % status)
        credential_id = json.loads(raw)["id"]
        status, raw, _ = request(API + "/models", {"models": [
            {"id": "deepseek-flash", "upstream_id": "deepseek-v4.1-flash"}]})
        require(status == 200, "case models POST %s" % status)
        deadline = time.monotonic() + 30
        while time.monotonic() < deadline:
            status, raw, _ = request("/v1/models", method="GET")
            if status == 200 and "deepseek-flash" in {
                    m["id"] for m in json.loads(raw).get("data", [])}:
                break
            time.sleep(0.25)

        before = len(origin.requests)
        try:
            status, raw, _ = request("/v1/chat/completions", {
                "model": "deepseek-flash",
                "messages": [{"role": "user", "content": "hi"}],
                "stream": False})
            body = raw
        except urllib.error.HTTPError as exc:
            status, body = exc.code, exc.read()
        result = {
            "status": "PASS" if status == fail_status else "FAIL",
            "upstream_status": fail_status,
            "surfaced_status": status,
            "reached_upstream": len(origin.requests) > before,
            "body_head": body.decode(errors="replace")[:160],
            "isolated_process": True,
        }
        require(result["reached_upstream"],
                "%d was answered by the host, not upstream" % fail_status)
        require(status == fail_status,
                "upstream %d surfaced as %s: %s"
                % (fail_status, status, result["body_head"]))
        return result
    except AssertionError as exc:
        result.setdefault("failure", str(exc)[:300])
        return result
    finally:
        if process is not None and process.poll() is None:
            process.terminate()
            try:
                process.wait(timeout=20)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait(timeout=10)
        if log is not None:
            log.close()
        for server in (proxy, origin):
            if server is not None:
                server.stop()
        shutil.rmtree(work, ignore_errors=True)


def probe_redirect(request, origin, proxy, credential_id, allow_redirect_host):
    """Ask the real host to follow a 307 to a *different* origin and see if the
    bearer key is re-sent. Both hosts reach the same loopback fake origin through
    the same CONNECT proxy, so nothing can leave the box."""
    target = "https://redirect-probe.example.test/v1/models"
    allow_redirect_host("redirect-probe.example.test:443")
    origin.redirect_to = target
    try:
        before = len(origin.requests)
        try:
            status, raw, _ = request(API + "/upstream-models?credential_id=" + credential_id)
        except urllib.error.HTTPError as exc:
            status = exc.code
        hops = origin.requests[before:]
        first = [h for h in hops if h["host"] == "amr-link.open-design.ai"]
        second = [h for h in hops if (h.get("host") or "").startswith("redirect-probe")]
        result = {
            "status_surfaced": status,
            "first_hop": {
                "host": first[0]["host"] if first else None,
                "authorization_present": bool(first and first[0]["authorization"]),
            },
            "redirected_hop": {
                "reached": bool(second),
                "host": second[0]["host"] if second else None,
                "authorization_present": bool(second and second[0]["authorization"]),
            },
            "via_connect_proxy": "redirect-probe.example.test:443" in proxy.connects,
        }
        if second:
            result["verdict"] = ("Authorization forwarded across origin"
                                 if second[0]["authorization"]
                                 else "Authorization stripped across origin")
        else:
            result["verdict"] = "redirect not followed (host refused cross-origin 307)"
        return result
    finally:
        origin.redirect_to = None


def _dechunk(raw):
    out = b""
    while raw:
        line, _, rest = raw.partition(b"\r\n")
        try:
            size = int(line.split(b";")[0], 16)
        except ValueError:
            return out
        if size == 0:
            return out
        out += rest[:size]
        raw = rest[size + 2:]
    return out


class Plugin:
    def __init__(self, path):
        self.library = ctypes.CDLL(str(path))
        init = self.library.cliproxy_plugin_init
        init.argtypes = [ctypes.c_void_p, ctypes.POINTER(PluginAPI)]
        init.restype = ctypes.c_int
        self.api = PluginAPI()
        require(init(None, ctypes.byref(self.api)) == 0, "cliproxy_plugin_init failed")
        require(self.api.abi_version == 1, "abi_version != 1")

    def raw_call(self, method, request):
        raw = json.dumps(request).encode()
        payload = (ctypes.c_uint8 * len(raw)).from_buffer_copy(raw)
        out = Buffer()
        code = self.api.call(method.encode(), payload, len(raw), ctypes.byref(out))
        if not out.ptr:
            raise RuntimeError("%s: empty payload (code=%d)" % (method, code))
        try:
            envelope = json.loads(ctypes.string_at(out.ptr, out.length))
        finally:
            self.api.free_buffer(out.ptr, out.length)
        return code, envelope

    def call(self, method, request):
        code, envelope = self.raw_call(method, request)
        if not envelope.get("ok") or code:
            raise RuntimeError("%s failed: code=%d envelope=%s" % (method, code, envelope))
        return envelope.get("result", {})

    def management(self, path, method="GET", query=None, body=None):
        request = {"Method": method, "Path": path, "Query": query or {},
                   "Headers": {}, "host_callback_id": "mgmt-cb"}
        if body is not None:
            request["Body"] = base64.b64encode(json.dumps(body).encode()).decode()
        result = self.call("management.handle", request)
        status = result.get("StatusCode")
        raw = base64.b64decode(result.get("Body") or b"")
        return status, raw, result.get("Headers", {})

    def shutdown(self):
        self.api.shutdown()


# --------------------------------------------------------------------- scenario
def run(artifact):
    upstream = FakeUpstream().start()
    work = Path(tempfile.mkdtemp(prefix="opendesign-abi-"))
    evidence = {}
    plugin = None
    try:
        host = FakeHost(upstream, work / "auths")

        class HostAPI(ctypes.Structure):
            _fields_ = [("abi_version", ctypes.c_uint32), ("host_ctx", ctypes.c_void_p),
                        ("call", HostCallFn), ("free_buffer", FreeFn)]

        host_api = HostAPI()
        host_api.abi_version = 1
        host_api.host_ctx = None
        host_api.call = HostCallFn(host.callback)
        host_api.free_buffer = FreeFn(lambda ptr, length: None)

        # Exactly what CPA's loader_unix.go does: dlopen, then dlsym the single
        # cliproxy_plugin_init export and hand it the host function table.
        lib = ctypes.CDLL(str(artifact))
        init = lib.cliproxy_plugin_init
        init.argtypes = [ctypes.POINTER(HostAPI), ctypes.POINTER(PluginAPI)]
        init.restype = ctypes.c_int
        api = PluginAPI()
        require(init(ctypes.byref(host_api), ctypes.byref(api)) == 0,
                "cliproxy_plugin_init(host) failed")
        require(api.abi_version == 1, "abi_version != 1")
        plugin = Plugin.__new__(Plugin)
        plugin.library, plugin.api = lib, api

        config = {"enabled": True, "data_dir": str(work / "data"), "base_url":
                  "https://amr-link.open-design.ai/v1"}
        yaml = "".join("%s: %s\n" % (k, json.dumps(v) if isinstance(v, str) else v)
                       for k, v in config.items())
        registration = plugin.call("plugin.register", {
            "schema_version": 6, "config_yaml": base64.b64encode(yaml.encode()).decode()})
        require(registration["schema_version"] == 6, registration)
        require(registration["metadata"]["Version"] == "0.1.0", registration["metadata"])
        evidence["registration"] = {"name": registration["metadata"]["Name"],
                                    "version": registration["metadata"]["Version"],
                                    "capabilities": sorted(registration["capabilities"])}

        registered = plugin.call("management.register", {})
        evidence["routes"] = sorted(r["Path"] for r in registered["routes"])

        status, raw, _ = plugin.management(API + "/status")
        require(status == 200, "status route returned %s" % status)
        body = json.loads(raw)
        require(body["version"] == "0.1.0", body)
        require(body["base_url"] == "https://amr-link.open-design.ai/v1", body)
        require(body["models"] == [] and body["credentials"] == [], body)
        evidence["status_before"] = body

        status, raw, _ = plugin.management(API + "/credentials", method="POST",
                                           body={"label": "fixture", "api_key": FIXTURE_KEY})
        require(status == 201, "credential create returned %s" % status)
        created = json.loads(raw)
        require("api_key" not in raw.decode(), "credential response echoed the key")
        require(FIXTURE_KEY not in raw.decode(), "credential response leaked the key")
        evidence["credential"] = created
        credential_id = created["id"]

        # P3: key never leaves through status either.
        status, raw, _ = plugin.management(API + "/status")
        require(FIXTURE_KEY not in raw.decode(), "status leaked the api key")
        require(json.loads(raw)["credentials"][0]["id"] == credential_id, raw)

        # Fake upstream must have seen the saved key through host.auth.save.
        saved = [v for v in host.auth_files.values()]
        require(any(d.get("api_key") == FIXTURE_KEY for d in saved), saved)

        status, raw, _ = plugin.management(API + "/upstream-models",
                                           query={"credential_id": [credential_id]})
        require(status == 200, "upstream-models returned %s: %s" % (status, raw[:400]))
        models = json.loads(raw)["data"]
        evidence["upstream_models"] = [m["id"] for m in models]
        require([m["id"] for m in models] == ["deepseek-v4.1-flash", "glm-5.3-flash"], models)
        models_get = [r for r in upstream.requests if r["path"].endswith("/models")]
        require(models_get, "plugin never issued GET /models")
        require(models_get[-1]["authorization"] == "Bearer " + FIXTURE_KEY,
                "GET /models must carry the stored bearer key")

        status, raw, _ = plugin.management(
            API + "/models", method="POST",
            body={"models": [{"id": "deepseek-flash", "upstream_id": "deepseek-v4.1-flash"}]})
        require(status == 200, "model import returned %s: %s" % (status, raw))
        evidence["models"] = json.loads(raw)["models"]

        auth_parse = plugin.call("auth.parse", {
            "Provider": PLUGIN_ID, "FileName": credential_id + ".json",
            "RawJSON": base64.b64encode(json.dumps(
                host.auth_files[credential_id + ".json"]).encode()).decode(),
            "Host": {"AuthDir": str(host.auth_dir)}})
        require(auth_parse.get("Handled") is True, auth_parse)
        require(FIXTURE_KEY not in json.dumps(auth_parse), "auth.parse echoed the key")
        evidence["auth_parse"] = {"handled": True,
                                  "auth_id": auth_parse["Auth"]["ID"]}

        model_reg = plugin.call("model.for_auth", {"Provider": PLUGIN_ID})
        require(model_reg["Provider"] == PLUGIN_ID, model_reg)
        require([m["ID"] for m in model_reg["Models"]] == ["deepseek-flash"], model_reg)
        evidence["model_registration"] = model_reg["Models"]

        storage = base64.b64encode(json.dumps(
            host.auth_files[credential_id + ".json"]).encode()).decode()

        # ---- executor.execute (buffered, JSON tools + usage) ----
        result = plugin.call("executor.execute", {
            "AuthID": credential_id, "AuthProvider": PLUGIN_ID,
            "Model": "deepseek-flash", "Format": "chat-completions",
            "Stream": False, "Payload": base64.b64encode(json.dumps({
                "model": "deepseek-flash",
                "messages": [{"role": "user", "content": "hi"}],
                "tools": [{"type": "function", "function": {
                    "name": "lookup", "parameters": {"type": "object"}}}],
            }).encode()).decode(),
            "StorageJSON": storage, "host_callback_id": "exec-cb"})
        payload = json.loads(base64.b64decode(result["Payload"]))
        require(payload["model"] == "deepseek-flash", "alias not restored: %s" % payload)
        require(payload["usage"]["total_tokens"] == 19, payload["usage"])
        require(payload["choices"][0]["message"]["tool_calls"][0]["function"]["name"] == "lookup",
                payload["choices"])
        chat_posts = [r for r in upstream.requests if r["path"].endswith("/chat/completions")]
        require(chat_posts, "plugin never issued POST /chat/completions")
        sent = json.loads(chat_posts[-1]["body"])
        require(sent["model"] == "deepseek-v4.1-flash", "model mapping not applied: %s" % sent)
        require(chat_posts[-1]["authorization"] == "Bearer " + FIXTURE_KEY,
                "POST /chat/completions must carry the stored bearer key")
        require("tools" in sent, "tools were dropped: %s" % sent)
        evidence["execute"] = {"upstream_model": sent["model"],
                               "alias": payload["model"],
                               "usage": payload["usage"],
                               "upstream_path": chat_posts[-1]["path"]}

        # ---- executor.execute_stream (multi chunk, JSON framing) ----
        result = plugin.call("executor.execute_stream", {
            "AuthID": credential_id, "AuthProvider": PLUGIN_ID,
            "Model": "deepseek-flash", "Format": "chat-completions",
            "Stream": True, "stream_id": "client-1", "host_callback_id": "stream-cb",
            "Payload": base64.b64encode(json.dumps({
                "model": "deepseek-flash",
                "messages": [{"role": "user", "content": "hi"}],
                "stream": True,
            }).encode()).decode(),
            "StorageJSON": storage, "Metadata": {"request_path": "/v1/chat/completions"}})
        require("Headers" in result, result)
        deadline = time.monotonic() + 5
        while time.monotonic() < deadline and not host.closed_clients:
            time.sleep(0.02)
        require(host.closed_clients, "plugin never closed the client stream")
        emitted = host.client_streams.get("client-1", [])
        frames = [json.loads(f) for f in emitted]
        require(len(frames) == 4, "expected 4 upstream frames, got %d" % len(frames))
        require(frames[0]["choices"][0]["delta"]["role"] == "assistant", frames[0])
        require(frames[1]["choices"][0]["delta"]["content"] == "Hello", frames[1])
        require(frames[-1]["choices"][0]["finish_reason"] == "stop", frames[-1])
        require(frames[-1]["usage"]["total_tokens"] == 26, frames[-1]["usage"])
        require(all(f["model"] == "deepseek-flash" for f in frames),
                "stream did not restore the client alias: %s" % frames)
        # CPA chat-completions framing: raw JSON frames, no `data:` prefix.
        require(all(not f.startswith(b"data:") for f in emitted),
                "chat-completions frames must not be SSE-wrapped: %r" % emitted[:1])
        close = host.closed_clients[-1]
        require(close["error"] is None, "clean stream closed with error: %s" % close)
        require(host.closed_upstreams, "upstream host stream was never closed")
        evidence["execute_stream"] = {"frames": len(frames),
                                      "framing": "raw-json",
                                      "closed_cleanly": True,
                                      "upstream_closed": host.closed_upstreams[-1]}

        # ---- /v1/messages framing branch: SSE wrapped payloads ----
        before = len(host.client_streams.get("client-2", []))
        result = plugin.call("executor.execute_stream", {
            "AuthID": credential_id, "AuthProvider": PLUGIN_ID,
            "Model": "deepseek-flash", "Format": "claude",
            "Stream": True, "stream_id": "client-2", "host_callback_id": "stream-cb-2",
            "Payload": base64.b64encode(json.dumps({
                "model": "deepseek-flash",
                "messages": [{"role": "user", "content": "hi"}],
                "stream": True,
            }).encode()).decode(),
            "StorageJSON": storage, "Metadata": {"request_path": "/v1/messages"}})
        deadline = time.monotonic() + 5
        while time.monotonic() < deadline and len(host.closed_clients) < 2:
            time.sleep(0.02)
        messages_frames = host.client_streams.get("client-2", [])[before:]
        require(messages_frames, "no /v1/messages frames emitted")
        require(all(f.startswith(b"data: ") and f.endswith(b"\n\n") for f in messages_frames),
                "/v1/messages frames must be SSE-wrapped: %r" % messages_frames[:1])
        evidence["messages_framing"] = {"frames": len(messages_frames),
                                        "framing": "sse-wrapped"}

        # ---- HTTP 401 / 429 pass-through status ----
        statuses = {}
        for code in (401, 429):
            upstream.fail["/chat/completions"] = (
                code, json.dumps({"error": {"message": "denied %d" % code,
                                            "type": "auth_error"}}))
            rc, envelope = plugin.raw_call("executor.execute", {
                "AuthID": credential_id, "AuthProvider": PLUGIN_ID,
                "Model": "deepseek-flash", "Format": "chat-completions",
                "Payload": base64.b64encode(json.dumps({
                    "model": "deepseek-flash",
                    "messages": [{"role": "user", "content": "hi"}],
                }).encode()).decode(),
                "StorageJSON": storage, "host_callback_id": "fail-cb"})
            require(not envelope.get("ok"), "%d did not surface as an RPC error" % code)
            detail = envelope.get("error") or {}
            require(detail.get("http_status") == code,
                    "%d lost its upstream status: %s" % (code, detail))
            require(FIXTURE_KEY not in env_text(envelope),
                    "error envelope leaked the api key")
            statuses[str(code)] = {"http_status": detail["http_status"],
                                   "code": detail.get("code")}
        upstream.fail.clear()
        evidence["http_status"] = statuses

        # Shutdown must close host streams and stay idempotent.
        plugin.shutdown()
        plugin = None
        evidence["shutdown"] = {"host_streams_closed": bool(host.closed_upstreams)}
        require(not host.callback_failures,
                "fake host callback raised: %s" % host.callback_failures)
        evidence["callback_failures"] = host.callback_failures
        evidence["status"] = "PASS"
        return evidence
    finally:
        if plugin is not None:
            try:
                plugin.shutdown()
            except Exception:  # noqa: BLE001 - best effort
                pass
        upstream.stop()
        shutil.rmtree(work, ignore_errors=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("artifact", help="path to dist/open-design.so")
    parser.add_argument("--json-report", default=None)
    parser.add_argument("--cpa-binary", nargs="?", const=CPA_DEFAULT, default=None,
                        help="also boot a real local CPA host (offline routes only) "
                             "from this binary; defaults to %s" % CPA_DEFAULT)
    parser.add_argument("--cpa-live", nargs="?", const=CPA_DEFAULT, default=None,
                        help="boot a real local CPA host wired to a loopback CONNECT "
                             "proxy + private-CA TLS fake origin, so upstream-models "
                             "and chat completions run end-to-end offline; defaults "
                             "to the same binary as --cpa-binary")
    args = parser.parse_args()
    artifact = Path(args.artifact).resolve()
    require(artifact.is_file(), "artifact not found: %s" % artifact)
    evidence = {}
    failure = None
    try:
        evidence = run(artifact)
    except AssertionError as exc:
        failure = str(exc)
        evidence = {"ctypes": {"status": "FAIL", "failure": failure[:600]}}
    if args.cpa_binary:
        evidence["cpa_host"] = run_cpa(artifact, args.cpa_binary)
    if args.cpa_live:
        evidence["cpa_live"] = run_cpa_live(artifact, args.cpa_live)
    if args.json_report:
        Path(args.json_report).write_text(json.dumps(evidence, indent=2) + "\n")
    if failure:
        print("FAIL:", failure)
        blocked_now = [k for k, v in evidence.items()
                       if isinstance(v, dict) and v.get("status") == "FAIL"]
        print("failing legs:", ", ".join(blocked_now) or "ctypes")
        return 1
    print(json.dumps(evidence, indent=2))
    blocked = [name for name, value in evidence.items()
               if isinstance(value, dict) and value.get("status") == "FAIL"]
    for name, value in evidence.items():
        if isinstance(value, dict) and value.get("status") == "FAIL":
            blocked += ["%s.%s" % (name, leg) for leg in value.get("core_blocked_legs", [])]
    if blocked:
        print("\nsmoke-abi: FAIL (see %s)" % ", ".join(blocked))
        return 1
    print("\nsmoke-abi: PASS")
    return 0


if __name__ == "__main__":
    sys.exit(main())
