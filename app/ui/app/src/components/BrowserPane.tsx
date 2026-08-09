import { useCallback, useEffect, useRef, useState } from "react";
import { useBrowserPane } from "@/hooks/useBrowserPane";
import { useSettings } from "@/hooks/useSettings";
import {
  abortBrowserTask,
  getBrowserTaskStatus,
  runBrowserTask,
  sendBrowserInput,
  type BrowserTaskResult,
} from "@/lib/browser-api";
import {
  keyInputForKeyUp,
  keyInputsForKeyDown,
  modifiersFrom,
  mouseButtonName,
  pageCoordsFromPointer,
} from "@/lib/browser-input";

/** Pointer motion is throttled to roughly one frame's worth. Every move is a
 * round trip, and the page cannot use them faster than it repaints. */
const MOUSE_MOVE_INTERVAL_MS = 33;

/** WebBrain reports a run's progress only when polled. */
const RUN_POLL_MS = 2000;

function CloseIcon() {
  return (
    <svg className="h-4 w-4 fill-current" viewBox="0 0 24 24" aria-hidden="true">
      <path d="M6.4 5.1 12 10.7l5.6-5.6 1.3 1.3-5.6 5.6 5.6 5.6-1.3 1.3-5.6-5.6-5.6 5.6-1.3-1.3 5.6-5.6-5.6-5.6z" />
    </svg>
  );
}

/** Extracts the run id from webbrain_status/run prose so the pane can keep
 * polling. The tools answer in text meant for a model, so there is no
 * structured field to read. */
function parseRunId(text: string): string | null {
  const match = text.match(/\brun[_ ]?id[:=]?\s*([A-Za-z0-9_-]{6,})/i);
  return match ? match[1] : null;
}

/** True once a run has stopped moving, so polling can stop too. */
function isTerminal(text: string): boolean {
  return /\b(completed|succeeded|failed|aborted|cancelled|canceled|error)\b/i.test(
    text,
  );
}

