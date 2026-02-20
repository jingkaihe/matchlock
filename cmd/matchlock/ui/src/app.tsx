import { FormEvent, useEffect, useRef, useState } from "react";
import { FitAddon } from "@xterm/addon-fit";
import { Terminal } from "@xterm/xterm";
import "@xterm/xterm/css/xterm.css";

type Sandbox = {
  id: string;
  pid: number;
  status: string;
  image: string;
  created_at: string;
  managed: boolean;
};

type SandboxesResponse = {
  sandboxes: Sandbox[];
};

type StartSandboxForm = {
  image: string;
  pull: boolean;
  cpus: number;
  memory_mb: number;
  disk_size_mb: number;
  workspace: string;
  privileged: boolean;
};

type PullImageForm = {
  image: string;
  force: boolean;
  tag: string;
};

const startDefaults: StartSandboxForm = {
  image: "alpine:latest",
  pull: false,
  cpus: 1,
  memory_mb: 512,
  disk_size_mb: 5120,
  workspace: "/workspace",
  privileged: false
};

const pullDefaults: PullImageForm = {
  image: "alpine:latest",
  force: false,
  tag: ""
};

const wsFrameTypeInput = 0;
const wsFrameTypeResize = 1;

async function requestJSON<T>(url: string, init?: RequestInit): Promise<T> {
  const headers = new Headers(init?.headers ?? undefined);
  if (!headers.has("Content-Type") && init?.method && init.method !== "GET") {
    headers.set("Content-Type", "application/json");
  }

  const response = await fetch(url, {
    ...init,
    headers
  });

  const raw = await response.text();
  const payload = raw ? (JSON.parse(raw) as Record<string, unknown>) : {};

  if (!response.ok) {
    const message = typeof payload.error === "string" ? payload.error : `Request failed (${response.status})`;
    throw new Error(message);
  }

  return payload as T;
}

function formatCreatedAt(value: string): string {
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) {
    return "-";
  }
  return date.toLocaleString();
}

