import { FormEvent, useEffect, useMemo, useRef, useState } from "react";
import { FitAddon } from "@xterm/addon-fit";
import { Terminal } from "@xterm/xterm";
import "@xterm/xterm/css/xterm.css";

import { formatCreatedAt, shortDigest, statusTone } from "./utils";

type Sandbox = {
  id: string;
  pid: number;
  status: string;
  image: string;
  created_at: string;
  managed: boolean;
};

type ImageSummary = {
  tag: string;
  source: string;
  digest: string;
  size_mb: number;
  created_at: string;
};

type SandboxesResponse = {
  sandboxes: Sandbox[];
};

type ImagesResponse = {
  images: ImageSummary[];
};

type ProfileSummary = {
  id: string;
  name: string;
  description: string;
  image: string;
  cpus: number;
  memory_mb: number;
  disk_size_mb: number;
  workspace: string;
  privileged: boolean;
  allow_hosts: string[];
  secret_names: string[];
  env_from_host: string[];
  require_repo: boolean;
};

type ProfilesResponse = {
  profiles: ProfileSummary[];
};

type ProfileLaunchMode = "terminal" | "exec";

type StartSandboxForm = {
  profile_id: string;
  repo: string;
  instruction: string;
  launch_mode: ProfileLaunchMode;
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
  profile_id: "",
  repo: "",
  instruction: "",
  launch_mode: "terminal",
  image: "",
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

export function App() {
  const [sandboxes, setSandboxes] = useState<Sandbox[]>([]);
  const [images, setImages] = useState<ImageSummary[]>([]);
  const [profiles, setProfiles] = useState<ProfileSummary[]>([]);
  const [activeTab, setActiveTab] = useState<"sandboxes" | "images">("sandboxes");
  const [busy, setBusy] = useState(false);
  const [notice, setNotice] = useState("");
  const [error, setError] = useState("");

  const [showStartModal, setShowStartModal] = useState(false);
  const [showTerminalModal, setShowTerminalModal] = useState(false);

  const [startForm, setStartForm] = useState<StartSandboxForm>(startDefaults);
  const [pullForm, setPullForm] = useState<PullImageForm>(pullDefaults);

  const [terminalSandboxID, setTerminalSandboxID] = useState("");
  const [terminalCommand, setTerminalCommand] = useState("sh");
  const [terminalConnected, setTerminalConnected] = useState(false);
  const [terminalConnecting, setTerminalConnecting] = useState(false);
  const [terminalReady, setTerminalReady] = useState(false);
  const [terminalAutoConnect, setTerminalAutoConnect] = useState(true);
  const [pendingTerminalCommand, setPendingTerminalCommand] = useState("");
  const [terminalCommandsBySandbox, setTerminalCommandsBySandbox] = useState<Record<string, string>>({});
  const [terminalModeBySandbox, setTerminalModeBySandbox] = useState<Record<string, "entrypoint" | "shell">>({});

  const wsRef = useRef<WebSocket | null>(null);
  const termHostRef = useRef<HTMLDivElement | null>(null);
  const termRef = useRef<Terminal | null>(null);
  const fitRef = useRef<FitAddon | null>(null);
  const encoderRef = useRef(new TextEncoder());
  const decoderRef = useRef(new TextDecoder());
  const terminalSandboxIDRef = useRef("");

  const runningSandboxes = sandboxes.filter((sandbox) => sandbox.status === "running");
  const activeTerminalSandboxID = terminalSandboxID || runningSandboxes[0]?.id || "";
  const selectedTerminalHasSavedEntrypoint = Boolean(
    activeTerminalSandboxID && terminalCommandsBySandbox[activeTerminalSandboxID]?.trim()
  );
  const selectedTerminalPreferredMode =
    (activeTerminalSandboxID ? terminalModeBySandbox[activeTerminalSandboxID] : undefined) ??
    (selectedTerminalHasSavedEntrypoint ? "entrypoint" : "shell");
  const selectedProfile = useMemo(
    () => profiles.find((profile) => profile.id === startForm.profile_id) ?? null,
    [profiles, startForm.profile_id]
  );
  const profileImageOptions = useMemo(
    () => {
      if (profiles.length === 0) {
        return [] as string[];
      }
      const seen = new Set<string>();
      const tags: string[] = [];
      for (const profile of profiles) {
        if (!profile.image || seen.has(profile.image)) {
          continue;
        }
        seen.add(profile.image);
        tags.push(profile.image);
      }
      return tags;
    },
    [profiles]
  );
  const imageOptions = useMemo(() => {
    const seen = new Set<string>();
    const tags: string[] = [];
    for (const item of images) {
      if (!item.tag || seen.has(item.tag)) {
        continue;
      }
      seen.add(item.tag);
      tags.push(item.tag);
    }
    for (const tag of profileImageOptions) {
      if (!seen.has(tag)) {
        seen.add(tag);
        tags.push(tag);
      }
    }
    return tags;
  }, [images, profileImageOptions]);

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
    setTerminalConnecting(false);
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
      const runningByID = new Map(data.sandboxes.filter((sandbox) => sandbox.status === "running").map((sandbox) => [sandbox.id, sandbox]));
      setTerminalCommandsBySandbox((prev) => {
        const liveIDs = new Set(data.sandboxes.map((sandbox) => sandbox.id));
        let changed = false;
        const next: Record<string, string> = {};
        for (const [id, command] of Object.entries(prev)) {
          if (!liveIDs.has(id)) {
            changed = true;
            continue;
          }
          next[id] = command;
        }
        return changed ? next : prev;
      });
      setTerminalModeBySandbox((prev) => {
        const liveIDs = new Set(data.sandboxes.map((sandbox) => sandbox.id));
        let changed = false;
        const next: Record<string, "entrypoint" | "shell"> = {};
        for (const [id, mode] of Object.entries(prev)) {
          if (!liveIDs.has(id)) {
            changed = true;
            continue;
          }
          next[id] = mode;
        }
        for (const id of runningByID.keys()) {
          if (!next[id]) {
            next[id] = "shell";
            changed = true;
          }
        }
        return changed ? next : prev;
      });

      const preferredSandboxID = data.sandboxes.find((sandbox) => sandbox.status === "running")?.id ?? "";
      const selectedSandboxID = terminalSandboxIDRef.current;
      if (!selectedSandboxID && preferredSandboxID) {
        setTerminalSandboxID(preferredSandboxID);
      }
      if (selectedSandboxID && !data.sandboxes.find((sandbox) => sandbox.id === selectedSandboxID)) {
        setTerminalSandboxID(preferredSandboxID);
        disconnectTerminal();
      }
    } catch (loadErr) {
      setError((loadErr as Error).message);
    }
  }

  async function loadImages(): Promise<void> {
    try {
      const data = await requestJSON<ImagesResponse>("/api/images");
      setImages(data.images);

      const tags = Array.from(new Set(data.images.map((item) => item.tag).filter((item) => item.length > 0)));
      setStartForm((prev) => {
        if (prev.image && tags.includes(prev.image)) {
          return prev;
        }
        if (tags.length > 0) {
          return { ...prev, image: tags[0] };
        }
        return { ...prev, image: "" };
      });
    } catch (loadErr) {
      setError((loadErr as Error).message);
    }
  }

  async function loadProfiles(): Promise<void> {
    try {
      const data = await requestJSON<ProfilesResponse>("/api/profiles");
      setProfiles(data.profiles);
      setStartForm((prev) => {
        const selected = data.profiles.find((profile) => profile.id === prev.profile_id);
        if (selected) {
          return {
            ...prev,
            image: selected.image,
            cpus: selected.cpus,
            memory_mb: selected.memory_mb,
            disk_size_mb: selected.disk_size_mb,
            workspace: selected.workspace,
            privileged: selected.privileged || prev.privileged
          };
        }
        return prev;
      });
    } catch (loadErr) {
      setError((loadErr as Error).message);
    }
  }

  useEffect(() => {
    terminalSandboxIDRef.current = terminalSandboxID;
  }, [terminalSandboxID]);

  useEffect(() => {
    void loadSandboxes();
    void loadImages();
    void loadProfiles();

    const timer = window.setInterval(() => {
      void loadSandboxes();
    }, 3000);

    return () => {
      window.clearInterval(timer);
      disconnectTerminal();
    };
  }, []);

  useEffect(() => {
    if (!showTerminalModal || !termHostRef.current) {
      return;
    }

    const terminal = new Terminal({
      cursorBlink: true,
      fontFamily: "IBM Plex Mono, monospace",
      fontSize: 13,
      convertEol: true,
      scrollback: 5000,
      theme: {
        background: "#141413",
        foreground: "#faf9f5",
        cursor: "#d97757",
        selectionBackground: "#6a9bcc66"
      }
    });
    const fitAddon = new FitAddon();
    terminal.loadAddon(fitAddon);
    terminal.open(termHostRef.current);
    fitAddon.fit();
    terminal.write("Matchlock terminal ready.\r\n");

    terminal.attachCustomKeyEventHandler((event: KeyboardEvent) => {
      if (
        event.type === "keydown" &&
        event.ctrlKey &&
        !event.metaKey &&
        !event.altKey &&
        event.key.toLowerCase() === "l"
      ) {
        event.preventDefault();
        sendTerminalInputData("\u000c");
        return false;
      }
      return true;
    });

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
    setTerminalReady(true);

    return () => {
      window.removeEventListener("resize", onResize);
      resizeDisposable.dispose();
      disconnectTerminal();
      setTerminalReady(false);
      termRef.current = null;
      fitRef.current = null;
      terminal.dispose();
    };
  }, [showTerminalModal]);

  async function onStartSandbox(event: FormEvent<HTMLFormElement>): Promise<void> {
    event.preventDefault();
    clearFeedback();

    if (!startForm.image.trim()) {
      setError("Select an image before starting a sandbox.");
      return;
    }
    if (selectedProfile?.require_repo && !startForm.repo.trim()) {
      setError("Repo is required for this profile (owner/repo).");
      return;
    }

    setBusy(true);
    try {
      const isProfileLaunch = Boolean(startForm.profile_id.trim());
      const launchMode: ProfileLaunchMode = isProfileLaunch ? startForm.launch_mode : "exec";
      const payload = {
        profile_id: startForm.profile_id.trim(),
        repo: startForm.repo.trim(),
        instruction: startForm.instruction.trim(),
        launch_mode: launchMode,
        image: startForm.image.trim(),
        pull: startForm.pull,
        cpus: startForm.cpus,
        memory_mb: startForm.memory_mb,
        disk_size_mb: startForm.disk_size_mb,
        workspace: startForm.workspace.trim(),
        privileged: startForm.privileged
      };
      const result = await requestJSON<{ id: string; startup_command?: string }>("/api/sandboxes", {
        method: "POST",
        body: JSON.stringify(payload)
      });
      const startupCommand = result.startup_command?.trim() || "";
      if (startupCommand) {
        setTerminalCommandsBySandbox((prev) => ({ ...prev, [result.id]: startupCommand }));
        setTerminalModeBySandbox((prev) => ({ ...prev, [result.id]: launchMode === "terminal" ? "entrypoint" : "shell" }));
      } else {
        setTerminalModeBySandbox((prev) => ({ ...prev, [result.id]: "shell" }));
      }
      setNotice(`Sandbox ${result.id} started.`);
      setShowStartModal(false);
      setTerminalSandboxID(result.id);
      await loadSandboxes();
      if (isProfileLaunch && launchMode === "terminal") {
        openTerminalModal(result.id, startupCommand || "sh", true);
      }
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

  async function onRemoveSandbox(id: string): Promise<void> {
    clearFeedback();
    setBusy(true);
    try {
      await requestJSON(`/api/sandboxes/${encodeURIComponent(id)}/rm`, {
        method: "POST"
      });
      if (terminalSandboxID === id) {
        disconnectTerminal();
        setShowTerminalModal(false);
      }
      setNotice(`Sandbox ${id} removed.`);
      await loadSandboxes();
    } catch (removeErr) {
      setError((removeErr as Error).message);
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
      setNotice(`Pulled ${payload.image} (${shortDigest(result.digest)})`);
      await loadImages();
    } catch (pullErr) {
      setError((pullErr as Error).message);
    } finally {
      setBusy(false);
    }
  }

  async function onRemoveImage(tag: string): Promise<void> {
    clearFeedback();
    setBusy(true);
    try {
      await requestJSON<{ removed: boolean }>("/api/images", {
        method: "DELETE",
        body: JSON.stringify({ tag })
      });
      setNotice(`Image ${tag} removed.`);
      await loadImages();
    } catch (removeErr) {
      setError((removeErr as Error).message);
    } finally {
      setBusy(false);
    }
  }

  function openStartSandboxModal(): void {
    clearFeedback();
    void loadImages();
    void loadProfiles();
    setShowStartModal(true);
  }

  function applyProfile(profileID: string): void {
    setStartForm((prev) => {
      const nextProfile = profiles.find((profile) => profile.id === profileID);
      if (!nextProfile) {
        return { ...prev, profile_id: "" };
      }
      return {
        ...prev,
        profile_id: nextProfile.id,
        launch_mode: "terminal",
        image: nextProfile.image,
        cpus: nextProfile.cpus,
        memory_mb: nextProfile.memory_mb,
        disk_size_mb: nextProfile.disk_size_mb,
        workspace: nextProfile.workspace,
        privileged: nextProfile.privileged,
        pull: false
      };
    });
  }

  function getTerminalTargetSandboxID(preferredSandboxID?: string): string {
    return (preferredSandboxID || terminalSandboxID || runningSandboxes[0]?.id || "").trim();
  }

  function openTerminalModal(sandboxID?: string, command?: string, autoConnect = true): void {
    clearFeedback();
    const targetSandboxID = getTerminalTargetSandboxID(sandboxID);
    if (targetSandboxID && targetSandboxID !== terminalSandboxID) {
      setTerminalSandboxID(targetSandboxID);
    }

    const explicitCommand = command?.trim();
    const savedCommand = targetSandboxID ? terminalCommandsBySandbox[targetSandboxID]?.trim() : "";
    const savedMode = targetSandboxID ? terminalModeBySandbox[targetSandboxID] : undefined;
    const fallbackCommand = savedMode === "entrypoint" && savedCommand ? savedCommand : "sh";
    const resolvedCommand = explicitCommand || fallbackCommand;

    if (showTerminalModal && (terminalConnected || terminalConnecting)) {
      setPendingTerminalCommand(resolvedCommand);
    } else {
      setTerminalCommand(resolvedCommand);
    }
    setTerminalAutoConnect(autoConnect);
    setShowTerminalModal(true);
  }

  function openTerminalForSandboxMode(sandboxID: string, mode: "entrypoint" | "shell"): void {
    clearFeedback();
    if (mode === "entrypoint") {
      const command = terminalCommandsBySandbox[sandboxID]?.trim();
      if (!command) {
        setError("No saved profile entrypoint for this sandbox. Use shell mode instead.");
        return;
      }
      setTerminalModeBySandbox((prev) => ({ ...prev, [sandboxID]: "entrypoint" }));
      openTerminalModal(sandboxID, command, true);
      return;
    }
    setTerminalModeBySandbox((prev) => ({ ...prev, [sandboxID]: "shell" }));
    openTerminalModal(sandboxID, "sh", true);
  }

  function connectTerminalUsingMode(mode: "entrypoint" | "shell"): void {
    clearFeedback();
    const targetSandboxID = getTerminalTargetSandboxID();
    if (!targetSandboxID) {
      setError("Select a sandbox for terminal session.");
      return;
    }
    if (mode === "entrypoint") {
      const command = terminalCommandsBySandbox[targetSandboxID]?.trim();
      if (!command) {
        setError("No saved profile entrypoint for this sandbox. Use shell mode instead.");
        return;
      }
      setTerminalModeBySandbox((prev) => ({ ...prev, [targetSandboxID]: "entrypoint" }));
      connectTerminal(targetSandboxID, command);
      return;
    }
    setTerminalModeBySandbox((prev) => ({ ...prev, [targetSandboxID]: "shell" }));
    connectTerminal(targetSandboxID, "sh");
  }

  useEffect(() => {
    if (!showTerminalModal || !terminalAutoConnect || !terminalReady || terminalConnected || terminalConnecting) {
      return;
    }
    const targetSandboxID = getTerminalTargetSandboxID();
    if (!targetSandboxID) {
      return;
    }
    if (targetSandboxID !== terminalSandboxID) {
      setTerminalSandboxID(targetSandboxID);
    }
    const mode = terminalModeBySandbox[targetSandboxID] ?? "shell";
    if (mode === "entrypoint") {
      const savedCommand = terminalCommandsBySandbox[targetSandboxID]?.trim();
      if (!savedCommand) {
        setError("No saved profile entrypoint for this sandbox. Switch mode to shell.");
        return;
      }
      connectTerminal(targetSandboxID, savedCommand);
      return;
    }
    connectTerminal(targetSandboxID, "sh");
  }, [showTerminalModal, terminalAutoConnect, terminalReady, terminalSandboxID, runningSandboxes, terminalConnected, terminalConnecting, terminalModeBySandbox, terminalCommandsBySandbox]);

  useEffect(() => {
    if (!pendingTerminalCommand || !terminalConnected || !terminalSandboxID) {
      return;
    }
    const nextCommand = pendingTerminalCommand;
    disconnectTerminal();
    setTerminalCommand(nextCommand);
    setPendingTerminalCommand("");
    setTerminalAutoConnect(true);
    window.setTimeout(() => {
      connectTerminal(terminalSandboxID, nextCommand);
    }, 0);
  }, [pendingTerminalCommand, terminalConnected, terminalSandboxID]);

  function closeTerminalModal(): void {
    disconnectTerminal();
    setPendingTerminalCommand("");
    setTerminalAutoConnect(true);
    setShowTerminalModal(false);
  }

  function connectTerminal(sandboxIDOverride?: string, commandOverride?: string): void {
    clearFeedback();
    const targetSandboxID = (sandboxIDOverride ?? terminalSandboxID).trim();
    if (!targetSandboxID) {
      setError("Select a sandbox for terminal session.");
      return;
    }

    const resolvedCommand = commandOverride?.trim() || terminalCommand.trim() || "sh";
    setTerminalCommand(resolvedCommand);

    disconnectTerminal();
    setTerminalConnecting(true);
    if (targetSandboxID !== terminalSandboxID) {
      setTerminalSandboxID(targetSandboxID);
    }

    const terminal = termRef.current;
    const fitAddon = fitRef.current;
    if (!terminal || !fitAddon) {
      setTerminalConnecting(false);
      setError("Terminal is not initialized yet.");
      return;
    }

    fitAddon.fit();

    const socketURL = new URL(`/api/sandboxes/${encodeURIComponent(targetSandboxID)}/terminal/ws`, window.location.href);
    socketURL.protocol = socketURL.protocol === "https:" ? "wss:" : "ws:";
    socketURL.searchParams.set("command", resolvedCommand);
    socketURL.searchParams.set("rows", String(Math.max(terminal.rows, 8)));
    socketURL.searchParams.set("cols", String(Math.max(terminal.cols, 20)));

    const socket = new WebSocket(socketURL);
    socket.binaryType = "arraybuffer";
    wsRef.current = socket;

    socket.onopen = () => {
      setTerminalConnecting(false);
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
      setTerminalConnecting(false);
      setError("Terminal websocket error.");
      writeTerminalLine("[terminal websocket error]");
    };

    socket.onclose = () => {
      wsRef.current = null;
      setTerminalConnecting(false);
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
        <h1 className="hero-title">Sandbox Operations</h1>
        <p className="hero-subtitle">Control sandboxes, image inventory, and interactive terminal sessions from a single workspace.</p>
      </header>

      <div className="toolbar">
        <div className="tab-bar" role="tablist" aria-label="Views">
          <button
            type="button"
            className={`tab-button ${activeTab === "sandboxes" ? "active" : ""}`}
            onClick={() => setActiveTab("sandboxes")}
          >
            Sandboxes
          </button>
          <button
            type="button"
            className={`tab-button ${activeTab === "images" ? "active" : ""}`}
            onClick={() => setActiveTab("images")}
          >
            Images
          </button>
        </div>

        <div className="toolbar-actions">
          <button type="button" className="ghost" onClick={() => void loadSandboxes()} disabled={busy}>
            Refresh
          </button>
          <button type="button" onClick={openStartSandboxModal} disabled={busy}>
            Start Sandbox
          </button>
          <button type="button" className="ghost" onClick={() => openTerminalModal(undefined, undefined, false)} disabled={busy || runningSandboxes.length === 0}>
            Terminal
          </button>
        </div>
      </div>

      {(notice || error) && <div className={`feedback ${error ? "error" : "ok"}`}>{error || notice}</div>}

      {activeTab === "sandboxes" && (
        <section className="panel">
          <div className="panel-head">
            <h2>Sandboxes ({sandboxes.length})</h2>
            <span className="summary-pill">running: {runningSandboxes.length}</span>
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
                  <th>Actions</th>
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
                      <span className={`chip ${statusTone(sandbox.status)}`}>{sandbox.status}</span>
                      {sandbox.managed && <span className="chip managed">ui</span>}
                    </td>
                    <td>{sandbox.image || "-"}</td>
                    <td>{formatCreatedAt(sandbox.created_at)}</td>
                    <td>{sandbox.pid > 0 ? sandbox.pid : "-"}</td>
                    <td>
                      <div className="action-buttons">
                        <button
                          type="button"
                          title={terminalCommandsBySandbox[sandbox.id]?.trim() ? undefined : "No saved profile entrypoint"}
                          disabled={busy || sandbox.status !== "running" || !terminalCommandsBySandbox[sandbox.id]?.trim()}
                          onClick={() => openTerminalForSandboxMode(sandbox.id, "entrypoint")}
                        >
                          TUI
                        </button>
                        <button
                          type="button"
                          className="ghost"
                          disabled={busy || sandbox.status !== "running"}
                          onClick={() => openTerminalForSandboxMode(sandbox.id, "shell")}
                        >
                          Shell
                        </button>
                        <button
                          type="button"
                          className="ghost"
                          disabled={busy || sandbox.status !== "running"}
                          onClick={() => void onStopSandbox(sandbox.id)}
                        >
                          Stop
                        </button>
                        <button
                          type="button"
                          className="danger"
                          disabled={busy}
                          onClick={() => void onRemoveSandbox(sandbox.id)}
                        >
                          Remove
                        </button>
                      </div>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </section>
      )}

      {activeTab === "images" && (
        <section className="panel images-layout">
          <div className="image-pull-card">
            <div className="panel-head">
              <h2>Pull Image</h2>
              <button type="button" className="ghost" onClick={() => void loadImages()} disabled={busy}>
                Refresh Images
              </button>
            </div>

            <form className="inline-form" onSubmit={(event) => void onPullImage(event)}>
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
                Pull
              </button>
            </form>
          </div>

          <div className="table-wrap">
            <table>
              <thead>
                <tr>
                  <th>Tag</th>
                  <th>Source</th>
                  <th>Digest</th>
                  <th>Size</th>
                  <th>Created</th>
                  <th>Actions</th>
                </tr>
              </thead>
              <tbody>
                {images.length === 0 && (
                  <tr>
                    <td colSpan={6} className="empty-row">
                      No images available.
                    </td>
                  </tr>
                )}
                {images.map((item) => (
                  <tr key={`${item.source}:${item.tag}`}>
                    <td className="mono">{item.tag}</td>
                    <td>{item.source || "local"}</td>
                    <td className="mono">{shortDigest(item.digest)}</td>
                    <td>{item.size_mb.toFixed(1)} MB</td>
                    <td>{formatCreatedAt(item.created_at)}</td>
                    <td>
                      <button type="button" className="danger" disabled={busy} onClick={() => void onRemoveImage(item.tag)}>
                        Remove
                      </button>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </section>
      )}

      {showStartModal && (
        <div className="modal-backdrop" role="presentation" onClick={() => setShowStartModal(false)}>
          <section className="modal" role="dialog" aria-modal="true" onClick={(event) => event.stopPropagation()}>
            <div className="modal-head">
              <div>
                <h3 className="modal-title">Start Sandbox</h3>
                <p className="modal-subtitle">Choose an image from your managed image inventory.</p>
              </div>
              <button type="button" className="ghost" onClick={() => setShowStartModal(false)}>
                Close
              </button>
            </div>

            <form onSubmit={(event) => void onStartSandbox(event)}>
              <label>
                Profile
                <select
                  value={startForm.profile_id}
                  onChange={(event) => {
                    const profileID = event.target.value;
                    if (!profileID) {
                      setStartForm((prev) => ({ ...prev, profile_id: "", launch_mode: "terminal" }));
                      return;
                    }
                    applyProfile(profileID);
                  }}
                >
                  <option value="">Custom</option>
                  {profiles.map((profile) => (
                    <option key={profile.id} value={profile.id}>
                      {profile.name}
                    </option>
                  ))}
                </select>
              </label>

              {selectedProfile && (
                <div className="profile-summary">
                  <p className="profile-name">{selectedProfile.name}</p>
                  <p className="profile-desc">{selectedProfile.description}</p>
                  <p className="profile-meta">
                    Secrets: {selectedProfile.secret_names.join(", ")} · Hosts: {selectedProfile.allow_hosts.length}
                  </p>
                </div>
              )}

              <label>
                Image
                <select
                  required
                  value={startForm.image}
                  onChange={(event) => setStartForm((prev) => ({ ...prev, image: event.target.value }))}
                >
                  <option value="">Select image</option>
                  {imageOptions.map((tag) => (
                    <option key={tag} value={tag}>
                      {tag}
                    </option>
                  ))}
                </select>
              </label>
              {imageOptions.length === 0 && <p className="helper-text">No images found. Pull one from the Images tab first.</p>}


              {selectedProfile?.require_repo && (
                <>
                  <label>
                    Launch behavior
                    <select
                      value={startForm.launch_mode}
                      onChange={(event) => setStartForm((prev) => ({ ...prev, launch_mode: event.target.value as ProfileLaunchMode }))}
                    >
                      <option value="terminal">Interactive entrypoint (TUI)</option>
                      <option value="exec">Exec console access (background startup)</option>
                    </select>
                  </label>
                  <p className="helper-text">
                    Interactive entrypoint opens terminal with the profile command. Exec console starts profile in background and keeps terminal as a plain shell.
                  </p>
                  <label>
                    Repository (owner/repo)
                    <input
                      required
                      placeholder="jingkaihe/matchlock"
                      value={startForm.repo}
                      onChange={(event) => setStartForm((prev) => ({ ...prev, repo: event.target.value }))}
                    />
                  </label>
                  <label>
                    Instruction (optional)
                    <textarea
                      rows={3}
                      placeholder="Fix failing tests in pkg/policy and add coverage"
                      value={startForm.instruction}
                      onChange={(event) => setStartForm((prev) => ({ ...prev, instruction: event.target.value }))}
                    />
                  </label>
                </>
              )}

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

              <div className="modal-actions">
                <button
                  type="button"
                  className="ghost"
                  onClick={() => {
                    void loadImages();
                    void loadProfiles();
                  }}
                  disabled={busy}
                >
                  Refresh
                </button>
                <button
                  type="submit"
                  disabled={busy || imageOptions.length === 0 || !startForm.image || (selectedProfile?.require_repo && !startForm.repo.trim())}
                >
                  Start Sandbox
                </button>
              </div>
            </form>
          </section>
        </div>
      )}

      {showTerminalModal && (
        <div className="modal-backdrop" role="presentation" onClick={closeTerminalModal}>
          <section className="modal large" role="dialog" aria-modal="true" onClick={(event) => event.stopPropagation()}>
            <div className="modal-head">
              <div>
                <h3 className="modal-title">Terminal</h3>
                <p className="modal-subtitle">Mode controls reconnect behavior. Entrypoint re-runs profile bootstrap; shell opens plain sh.</p>
                {!selectedTerminalHasSavedEntrypoint && (
                  <p className="helper-text">No saved entrypoint for this sandbox yet; use shell mode.</p>
                )}
              </div>
              <button type="button" className="ghost" onClick={closeTerminalModal}>
                Close
              </button>
            </div>

            <div className="terminal-toolbar">
              <label>
                Sandbox
                <select
                  value={terminalSandboxID}
                  onChange={(event) => {
                    const nextSandboxID = event.target.value;
                    setTerminalSandboxID(nextSandboxID);
                    const nextMode = terminalModeBySandbox[nextSandboxID] ?? "shell";
                    if (nextMode === "entrypoint") {
                      const savedCommand = terminalCommandsBySandbox[nextSandboxID]?.trim();
                      setTerminalCommand(savedCommand || "sh");
                    } else {
                      setTerminalCommand("sh");
                    }
                  }}
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
                Mode
                <select
                  value={selectedTerminalPreferredMode}
                  onChange={(event) => {
                    const targetSandboxID = getTerminalTargetSandboxID();
                    if (!targetSandboxID) {
                      return;
                    }
                    const nextMode = event.target.value as "entrypoint" | "shell";
                    setTerminalModeBySandbox((prev) => ({ ...prev, [targetSandboxID]: nextMode }));
                    if (nextMode === "entrypoint") {
                      const savedCommand = terminalCommandsBySandbox[targetSandboxID]?.trim();
                      if (savedCommand) {
                        setTerminalCommand(savedCommand);
                      }
                    } else {
                      setTerminalCommand("sh");
                    }
                  }}
                >
                  <option value="entrypoint" disabled={!selectedTerminalHasSavedEntrypoint}>
                    Reopen TUI entrypoint
                  </option>
                  <option value="shell">Open shell</option>
                </select>
              </label>
              <label>
                Command
                <input
                  value={terminalCommand}
                  onChange={(event) => setTerminalCommand(event.target.value)}
                  disabled={selectedTerminalPreferredMode === "entrypoint"}
                />
              </label>
              <span className={`dot ${terminalConnected ? "online" : "offline"}`}>{terminalConnected ? "connected" : terminalConnecting ? "connecting" : "offline"}</span>
            </div>

            <div className="terminal-actions">
              <button
                type="button"
                className="ghost"
                title={selectedTerminalPreferredMode === "entrypoint" && !selectedTerminalHasSavedEntrypoint ? "No saved profile entrypoint for selected sandbox" : undefined}
                onClick={() => connectTerminalUsingMode(selectedTerminalPreferredMode)}
                disabled={terminalConnecting || (selectedTerminalPreferredMode === "entrypoint" && !selectedTerminalHasSavedEntrypoint)}
              >
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
        </div>
      )}
    </div>
  );
}
