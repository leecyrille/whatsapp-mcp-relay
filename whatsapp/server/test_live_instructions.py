"""Proves a new MCP session gets the current instructions, over the real HTTP transport."""
import asyncio
import socket
import threading
import time

import app_info
import pytest
from fastmcp import Client

import server


def free_port():
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return sock.getsockname()[1]


async def session_details(url):
    async with Client(url) as client:
        tools = await client.list_tools()
        return client.initialize_result.instructions, {tool.name for tool in tools}


@pytest.fixture(scope="module")
def url():
    port = free_port()
    thread = threading.Thread(
        target=lambda: server.mcp.run(
            transport="http", host="127.0.0.1", port=port, path="/mcp", show_banner=False
        ),
        daemon=True,
    )
    thread.start()
    for _ in range(100):
        try:
            socket.create_connection(("127.0.0.1", port), timeout=0.2).close()
            break
        except OSError:
            time.sleep(0.1)
    return f"http://127.0.0.1:{port}/mcp"


def test_each_new_session_gets_current_instructions(url, tmp_path, monkeypatch):
    notes = tmp_path / "notes.md"
    notes.write_text("Note version one", encoding="utf-8")
    monkeypatch.setattr(app_info, "NOTES", notes)
    monkeypatch.setattr(
        app_info.build_instructions, "__defaults__", (app_info.DIST, notes)
    )
    first, tools = asyncio.run(session_details(url))
    assert first.startswith(server.SAFETY) and "Note version one" in first
    assert "get_app_installer" in tools

    notes.write_text("Note version two", encoding="utf-8")
    second, _ = asyncio.run(session_details(url))
    assert second.startswith(server.SAFETY)
    assert "Note version two" in second and "Note version one" not in second
