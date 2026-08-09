import { useCallback, useEffect, useRef, useState } from "react";
import {
  connectBrowser,
  disconnectBrowser,
  getBrowserStatus,
  listBrowserTabs,
  navigateBrowser,
  selectBrowserTab,
  streamBrowserFrames,
  type BrowserFrame,
  type BrowserStatus,
  type BrowserTab,
} from "@/lib/browser-api";

/** How often the pane re-checks the connection. Slow on purpose: it exists to
 * notice the browser quitting, not to drive the UI. */
const STATUS_POLL_MS = 4000;

export interface BrowserPaneState {
  status: BrowserStatus | null;
  frame: BrowserFrame | null;
  tabs: BrowserTab[];
  connecting: boolean;
  error: string | null;
  connect: (port?: number, tabId?: string) => Promise<void>;
  disconnect: () => Promise<void>;
  refreshTabs: () => Promise<void>;
  selectTab: (tabId: string) => Promise<void>;
  navigate: (url: string) => Promise<void>;
  clearError: () => void;
}

/**
 * Owns the browser pane's connection. `enabled` gates everything: while the
 * pane is closed nothing polls, nothing streams, and no debugging port is
 * touched.
 */
export function useBrowserPane(enabled: boolean): BrowserPaneState {
  const [status, setStatus] = useState<BrowserStatus | null>(null);
  const [frame, setFrame] = useState<BrowserFrame | null>(null);
  const [tabs, setTabs] = useState<BrowserTab[]>([]);
  const [connecting, setConnecting] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const streamAbort = useRef<AbortController | null>(null);

  const clearError = useCallback(() => setError(null), []);

  const stopStream = useCallback(() => {
    streamAbort.current?.abort();
    streamAbort.current = null;
  }, []);

  const startStream = useCallback(() => {
    stopStream();

    const controller = new AbortController();
    streamAbort.current = controller;

    void (async () => {
      try {
        for await (const next of streamBrowserFrames(controller.signal)) {
          if (controller.signal.aborted) return;
          setFrame(next);
        }
      } catch (err) {
        if (controller.signal.aborted) return;
        // A dropped stream is reported through the status poll, which carries
        // a better message than the fetch failure does.
        console.debug("browser frame stream ended", err);
      }
    })();
  }, [stopStream]);

  const connect = useCallback(
    async (port?: number, tabId?: string) => {
      setConnecting(true);
      setError(null);
      try {
        const next = await connectBrowser(port, tabId);
        setStatus(next);
        startStream();
      } catch (err) {
        setError(err instanceof Error ? err.message : String(err));
        setStatus(null);
      } finally {
        setConnecting(false);
      }
    },
    [startStream],
  );

  const disconnect = useCallback(async () => {
    stopStream();
    setFrame(null);
    setTabs([]);
    try {
      const next = await disconnectBrowser();
      setStatus(next);
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    }
  }, [stopStream]);

  const refreshTabs = useCallback(async () => {
    try {
      setTabs(await listBrowserTabs());
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    }
  }, []);

  const selectTab = useCallback(
    async (tabId: string) => {
      setError(null);
      try {
        await selectBrowserTab(tabId);
        // Attaching replaces the socket, so the frame stream has to be
        // reopened against the new one.
        startStream();
        setStatus(await getBrowserStatus());
      } catch (err) {
        setError(err instanceof Error ? err.message : String(err));
      }
    },
    [startStream],
  );

  const navigate = useCallback(async (url: string) => {
    setError(null);
    try {
      await navigateBrowser(url);
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    }
  }, []);

  // Poll status only while the pane is open.
  useEffect(() => {
    if (!enabled) {
      stopStream();
      setStatus(null);
      setFrame(null);
      return;
    }

    let cancelled = false;

    const poll = async () => {
      try {
        const next = await getBrowserStatus();
        if (!cancelled) setStatus(next);
      } catch {
        // Leave the last known status in place; the app server may just be
        // restarting.
      }
    };

    void poll();
    const timer = setInterval(poll, STATUS_POLL_MS);

    return () => {
      cancelled = true;
      clearInterval(timer);
    };
  }, [enabled, stopStream]);

  // The manager stays connected while the pane is hidden, so reopening finds
  // a live connection but no stream. Without this the pane would sit on
  // "waiting for the first frame" forever.
  useEffect(() => {
    if (!enabled || !status?.connected) return;
    if (streamAbort.current) return;
    startStream();
  }, [enabled, status?.connected, startStream]);

  // Tear the stream down when the pane closes or the component unmounts.
  useEffect(() => stopStream, [stopStream]);

  return {
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
    clearError,
  };
}
