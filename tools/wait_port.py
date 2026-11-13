"""Wait until a local port accepts connections. Usage: wait_port.py <port> [seconds]"""
import socket
import sys
import time

port = int(sys.argv[1])
deadline = time.time() + (float(sys.argv[2]) if len(sys.argv) > 2 else 60)
start = time.time()
while time.time() < deadline:
    with socket.socket() as s:
        s.settimeout(1)
        if s.connect_ex(("127.0.0.1", port)) == 0:
            print(f"port {port} up after {time.time() - start:.0f}s")
            sys.exit(0)
    time.sleep(0.5)
print(f"port {port} not up")
sys.exit(1)