export function BrowserPane({ onClose }: { onClose: () => void }) {
  const { settingsData } = useSettings();
  const debugPort = settingsData?.BrowserDebugPort || 0;

  const {
    status,
    frame,
    tabs,
    connecting,
    error,
    connect,
    disconnect,
    refreshTabs,
    selectTab,
    navigate,
  } = useBrowserPane(true);

  const surfaceRef = useRef<HTMLDivElement>(null);
  const lastMoveRef = useRef(0);

  const [address, setAddress] = useState("");
  const [addressDirty, setAddressDirty] = useState(false);
  const [task, setTask] = useState("");
  const [mode, setMode] = useState<"ask" | "act">("ask");
  const [run, setRun] = useState<BrowserTaskResult | null>(null);
  const [runId, setRunId] = useState<string | null>(null);
  const [running, setRunning] = useState(false);

  const connected = status?.connected ?? false;

  // Follow the page's own navigation unless the user is mid-edit.
  useEffect(() => {
    if (!addressDirty && status?.tabUrl) setAddress(status.tabUrl);
  }, [status?.tabUrl, addressDirty]);

  useEffect(() => {
    if (connected) void refreshTabs();
  }, [connected, status?.tabId, refreshTabs]);

  // Poll a live run until it settles.
  useEffect(() => {
    if (!runId) return;

    let cancelled = false;
    const timer = setInterval(async () => {
      try {
        const next = await getBrowserTaskStatus(runId);
        if (cancelled) return;
        setRun(next);
        if (isTerminal(next.text)) {
          setRunning(false);
          setRunId(null);
        }
      } catch {
        // Keep the last result; the status poll surfaces a lost connection.
      }
    }, RUN_POLL_MS);

    return () => {
      cancelled = true;
      clearInterval(timer);
    };
  }, [runId]);

  const toPage = useCallback(
    (clientX: number, clientY: number) => {
      const el = surfaceRef.current;
      if (!el || !frame) return null;
      return pageCoordsFromPointer(clientX, clientY, el.getBoundingClientRect(), frame);
    },
    [frame],
  );

  const handleMouse = useCallback(
    (
      e: React.MouseEvent<HTMLDivElement>,
      type: "mousePressed" | "mouseReleased" | "mouseMoved",
    ) => {
      const point = toPage(e.clientX, e.clientY);
      if (!point) return;

      if (type === "mouseMoved") {
        const now = performance.now();
        if (now - lastMoveRef.current < MOUSE_MOVE_INTERVAL_MS) return;
        lastMoveRef.current = now;
      }

      sendBrowserInput({
        kind: "mouse",
        type,
        x: point.x,
        y: point.y,
        button: type === "mouseMoved" ? "none" : mouseButtonName(e.button),
        clickCount: type === "mouseMoved" ? 0 : e.detail || 1,
        modifiers: modifiersFrom(e),
      });
    },
    [toPage],
  );

  const handleWheel = useCallback(
    (e: React.WheelEvent<HTMLDivElement>) => {
      const point = toPage(e.clientX, e.clientY);
      if (!point) return;
      sendBrowserInput({
        kind: "mouse",
        type: "mouseWheel",
        x: point.x,
        y: point.y,
        deltaX: e.deltaX,
        deltaY: e.deltaY,
        modifiers: modifiersFrom(e),
      });
    },
    [toPage],
  );

  const handleKeyDown = useCallback((e: React.KeyboardEvent<HTMLDivElement>) => {
    // Let the user tab out of the surface rather than trapping focus.
    if (e.key === "Tab") return;
    e.preventDefault();
    for (const input of keyInputsForKeyDown(e)) sendBrowserInput(input);
  }, []);

  const handleKeyUp = useCallback((e: React.KeyboardEvent<HTMLDivElement>) => {
    if (e.key === "Tab") return;
    e.preventDefault();
    sendBrowserInput(keyInputForKeyUp(e));
  }, []);

  const submitAddress = useCallback(
    (e: React.FormEvent) => {
      e.preventDefault();
      setAddressDirty(false);
      void navigate(address);
    },
    [address, navigate],
  );

  const submitTask = useCallback(
    async (e: React.FormEvent) => {
      e.preventDefault();
      if (!task.trim()) return;

      setRunning(true);
      setRun(null);
      try {
        const result = await runBrowserTask(task, mode);
        setRun(result);
        const id = parseRunId(result.text);
        setRunId(id);
        if (!id || result.isError) setRunning(false);
      } catch (err) {
        setRun({
          text: err instanceof Error ? err.message : String(err),
          isError: true,
        });
        setRunning(false);
      }
    },
    [task, mode],
  );

  const stopRun = useCallback(async () => {
    if (!runId) {
      setRunning(false);
      return;
    }
    try {
      setRun(await abortBrowserTask(runId));
    } catch {
      // Nothing useful to add; the run may already have ended.
    }
    setRunning(false);
    setRunId(null);
  }, [runId]);

  return (
    <aside className="flex h-screen w-full flex-col border-l border-neutral-200 bg-white dark:border-neutral-800 dark:bg-neutral-900">
      <div
        className="flex h-13 flex-none items-center gap-2 px-3"
        onDoubleClick={() => window.doubleClick && window.doubleClick()}
        onMouseDown={() => window.drag && window.drag()}
      >
        <span className="flex-1 truncate text-sm font-medium text-neutral-700 dark:text-neutral-200">
          {status?.tabTitle || "Browser"}
        </span>
        <button
          onClick={onClose}
          onMouseDown={(e) => e.stopPropagation()}
          className="flex h-7 w-7 items-center justify-center rounded-full text-neutral-500 hover:bg-neutral-100 dark:text-neutral-400 dark:hover:bg-neutral-700/75"
          aria-label="Hide browser"
          title="Hide browser"
        >
          <CloseIcon />
        </button>
      </div>

      {connected && (
        <div className="flex flex-none items-center gap-2 px-3 pb-2">
          <form onSubmit={submitAddress} className="flex-1">
            <input
              value={address}
              onChange={(e) => {
                setAddress(e.target.value);
                setAddressDirty(true);
              }}
              onBlur={() => setAddressDirty(false)}
              placeholder="Enter a URL"
              spellCheck={false}
              className="w-full rounded-full border border-neutral-200 bg-neutral-50 px-3 py-1.5 text-xs text-neutral-800 outline-none focus:border-neutral-400 dark:border-neutral-700 dark:bg-neutral-800 dark:text-neutral-100"
            />
          </form>
          {tabs.length > 1 && (
            <select
              value={status?.tabId ?? ""}
              onChange={(e) => void selectTab(e.target.value)}
              className="max-w-32 rounded-full border border-neutral-200 bg-neutral-50 px-2 py-1.5 text-xs text-neutral-700 outline-none dark:border-neutral-700 dark:bg-neutral-800 dark:text-neutral-200"
              aria-label="Choose tab"
            >
              {tabs.map((tab) => (
                <option key={tab.id} value={tab.id}>
                  {tab.title || tab.url}
                </option>
              ))}
            </select>
          )}
        </div>
      )}

      <div className="relative flex min-h-0 flex-1 items-center justify-center bg-neutral-100 dark:bg-neutral-950">
        {connected && frame ? (
          <div
            ref={surfaceRef}
            tabIndex={0}
            role="application"
            aria-label="Browser page"
            className="h-full w-full cursor-default outline-none"
            onMouseDown={(e) => handleMouse(e, "mousePressed")}
            onMouseUp={(e) => handleMouse(e, "mouseReleased")}
            onMouseMove={(e) => handleMouse(e, "mouseMoved")}
            onWheel={handleWheel}
            onKeyDown={handleKeyDown}
            onKeyUp={handleKeyUp}
            onContextMenu={(e) => e.preventDefault()}
          >
            <img
              src={`data:image/jpeg;base64,${frame.data}`}
              alt=""
              draggable={false}
              className="pointer-events-none h-full w-full object-contain object-top select-none"
            />
          </div>
        ) : (
          <ConnectPrompt
            connected={connected}
            connecting={connecting}
            error={error ?? status?.lastError ?? null}
            port={debugPort || 9222}
            onConnect={() => void connect(debugPort || undefined)}
          />
        )}
      </div>

      {connected && (
        <div className="flex-none border-t border-neutral-200 px-3 py-2 dark:border-neutral-800">
          <WebBrainBar
            attached={status?.attached ?? false}
            detail={status?.webbrain}
            task={task}
            setTask={setTask}
            mode={mode}
            setMode={setMode}
            running={running}
            run={run}
            onSubmit={submitTask}
            onStop={stopRun}
          />
          <button
            onClick={() => void disconnect()}
            className="mt-2 text-[11px] text-neutral-400 hover:text-neutral-600 dark:hover:text-neutral-300"
          >
            Disconnect
          </button>
        </div>
      )}
    </aside>
  );
}

