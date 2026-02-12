#!/usr/bin/env -S uv run --script
# /// script
# requires-python = ">=3.11"
# dependencies = [
#     "streamlit>=1.45.0",
#     "agent-client-protocol>=0.7.0",
# ]
# ///
"""
Kodelet Streamlit Chatbot (ACP)

A chatbot interface that communicates with kodelet via the Agent Client Protocol (ACP).

Usage:
    uv run main.py
"""

import asyncio
import base64
import contextlib
import json
import logging
import os
import sys
import threading
from datetime import datetime
from typing import Any, cast

import streamlit as st_module  # type: ignore[import-untyped]
from acp import (  # type: ignore[import-not-found]
    PROTOCOL_VERSION,
    Client,
    RequestError,
    connect_to_agent,
    image_block,
    text_block,
)
from acp.client.connection import ClientSideConnection  # type: ignore[import-not-found]
from acp.schema import (  # type: ignore[import-not-found]
    AgentMessageChunk,
    AgentThoughtChunk,
    CreateTerminalResponse,
    EnvVariable,
    KillTerminalCommandResponse,
    PermissionOption,
    ReadTextFileResponse,
    ReleaseTerminalResponse,
    RequestPermissionResponse,
    TerminalOutputResponse,
    TextContentBlock,
    ToolCall,
    ToolCallProgress,
    ToolCallStart,
    WaitForTerminalExitResponse,
    WriteTextFileResponse,
)
from acp.transports import default_environment  # type: ignore[import-not-found]

os.environ["STREAMLIT_THEME_BASE"] = "light"
st: Any = cast(Any, st_module)

CUSTOM_CSS = """
<style>
@import url('https://fonts.googleapis.com/css2?family=Lora:wght@400;500;600&family=Poppins:wght@400;500;600;700&display=swap');

:root {
    --kodelet-dark: #141413;
    --kodelet-light: #faf9f5;
    --kodelet-mid-gray: #b0aea5;
    --kodelet-light-gray: #e8e6dc;
    --kodelet-orange: #d97757;
}

.stApp { background-color: var(--kodelet-light); }
h1, h2, h3 { font-family: 'Poppins', Arial, sans-serif !important; color: var(--kodelet-dark) !important; }
.stMarkdown p, .stMarkdown li { font-family: 'Lora', Georgia, serif; }

[data-testid="stChatMessage"] {
    background-color: white;
    border: 1px solid var(--kodelet-light-gray);
    border-radius: 8px;
    padding: 1rem !important;
}
[data-testid="stChatMessage"] * { border-color: transparent !important; }
[data-testid="stChatMessage"] [data-testid="stExpander"] {
    border-color: var(--kodelet-light-gray) !important;
    border-radius: 6px !important;
}

code, pre { font-family: 'Monaco', 'Menlo', 'Ubuntu Mono', monospace !important; }

.stButton > button {
    background-color: var(--kodelet-orange) !important;
    color: white !important;
    border: none !important;
    font-weight: 500 !important;
}
.stButton > button:hover { background-color: #c4644a !important; }

[data-testid="stSidebar"] { background-color: var(--kodelet-light-gray) !important; }
</style>
"""

ACP_BUFFER_LIMIT = 50 * 1024 * 1024
logger = logging.getLogger(__name__)


def find_matchlock_binary() -> str:
    local = os.path.join(os.path.dirname(__file__), "..", "..", "bin", "matchlock")
    local = os.path.normpath(local)
    if os.path.isfile(local) and os.access(local, os.X_OK):
        return local
    st.error(f"Could not find matchlock at {local}. Build with `mise run build`.")
    st.stop()
    raise SystemExit


# ---------------------------------------------------------------------------
# Persistent ACP connection
# ---------------------------------------------------------------------------
# Streamlit reruns the script on every interaction. We keep the matchlock VM
# subprocess and ACP connection alive in a dedicated asyncio event loop
# running on a background daemon thread, so the VM is only booted once per
# browser session.
# ---------------------------------------------------------------------------


