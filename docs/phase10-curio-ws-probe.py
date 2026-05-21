#!/usr/bin/env python3
"""Hit Curio's webui WebSocket RPC and dump NetSummary."""
import json
import socket
import struct
import base64
import os

# Minimal websocket client.
def ws_request(host, port, path, methods):
    s = socket.create_connection((host, port), timeout=10)
    key = base64.b64encode(os.urandom(16)).decode()
    handshake = (
        f"GET {path} HTTP/1.1\r\n"
        f"Host: {host}:{port}\r\n"
        "Upgrade: websocket\r\n"
        "Connection: Upgrade\r\n"
        f"Sec-WebSocket-Key: {key}\r\n"
        "Sec-WebSocket-Version: 13\r\n"
        "\r\n"
    )
    s.send(handshake.encode())
    # Read handshake response.
    buf = b""
    while b"\r\n\r\n" not in buf:
        chunk = s.recv(4096)
        if not chunk:
            break
        buf += chunk
    header_end = buf.index(b"\r\n\r\n") + 4
    leftover = buf[header_end:]
    if b"101 Switching" not in buf[:header_end]:
        print("WS handshake failed:", buf[:200].decode(errors="replace"))
        return None

    results = {}

    def send_frame(payload):
        data = payload.encode()
        header = bytearray()
        header.append(0x81)  # FIN + text
        mask_bit = 0x80
        ln = len(data)
        if ln < 126:
            header.append(mask_bit | ln)
        elif ln < (1 << 16):
            header.append(mask_bit | 126)
            header += struct.pack(">H", ln)
        else:
            header.append(mask_bit | 127)
            header += struct.pack(">Q", ln)
        mask = os.urandom(4)
        header += mask
        masked = bytes(b ^ mask[i % 4] for i, b in enumerate(data))
        s.send(bytes(header) + masked)

    def read_frame():
        nonlocal leftover
        def recv_exactly(n):
            nonlocal leftover
            while len(leftover) < n:
                chunk = s.recv(65536)
                if not chunk:
                    return None
                leftover += chunk
            out, leftover = leftover[:n], leftover[n:]
            return out

        hdr = recv_exactly(2)
        if not hdr:
            return None
        b1, b2 = hdr[0], hdr[1]
        ln = b2 & 0x7f
        if ln == 126:
            ln = struct.unpack(">H", recv_exactly(2))[0]
        elif ln == 127:
            ln = struct.unpack(">Q", recv_exactly(8))[0]
        payload = recv_exactly(ln)
        return payload

    for i, m in enumerate(methods):
        body = json.dumps({"jsonrpc":"2.0","method":m,"params":[],"id":i+1})
        send_frame(body)
    # Read N responses.
    for _ in range(len(methods)):
        frame = read_frame()
        if frame is None:
            break
        try:
            r = json.loads(frame)
            results[r.get("id")] = r
        except Exception as e:
            print("decode:", e, frame[:200])
    s.close()
    return results

methods = ["CurioWeb.NetSummary", "CurioWeb.SyncerState", "CurioWeb.Version"]
res = ws_request("192.168.2.32", 4701, "/api/webrpc/v0", methods)
print(json.dumps(res, indent=2))
