# Copyright (c) 2026 Kristoffer Dalby
# SPDX-License-Identifier: BSD-3-Clause

import http.client
import json
import os
import pathlib
import sys
import time

connection = http.client.HTTPConnection(sys.argv[1], timeout=10)
connection.connect()
sock = connection.sock
assert sock is not None
port = sock.getsockname()[1]


def reconnect() -> None:
    raise RuntimeError("revocation probe reconnected")


connection.connect = reconnect
status = pathlib.Path("/run/revocation.json")
try:
    while True:
        phase = pathlib.Path("/run/revocation-phase").read_text().strip()
        codes: list[int] = []
        for method, path in [
            ("GET", "/debug/vars"),
            ("PUT", "/nar/revocation.nar"),
            ("GET", "/nix-cache-info"),
        ]:
            connection.request(method, path, body=b"" if method == "PUT" else None)
            response = connection.getresponse()
            codes.append(response.status)
            response.read()
            assert connection.sock is sock and sock.getsockname()[1] == port
        temporary = status.with_suffix(".tmp")
        temporary.write_text(json.dumps({"phase": phase, "statuses": codes, "port": port}))
        os.replace(temporary, status)
        time.sleep(0.2)
except Exception as exc:
    status.write_text(json.dumps({"error": repr(exc)}))
    raise
finally:
    connection.close()
