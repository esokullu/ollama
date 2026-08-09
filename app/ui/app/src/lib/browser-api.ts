import { API_BASE } from "./config";
import { parseJsonlFromResponse } from "@/util/jsonl-parsing";

/** One screencast image. Width/height are the page's CSS-pixel viewport, which
 * is the coordinate space input events must be expressed in. */
export interface BrowserFrame {
  data: string; // base64 JPEG
  width: number;
  height: number;
  scrollX: number;
  scrollY: number;
}

export interface BrowserStatus {
  connected: boolean;
  port: number;
  browser?: string;
  tabId?: string;
  tabTitle?: string;
  tabUrl?: string;
  /** webbrain_connection's prose, shown verbatim when the bridge is down. */
  webbrain?: string;
  /** Whether the WebBrain extension has dialled into the bridge. */
  attached: boolean;
  lastError?: string;
}

export interface BrowserTab {
  id: string;
  type: string;
  title: string;
  url: string;
}

export interface BrowserTaskResult {
  text: string;
  isError: boolean;
}

async function request<T>(
  path: string,
  init?: RequestInit,
): Promise<T> {
  const response = await fetch(`${API_BASE}/api/v1/browser${path}`, {
    headers: { "Content-Type": "application/json" },
    ...init,
  });

  if (!response.ok) {
    // The server answers errors as {"error": "..."}; fall back to the status
    // line when the body is not JSON.
    let message = response.statusText;
    try {
      const body = await response.json();
      if (body?.error) message = body.error;
    } catch {
      // keep statusText
    }
    throw new Error(message);
  }

  if (response.status === 204) return undefined as T;
  return response.json();
}

export function getBrowserStatus(): Promise<BrowserStatus> {
  return request<BrowserStatus>("/status");
}

export function connectBrowser(
  port?: number,
  tabId?: string,
): Promise<BrowserStatus> {
  return request<BrowserStatus>("/connect", {
    method: "POST",
    body: JSON.stringify({ port: port ?? 0, tabId: tabId ?? "" }),
  });
}

export function disconnectBrowser(): Promise<BrowserStatus> {
  return request<BrowserStatus>("/disconnect", { method: "POST" });
}

export async function listBrowserTabs(): Promise<BrowserTab[]> {
  const body = await request<{ tabs: BrowserTab[] }>("/tabs");
  return body.tabs ?? [];
}

export function selectBrowserTab(tabId: string): Promise<BrowserTab> {
  return request<BrowserTab>("/tab", {
    method: "POST",
    body: JSON.stringify({ tabId }),
  });
}

export function navigateBrowser(url: string): Promise<{ url: string }> {
  return request<{ url: string }>("/navigate", {
    method: "POST",
    body: JSON.stringify({ url }),
  });
}

export interface MouseInput {
  kind: "mouse";
  type: "mousePressed" | "mouseReleased" | "mouseMoved" | "mouseWheel";
  x: number;
  y: number;
  button?: string;
  buttons?: number;
  clickCount?: number;
  deltaX?: number;
  deltaY?: number;
  modifiers?: number;
}

export interface KeyInput {
  kind: "key";
  type: "keyDown" | "keyUp" | "char" | "rawKeyDown";
  text?: string;
  unmodifiedText?: string;
  key?: string;
  code?: string;
  windowsVirtualKeyCode?: number;
  modifiers?: number;
}

/** Input is fire-and-forget: a dropped mouse move is not worth surfacing, and
 * awaiting each event would add a round trip to every pointer motion. */
export function sendBrowserInput(input: MouseInput | KeyInput): void {
  void request<void>("/input", {
    method: "POST",
    body: JSON.stringify(input),
  }).catch(() => {
    // Ignore; the status poll reports a lost connection.
  });
}

export function runBrowserTask(
  task: string,
  mode: "ask" | "act",
): Promise<BrowserTaskResult> {
  return request<BrowserTaskResult>("/task", {
    method: "POST",
    body: JSON.stringify({ task, mode }),
  });
}

export function getBrowserTaskStatus(
  runId?: string,
): Promise<BrowserTaskResult> {
  const query = runId ? `?runId=${encodeURIComponent(runId)}` : "";
  return request<BrowserTaskResult>(`/task${query}`);
}

export function abortBrowserTask(runId: string): Promise<BrowserTaskResult> {
  return request<BrowserTaskResult>("/task/abort", {
    method: "POST",
    body: JSON.stringify({ runId }),
  });
}

export function respondBrowserTask(
  runId: string,
  clarifyId: string,
  answer: string,
): Promise<BrowserTaskResult> {
  return request<BrowserTaskResult>("/task/respond", {
    method: "POST",
    body: JSON.stringify({ runId, clarifyId, answer }),
  });
}

/** Streams frames until the signal aborts. The stream stays open across page
 * navigations, so it is opened once per connection rather than per tab. */
export async function* streamBrowserFrames(
  signal: AbortSignal,
): AsyncGenerator<BrowserFrame, void, unknown> {
  const response = await fetch(`${API_BASE}/api/v1/browser/stream`, { signal });
  if (!response.ok) {
    throw new Error(`frame stream failed: ${response.statusText}`);
  }
  yield* parseJsonlFromResponse<BrowserFrame>(response);
}