function ConnectPrompt({
  connected,
  connecting,
  error,
  port,
  onConnect,
}: {
  connected: boolean;
  connecting: boolean;
  error: string | null;
  port: number;
  onConnect: () => void;
}) {
  return (
    <div className="max-w-sm px-6 text-center">
      <p className="text-sm font-medium text-neutral-700 dark:text-neutral-200">
        {connected ? "Waiting for the first frame…" : "Connect to your browser"}
      </p>

      {!connected && (
        <>
          <p className="mt-2 text-xs leading-relaxed text-neutral-500 dark:text-neutral-400">
            Quit Chrome completely, then relaunch it with remote debugging so
            the pane can attach:
          </p>
          <code className="mt-2 block overflow-x-auto rounded-lg bg-neutral-200 px-3 py-2 text-left text-[11px] whitespace-pre text-neutral-700 dark:bg-neutral-800 dark:text-neutral-300">
            {`open -a "Google Chrome" --args --remote-debugging-port=${port}`}
          </code>
          <button
            onClick={onConnect}
            disabled={connecting}
            className="mt-3 rounded-full bg-neutral-900 px-4 py-1.5 text-xs font-medium text-white disabled:opacity-50 dark:bg-neutral-100 dark:text-neutral-900"
          >
            {connecting ? "Connecting…" : "Connect"}
          </button>
        </>
      )}

      {error && (
        <p className="mt-3 text-xs leading-relaxed text-red-600 dark:text-red-400">
          {error}
        </p>
      )}
    </div>
  );
}