class _PersistentACP:
    """Owns a matchlock subprocess + ACP connection on a background event loop."""

    def __init__(self):
        self._loop: asyncio.AbstractEventLoop | None = None
        self._thread: threading.Thread | None = None
        self._conn: ClientSideConnection | None = None
        self._process: asyncio.subprocess.Process | None = None
        self._session_id: str | None = None
        self._client: ACPClient | None = None
        self._ready = threading.Event()
        self._error: Exception | None = None

    @property
    def alive(self) -> bool:
        if self._process is None or self._conn is None or self._loop is None:
            return False
        if self._process.returncode is not None:
            return False
        try:
            if self._conn.is_closed():
                return False
        except AttributeError:
            # Fallback for ACP library versions without is_closed()
            try:
                if self._conn._conn._closed:  # type: ignore[attr-defined]
                    return False
            except AttributeError:
                pass
        return True

    @property
    def session_id(self) -> str | None:
        return self._session_id

    @property
    def client(self) -> "ACPClient | None":
        return self._client

    def start(self) -> None:
        self._ready.clear()
        self._error = None
        self._thread = threading.Thread(target=self._run_loop, daemon=True)
        self._thread.start()
        self._ready.wait(timeout=120)
        if self._error:
            raise self._error

    def _run_loop(self) -> None:
        self._loop = asyncio.new_event_loop()
        asyncio.set_event_loop(self._loop)
        try:
            self._loop.run_until_complete(self._start_connection())
        except Exception as exc:
            self._error = exc
            self._ready.set()
            return
        self._ready.set()
        # Keep event loop alive so the connection's receive loop keeps running
        try:
            self._loop.run_forever()
        finally:
            self._loop.run_until_complete(self._cleanup())
            self._loop.close()

    async def _start_connection(self) -> None:
        matchlock_bin = find_matchlock_binary()
        merged_env = dict(default_environment())
        merged_env.update(os.environ)

        self._process = await asyncio.create_subprocess_exec(
            matchlock_bin, "run",
            "--image", "acp:latest",
            "--secret", "ANTHROPIC_API_KEY@api.anthropic.com",
            "--allow-host", "*",
            "-i", "--",
            stdin=asyncio.subprocess.PIPE,
            stdout=asyncio.subprocess.PIPE,
            stderr=asyncio.subprocess.PIPE,
            env=merged_env,
            limit=ACP_BUFFER_LIMIT,
        )

        self._client = ACPClient()
        assert self._process.stdout is not None
        assert self._process.stdin is not None
        self._conn = connect_to_agent(
            self._client,
            input_stream=self._process.stdin,
            output_stream=self._process.stdout,
        )

        await self._conn.initialize(
            protocol_version=PROTOCOL_VERSION, client_capabilities=None,
        )

        session = await self._conn.new_session(cwd=os.getcwd(), mcp_servers=[])
        self._session_id = session.session_id

    async def _cleanup(self) -> None:
        if self._conn:
            with contextlib.suppress(Exception):
                await self._conn.close()
        if self._process and self._process.returncode is None:
            with contextlib.suppress(Exception):
                self._process.terminate()
            with contextlib.suppress(Exception):
                await asyncio.wait_for(self._process.wait(), timeout=3)
            if self._process.returncode is None:
                with contextlib.suppress(Exception):
                    self._process.kill()

    def run_prompt(
        self,
        query: str,
        placeholder: Any,
        images: list[Any] | None = None,
    ) -> dict:
        """Send a prompt over the persistent connection. Blocks until done.

        Polls the client's accumulated state from the main thread so that
        Streamlit rendering works (st widgets must be called from the main
        script thread, not the background event-loop thread).
        """
        import concurrent.futures

        assert self._loop is not None and self._conn is not None and self._client is not None
        self._client.placeholder = placeholder
        self._client.start_streaming()

        future = asyncio.run_coroutine_threadsafe(
            self._prompt_coro(query, images), self._loop,
        )

        while True:
            try:
                future.result(timeout=0.15)
                break
            except concurrent.futures.TimeoutError:
                self._client.render_to(placeholder)
            except Exception:
                break

        self._client.render_to(placeholder)
        return self._client.get_result()

    async def _prompt_coro(
        self,
        query: str,
        images: list[Any] | None = None,
    ) -> None:
        assert self._conn is not None and self._client is not None

        prompt_blocks: list[Any] = []
        if images:
            for img in images:
                img_data = base64.b64encode(img.read()).decode("utf-8")
                prompt_blocks.append(image_block(img_data, img.type))
        if query:
            prompt_blocks.append(text_block(query))

        await self._conn.prompt(session_id=self._session_id, prompt=prompt_blocks)

    def shutdown(self) -> None:
        if self._loop and self._loop.is_running():
            self._loop.call_soon_threadsafe(self._loop.stop)
        if self._thread:
            self._thread.join(timeout=10)
        self._conn = None
        self._process = None
        self._session_id = None
        self._client = None


