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
import socket
import sys
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
        name = ctypes.string_at(method).decode()
        raw = bytes(bytearray(request[:request_len])) if request_len else b""
        payload = json.loads(raw) if raw else {}
        try:
            result = self.dispatch(name, payload)
            envelope = {"ok": True, "result": result}
            code = 0
        except AssertionError as exc:
            raise
        except Exception as exc:  # noqa: BLE001 - surfaced to the plugin as an error
            envelope = {"ok": False, "error": {"code": "host_error",
                                               "message": str(exc),
                                               "http_status": 500}}
            code = 1
        data = json.dumps(envelope).encode()
        holder = ctypes.create_string_buffer(data, len(data))
        self._keepalive.append(holder)
        response[0].ptr = ctypes.cast(holder, ctypes.c_void_p)
        response[0].length = len(data)
        return code

    def dispatch(self, name, payload):
        self.calls.append({"method": name, "payload": payload})
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
            return {"status_code": status, "headers": headers,
                    "body": base64.b64encode(body).decode()}
        if name == "host.http.do_stream":
            status, headers, body = self._http(payload.get("method", "POST"),
                                               payload["url"], payload.get("headers"),
                                               payload.get("body") and
                                               base64.b64decode(payload["body"]))
            self.next_stream += 1
            stream_id = "upstream-%d" % self.next_stream
            self.upstream_streams[stream_id] = body
            return {"status_code": status, "headers": headers, "stream_id": stream_id}
        if name == "host.http.stream_read":
            stream_id = payload["stream_id"]
            require(stream_id in self.upstream_streams,
                    "stream_read on unknown stream %s" % stream_id)
            body = self.upstream_streams[stream_id]
            self.upstream_streams[stream_id] = b""
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


def run_cpa(artifact, binary, report_path=None):
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
            "    - smoke-client-fixture-key",
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
        evidence["skipped_online"] = ["upstream-models", "executor.execute",
                                      "executor.execute_stream"]
        evidence["status"] = "PASS"
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
        if report_path:
            Path(report_path).write_text(json.dumps(evidence, indent=2) + "\n")


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
def run(artifact, report_path=None):
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
        require(status == 200, "upstream-models returned %s: %s" % (status, raw))
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
        if report_path:
            Path(report_path).write_text(json.dumps(evidence, indent=2) + "\n")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("artifact", help="path to dist/open-design.so")
    parser.add_argument("--json-report", default=None)
    parser.add_argument("--cpa-binary", nargs="?", const=CPA_DEFAULT, default=None,
                        help="also boot a real local CPA host (offline routes only) "
                             "from this binary; defaults to %s" % CPA_DEFAULT)
    args = parser.parse_args()
    artifact = Path(args.artifact).resolve()
    require(artifact.is_file(), "artifact not found: %s" % artifact)
    try:
        evidence = run(artifact, args.json_report)
        if args.cpa_binary:
            evidence["cpa_host"] = run_cpa(artifact, args.cpa_binary)
    except AssertionError as exc:
        print("FAIL:", exc)
        return 1
    print(json.dumps(evidence, indent=2))
    print("\nsmoke-abi: PASS")
    return 0


if __name__ == "__main__":
    sys.exit(main())