function WebBrainBar({
  attached,
  detail,
  task,
  setTask,
  mode,
  setMode,
  running,
  run,
  onSubmit,
  onStop,
}: {
  attached: boolean;
  detail?: string;
  task: string;
  setTask: (value: string) => void;
  mode: "ask" | "act";
  setMode: (mode: "ask" | "act") => void;
  running: boolean;
  run: BrowserTaskResult | null;
  onSubmit: (e: React.FormEvent) => void;
  onStop: () => void;
}) {
  const [showDetail, setShowDetail] = useState(false);

  if (!attached) {
    return (
      <div className="text-[11px] leading-relaxed text-neutral-500 dark:text-neutral-400">
        <button
          onClick={() => setShowDetail((open) => !open)}
          className="font-medium text-neutral-600 hover:underline dark:text-neutral-300"
        >
          WebBrain is not connected
        </button>
        {showDetail && detail && (
          <pre className="mt-1 max-h-40 overflow-auto whitespace-pre-wrap text-[11px] text-neutral-500 dark:text-neutral-400">
            {detail}
          </pre>
        )}
      </div>
    );
  }

  return (
    <>
      <form onSubmit={onSubmit} className="flex items-center gap-2">
        <input
          value={task}
          onChange={(e) => setTask(e.target.value)}
          placeholder={
            mode === "ask" ? "Ask about this page" : "Tell WebBrain what to do"
          }
          className="min-w-0 flex-1 rounded-full border border-neutral-200 bg-neutral-50 px-3 py-1.5 text-xs text-neutral-800 outline-none focus:border-neutral-400 dark:border-neutral-700 dark:bg-neutral-800 dark:text-neutral-100"
        />
        <button
          type="button"
          onClick={() => setMode(mode === "ask" ? "act" : "ask")}
          title={
            mode === "ask"
              ? "Ask is read-only. Switch to Act to allow clicking and typing."
              : "Act can click, type and submit. Switch back to read-only Ask."
          }
          className={`rounded-full px-2.5 py-1.5 text-[11px] font-medium ${
            mode === "act"
              ? "bg-amber-100 text-amber-900 dark:bg-amber-900/40 dark:text-amber-200"
              : "bg-neutral-100 text-neutral-600 dark:bg-neutral-800 dark:text-neutral-300"
          }`}
        >
          {mode === "ask" ? "Ask" : "Act"}
        </button>
        {running ? (
          <button
            type="button"
            onClick={onStop}
            className="rounded-full bg-neutral-200 px-3 py-1.5 text-[11px] font-medium text-neutral-700 dark:bg-neutral-700 dark:text-neutral-200"
          >
            Stop
          </button>
        ) : (
          <button
            type="submit"
            disabled={!task.trim()}
            className="rounded-full bg-neutral-900 px-3 py-1.5 text-[11px] font-medium text-white disabled:opacity-40 dark:bg-neutral-100 dark:text-neutral-900"
          >
            Run
          </button>
        )}
      </form>

      {run && (
        <pre
          className={`mt-2 max-h-40 overflow-auto whitespace-pre-wrap text-[11px] leading-relaxed ${
            run.isError
              ? "text-red-600 dark:text-red-400"
              : "text-neutral-600 dark:text-neutral-300"
          }`}
        >
          {run.text}
        </pre>
      )}
    </>
  );
}

export default BrowserPane;