def _get_acp() -> _PersistentACP:
    """Get or create the persistent ACP connection for this Streamlit session."""
    if "acp" not in st.session_state or not st.session_state.acp.alive:
        if "acp" in st.session_state:
            st.session_state.acp.shutdown()
        acp = _PersistentACP()
        acp.start()
        st.session_state.acp = acp
    return st.session_state.acp


class ACPClient(Client):
    """ACP Client that streams responses to a Streamlit placeholder."""

    def __init__(self):
        self.thinking = ""
        self.message = ""
        self.tools: list[dict] = []
        self.placeholder: Any = None
        self.streaming = False
        self._thinking_done = False

    def start_streaming(self):
        self.thinking = ""
        self.message = ""
        self.tools = []
        self._thinking_done = False
        self.streaming = True

    async def request_permission(self, options: list[PermissionOption], session_id: str, tool_call: ToolCall, **kwargs: Any) -> RequestPermissionResponse:
        raise RequestError.method_not_found("session/request_permission")

    async def write_text_file(self, content: str, path: str, session_id: str, **kwargs: Any) -> WriteTextFileResponse | None:
        raise RequestError.method_not_found("fs/write_text_file")

    async def read_text_file(self, path: str, session_id: str, limit: int | None = None, line: int | None = None, **kwargs: Any) -> ReadTextFileResponse:
        raise RequestError.method_not_found("fs/read_text_file")

    async def create_terminal(self, command: str, session_id: str, args: list[str] | None = None, cwd: str | None = None, env: list[EnvVariable] | None = None, output_byte_limit: int | None = None, **kwargs: Any) -> CreateTerminalResponse:
        raise RequestError.method_not_found("terminal/create")

    async def terminal_output(self, session_id: str, terminal_id: str, **kwargs: Any) -> TerminalOutputResponse:
        raise RequestError.method_not_found("terminal/output")

    async def release_terminal(self, session_id: str, terminal_id: str, **kwargs: Any) -> ReleaseTerminalResponse | None:
        raise RequestError.method_not_found("terminal/release")

    async def wait_for_terminal_exit(self, session_id: str, terminal_id: str, **kwargs: Any) -> WaitForTerminalExitResponse:
        raise RequestError.method_not_found("terminal/wait_for_exit")

    async def kill_terminal(self, session_id: str, terminal_id: str, **kwargs: Any) -> KillTerminalCommandResponse | None:
        raise RequestError.method_not_found("terminal/kill")

    async def ext_method(self, method: str, params: dict) -> dict:
        raise RequestError.method_not_found(method)

    async def ext_notification(self, method: str, params: dict) -> None:
        pass

    def on_connect(self, conn: Any) -> None:
        pass

    async def session_update(self, session_id: str, update: Any, **kwargs: Any) -> None:
        """Accumulate data only — rendering happens on the main thread via render_to()."""
        if not self.streaming:
            return

        if isinstance(update, AgentThoughtChunk) and isinstance(update.content, TextContentBlock):
            self.thinking += update.content.text
        elif isinstance(update, AgentMessageChunk) and isinstance(update.content, TextContentBlock):
            self._thinking_done = True
            self.message += update.content.text
        elif isinstance(update, ToolCallStart):
            self._thinking_done = True
            self.tools.append({"id": update.tool_call_id, "title": update.title, "status": update.status, "output": update.raw_output})
        elif isinstance(update, ToolCallProgress):
            for tc in self.tools:
                if tc["id"] == update.tool_call_id:
                    if update.status:
                        tc["status"] = update.status
                    if update.raw_output:
                        tc["output"] = update.raw_output

    def render_to(self, placeholder: Any) -> None:
        """Render current accumulated state to a Streamlit placeholder.
        Must be called from the main Streamlit thread."""
        with placeholder.container():
            if self.thinking:
                label = "Thinking" if self._thinking_done else "Thinking..."
                with st.expander(label, expanded=not self._thinking_done):
                    st.markdown(self.thinking)
            if self.tools:
                with st.expander(f"Tools ({len(self.tools)})", expanded=False):
                    for i, tc in enumerate(self.tools):
                        icon = "⏳" if tc.get("status") == "running" else "✓"
                        st.write(f"**{i + 1}. {icon} {tc['title']}**")
                        if tc.get("output"):
                            output = tc["output"]
                            st.code(json.dumps(output, indent=2) if isinstance(output, dict) else str(output))
            if self.message:
                st.markdown(self.message)

    def get_result(self) -> dict:
        return {"role": "assistant", "content": self.message, "thinking": self.thinking, "tools": self.tools}