export function App() {
  const [sandboxes, setSandboxes] = useState<Sandbox[]>([]);
  const [busy, setBusy] = useState(false);
  const [notice, setNotice] = useState("");
  const [error, setError] = useState("");

  const [startForm, setStartForm] = useState<StartSandboxForm>(startDefaults);
  const [pullForm, setPullForm] = useState<PullImageForm>(pullDefaults);

  const [terminalSandboxID, setTerminalSandboxID] = useState("");
  const [terminalCommand, setTerminalCommand] = useState("sh");
  const [terminalConnected, setTerminalConnected] = useState(false);

  const wsRef = useRef<WebSocket | null>(null);
  const termHostRef = useRef<HTMLDivElement | null>(null);
  const termRef = useRef<Terminal | null>(null);
  const fitRef = useRef<FitAddon | null>(null);
  const encoderRef = useRef(new TextEncoder());
  const decoderRef = useRef(new TextDecoder());

  const runningSandboxes = sandboxes.filter((s) => s.status === "running");

  function clearFeedback(): void {
    setError("");
    setNotice("");
  }

  function writeTerminal(message: string): void {
    const terminal = termRef.current;
    if (!terminal) {
      return;
    }
    terminal.write(message);
  }

  function writeTerminalLine(message: string): void {
    writeTerminal(`\r\n${message}\r\n`);
  }

  function disconnectTerminal(): void {
    const ws = wsRef.current;
    if (ws) {
      ws.close();
      wsRef.current = null;
    }
    setTerminalConnected(false);
  }

  function sendTerminalInputData(data: string): void {
    const ws = wsRef.current;
    if (!ws || ws.readyState !== WebSocket.OPEN) {
      return;
    }
    const payload = encoderRef.current.encode(data);
    const frame = new Uint8Array(payload.length + 1);
    frame[0] = wsFrameTypeInput;
    frame.set(payload, 1);
    ws.send(frame);
  }

  function sendTerminalResize(rows: number, cols: number): void {
    const ws = wsRef.current;
    if (!ws || ws.readyState !== WebSocket.OPEN) {
      return;
    }
    const boundedRows = Math.min(Math.max(Math.floor(rows), 1), 65535);
    const boundedCols = Math.min(Math.max(Math.floor(cols), 1), 65535);
    const frame = new Uint8Array(5);
    frame[0] = wsFrameTypeResize;
    frame[1] = (boundedRows >> 8) & 0xff;
    frame[2] = boundedRows & 0xff;
    frame[3] = (boundedCols >> 8) & 0xff;
    frame[4] = boundedCols & 0xff;
    ws.send(frame);
  }

  async function loadSandboxes(): Promise<void> {
    try {
      const data = await requestJSON<SandboxesResponse>("/api/sandboxes");
      setSandboxes(data.sandboxes);
      if (!terminalSandboxID && data.sandboxes.length > 0) {
        setTerminalSandboxID(data.sandboxes[0].id);
      }
      if (terminalSandboxID && !data.sandboxes.find((s) => s.id === terminalSandboxID)) {
        setTerminalSandboxID(data.sandboxes[0]?.id ?? "");
        disconnectTerminal();
      }
    } catch (loadErr) {
      setError((loadErr as Error).message);
    }
  }

  useEffect(() => {
    if (!termHostRef.current) {
      return;
    }

    const terminal = new Terminal({
      cursorBlink: true,
      fontFamily: "IBM Plex Mono, monospace",
      fontSize: 13,
      convertEol: true,
      scrollback: 5000,
      theme: {
        background: "#141b26",
        foreground: "#cbeff0",
        cursor: "#ffd17a",
        selectionBackground: "#35506a"
      }
    });
    const fitAddon = new FitAddon();
    terminal.loadAddon(fitAddon);
    terminal.open(termHostRef.current);
    fitAddon.fit();
    terminal.write("Matchlock terminal ready. Pick a sandbox and click Connect.\r\n");

    terminal.onData((data: string) => {
      sendTerminalInputData(data);
    });
    const resizeDisposable = terminal.onResize(({ rows, cols }) => {
      sendTerminalResize(rows, cols);
    });

    const onResize = () => {
      fitAddon.fit();
      sendTerminalResize(terminal.rows, terminal.cols);
    };
    window.addEventListener("resize", onResize);

    termRef.current = terminal;
    fitRef.current = fitAddon;

    return () => {
      window.removeEventListener("resize", onResize);
      resizeDisposable.dispose();
      disconnectTerminal();
      termRef.current = null;
      fitRef.current = null;
      terminal.dispose();
    };
  }, []);

  useEffect(() => {
    void loadSandboxes();
    const timer = window.setInterval(() => {
      void loadSandboxes();
    }, 3000);
    return () => {
      window.clearInterval(timer);
      disconnectTerminal();
    };
  }, []);

  async function onStartSandbox(event: FormEvent<HTMLFormElement>): Promise<void> {
    event.preventDefault();
    clearFeedback();
    setBusy(true);
    try {
      const payload = {
        ...startForm,
        image: startForm.image.trim(),
        workspace: startForm.workspace.trim()
      };
      const result = await requestJSON<{ id: string }>("/api/sandboxes", {
        method: "POST",
        body: JSON.stringify(payload)
      });
      setNotice(`Sandbox ${result.id} started.`);
      setTerminalSandboxID(result.id);
      await loadSandboxes();
    } catch (startErr) {
      setError((startErr as Error).message);
    } finally {
      setBusy(false);
    }
  }

  async function onStopSandbox(id: string): Promise<void> {
    clearFeedback();
    setBusy(true);
    try {
      await requestJSON(`/api/sandboxes/${encodeURIComponent(id)}/stop`, {
        method: "POST"
      });
      if (terminalSandboxID === id) {
        disconnectTerminal();
      }
      setNotice(`Sandbox ${id} stop requested.`);
      await loadSandboxes();
    } catch (stopErr) {
      setError((stopErr as Error).message);
    } finally {
      setBusy(false);
    }
  }

  async function onPullImage(event: FormEvent<HTMLFormElement>): Promise<void> {
    event.preventDefault();
    clearFeedback();
    setBusy(true);
    try {
      const payload = {
        ...pullForm,
        image: pullForm.image.trim(),
        tag: pullForm.tag.trim()
      };
      const result = await requestJSON<{ digest: string }>("/api/images/pull", {
        method: "POST",
        body: JSON.stringify(payload)
      });
      setNotice(`Pulled ${payload.image} (${result.digest.slice(0, 18)}...)`);
    } catch (pullErr) {
      setError((pullErr as Error).message);
    } finally {
      setBusy(false);
    }
  }

  function connectTerminal(sandboxIDOverride?: string): void {
    clearFeedback();
    const targetSandboxID = (sandboxIDOverride ?? terminalSandboxID).trim();
    if (!targetSandboxID) {
      setError("Select a sandbox for terminal session.");
      return;
    }

    disconnectTerminal();
    if (targetSandboxID !== terminalSandboxID) {
      setTerminalSandboxID(targetSandboxID);
    }

    const terminal = termRef.current;
    const fitAddon = fitRef.current;
    if (!terminal || !fitAddon) {
      setError("Terminal is not initialized yet.");
      return;
    }

    fitAddon.fit();

    const socketURL = new URL(`/api/sandboxes/${encodeURIComponent(targetSandboxID)}/terminal/ws`, window.location.href);
    socketURL.protocol = socketURL.protocol === "https:" ? "wss:" : "ws:";
    socketURL.searchParams.set("command", terminalCommand.trim() || "sh");
    socketURL.searchParams.set("rows", String(Math.max(terminal.rows, 8)));
    socketURL.searchParams.set("cols", String(Math.max(terminal.cols, 20)));

    const socket = new WebSocket(socketURL);
    socket.binaryType = "arraybuffer";
    wsRef.current = socket;

    socket.onopen = () => {
      setTerminalConnected(true);
      writeTerminalLine(`[connected to ${targetSandboxID}]`);
      sendTerminalResize(Math.max(terminal.rows, 8), Math.max(terminal.cols, 20));
      terminal.focus();
    };

    socket.onmessage = (event: MessageEvent) => {
      if (typeof event.data === "string") {
        writeTerminal(event.data);
        return;
      }
      if (event.data instanceof ArrayBuffer) {
        writeTerminal(decoderRef.current.decode(new Uint8Array(event.data)));
      }
    };

    socket.onerror = () => {
      setError("Terminal websocket error.");
      writeTerminalLine("[terminal websocket error]");
    };

    socket.onclose = () => {
      wsRef.current = null;
      setTerminalConnected(false);
      writeTerminalLine("[terminal disconnected]");
    };

  }

  function sendCtrlC(): void {
    if (!terminalConnected) {
      return;
    }
    sendTerminalInputData("\u0003");
  }

  function clearTerminal(): void {
    termRef.current?.clear();
  }

  return (
    <div className="app-shell">
      <header className="hero">
        <p className="eyebrow">Matchlock</p>
        <h1>Sandbox Control Deck</h1>
        <p className="subtitle">Manage sandboxes, pull images, and open live terminal sessions from one binary.</p>
      </header>

      {(notice || error) && (
        <div className={`feedback ${error ? "error" : "ok"}`}>{error || notice}</div>
      )}

      <main className="layout">
        <section className="panel sandboxes">
          <div className="panel-head">
            <h2>Sandboxes</h2>
            <button type="button" onClick={() => void loadSandboxes()} disabled={busy}>
              Refresh
            </button>
          </div>

          <div className="table-wrap">
            <table>
              <thead>
                <tr>
                  <th>ID</th>
                  <th>Status</th>
                  <th>Image</th>
                  <th>Created</th>
                  <th>PID</th>
                  <th />
                </tr>
              </thead>
              <tbody>
                {sandboxes.length === 0 && (
                  <tr>
                    <td colSpan={6} className="empty-row">
                      No sandboxes yet.
                    </td>
                  </tr>
                )}
                {sandboxes.map((sandbox) => (
                  <tr key={sandbox.id}>
                    <td className="mono">{sandbox.id}</td>
                    <td>
                      <span className={`chip ${sandbox.status}`}>{sandbox.status}</span>
                      {sandbox.managed && <span className="chip managed">ui</span>}
                    </td>
                    <td>{sandbox.image || "-"}</td>
                    <td>{formatCreatedAt(sandbox.created_at)}</td>
                    <td>{sandbox.pid > 0 ? sandbox.pid : "-"}</td>
                    <td>
                      <button
                        type="button"
                        disabled={busy || sandbox.status !== "running"}
                        onClick={() => {
                          setTerminalSandboxID(sandbox.id);
                          connectTerminal(sandbox.id);
                        }}
                      >
                        Terminal
                      </button>
                      <button
                        type="button"
                        className="ghost"
                        disabled={busy || sandbox.status !== "running"}
                        onClick={() => void onStopSandbox(sandbox.id)}
                      >
                        Stop
                      </button>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </section>

        <section className="panel actions">
          <h2>Start Sandbox</h2>
          <form onSubmit={(event) => void onStartSandbox(event)}>
            <label>
              Image
              <input
                required
                value={startForm.image}
                onChange={(event) => setStartForm((prev) => ({ ...prev, image: event.target.value }))}
              />
            </label>
            <div className="grid-two">
              <label>
                CPUs
                <input
                  type="number"
                  min={1}
                  value={startForm.cpus}
                  onChange={(event) => setStartForm((prev) => ({ ...prev, cpus: Number(event.target.value) || 1 }))}
                />
              </label>
              <label>
                Memory MB
                <input
                  type="number"
                  min={128}
                  value={startForm.memory_mb}
                  onChange={(event) => setStartForm((prev) => ({ ...prev, memory_mb: Number(event.target.value) || 512 }))}
                />
              </label>
            </div>
            <div className="grid-two">
              <label>
                Disk MB
                <input
                  type="number"
                  min={1024}
                  value={startForm.disk_size_mb}
                  onChange={(event) => setStartForm((prev) => ({ ...prev, disk_size_mb: Number(event.target.value) || 5120 }))}
                />
              </label>
              <label>
                Workspace
                <input
                  value={startForm.workspace}
                  onChange={(event) => setStartForm((prev) => ({ ...prev, workspace: event.target.value }))}
                />
              </label>
            </div>
            <label className="toggle">
              <input
                type="checkbox"
                checked={startForm.pull}
                onChange={(event) => setStartForm((prev) => ({ ...prev, pull: event.target.checked }))}
              />
              Always pull latest image
            </label>
            <label className="toggle">
              <input
                type="checkbox"
                checked={startForm.privileged}
                onChange={(event) => setStartForm((prev) => ({ ...prev, privileged: event.target.checked }))}
              />
              Privileged sandbox
            </label>
            <button type="submit" disabled={busy}>
              Start Sandbox
            </button>
          </form>

          <h2>Pull Image</h2>
          <form onSubmit={(event) => void onPullImage(event)}>
            <label>
              Image
              <input
                required
                value={pullForm.image}
                onChange={(event) => setPullForm((prev) => ({ ...prev, image: event.target.value }))}
              />
            </label>
            <label>
              Tag (optional)
              <input
                placeholder="myapp:latest"
                value={pullForm.tag}
                onChange={(event) => setPullForm((prev) => ({ ...prev, tag: event.target.value }))}
              />
            </label>
            <label className="toggle">
              <input
                type="checkbox"
                checked={pullForm.force}
                onChange={(event) => setPullForm((prev) => ({ ...prev, force: event.target.checked }))}
              />
              Force remote pull
            </label>
            <button type="submit" disabled={busy}>
              Pull Image
            </button>
          </form>
        </section>

        <section className="panel terminal">
          <div className="panel-head">
            <h2>Terminal</h2>
            <span className={`dot ${terminalConnected ? "online" : "offline"}`}>
              {terminalConnected ? "connected" : "offline"}
            </span>
          </div>

          <div className="grid-two compact">
            <label>
              Sandbox
              <select
                value={terminalSandboxID}
                onChange={(event) => setTerminalSandboxID(event.target.value)}
              >
                <option value="">Select sandbox</option>
                {runningSandboxes.map((sandbox) => (
                  <option key={sandbox.id} value={sandbox.id}>
                    {sandbox.id}
                  </option>
                ))}
              </select>
            </label>
            <label>
              Command
              <input
                value={terminalCommand}
                onChange={(event) => setTerminalCommand(event.target.value)}
              />
            </label>
          </div>

          <div className="terminal-actions">
            <button type="button" onClick={connectTerminal} disabled={busy || terminalConnected || !terminalSandboxID}>
              Connect
            </button>
            <button type="button" className="ghost" onClick={disconnectTerminal} disabled={!terminalConnected}>
              Disconnect
            </button>
            <button type="button" className="ghost" onClick={sendCtrlC} disabled={!terminalConnected}>
              Ctrl+C
            </button>
            <button type="button" className="ghost" onClick={clearTerminal}>
              Clear
            </button>
          </div>

          <div className="terminal-screen">
            <div ref={termHostRef} className="terminal-host" />
          </div>
        </section>
      </main>
    </div>
  );
}