def render_assistant_message(msg: dict):
    if msg.get("thinking"):
        with st.expander("Thinking", expanded=False):
            st.markdown(msg["thinking"])
    if msg.get("tools"):
        with st.expander(f"Tools ({len(msg['tools'])})", expanded=False):
            for i, tc in enumerate(msg["tools"]):
                title = tc.get("title") or tc.get("name", "Tool")
                st.write(f"**{i + 1}. ✓ {title}**")
                if tc.get("output") or tc.get("result"):
                    output = tc.get("output") or tc.get("result")
                    st.code(json.dumps(output, indent=2) if isinstance(output, dict) else str(output))
    st.markdown(msg.get("content", ""))


def main():
    st.set_page_config(page_title="Kodelet Chat (ACP)", page_icon="K", layout="wide")
    st.markdown(CUSTOM_CSS, unsafe_allow_html=True)

    if "messages" not in st.session_state:
        st.session_state.messages = []

    # Greeting
    hour = datetime.now().hour
    greeting = "Good Morning" if hour < 12 else "Good Afternoon" if hour < 18 else "Good Evening"
    st.title(greeting)

    # Render message history
    for msg in st.session_state.messages:
        with st.chat_message(msg["role"]):
            if msg["role"] == "assistant":
                render_assistant_message(msg)
            else:
                if msg.get("images"):
                    for img in msg["images"]:
                        st.image(base64.b64decode(img["data"]))
                if msg.get("content"):
                    st.markdown(msg["content"])

    # Handle new input
    if prompt := st.chat_input(
        "Ask kodelet anything...",
        accept_file="multiple",
        file_type=["png", "jpg", "jpeg", "gif", "webp"],
    ):
        text = prompt.text if hasattr(prompt, "text") else str(prompt)
        files = prompt.files if hasattr(prompt, "files") else []

        with st.chat_message("user"):
            for f in files:
                st.image(f)
            if text:
                st.markdown(text)

        with st.chat_message("assistant"):
            placeholder = st.empty()
            try:
                acp = _get_acp()
                with st.spinner("Booting sandbox...") if not (hasattr(st.session_state, "acp") and st.session_state.get("acp") and st.session_state.acp.alive) else contextlib.nullcontext():
                    pass
                result = acp.run_prompt(text, placeholder, images=files if files else None)
            except Exception as e:
                import traceback
                st.error(f"ACP error: {e}")
                st.code(traceback.format_exc())
                result = {"role": "assistant", "content": "", "thinking": "", "tools": []}

        user_msg: dict[str, Any] = {"role": "user", "content": text}
        if files:
            user_msg["images"] = [
                {"data": base64.b64encode(f.getvalue()).decode("utf-8"), "type": f.type}
                for f in files
            ]
        st.session_state.messages.append(user_msg)
        st.session_state.messages.append(result)

    # Sidebar
    with st.sidebar:
        if st.button("✨ New Chat", use_container_width=True):
            if "acp" in st.session_state:
                st.session_state.acp.shutdown()
                del st.session_state.acp
            st.session_state.messages = []
            st.rerun()

        if "acp" in st.session_state and st.session_state.acp.alive:
            st.success("🟢 Sandbox connected", icon="✅")
        else:
            st.info("🔴 Sandbox will start on first message")


if __name__ == "__main__":
    from streamlit.web import cli as stcli  # type: ignore[import-not-found]

    if st.runtime.exists():
        main()
    else:
        sys.argv = ["streamlit", "run", __file__, "--server.headless", "true"]
        sys.exit(stcli.main())
