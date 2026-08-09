import type { BrowserFrame, KeyInput } from "./browser-api";

/** CDP's modifier bitmask (Input domain). */
export const MOD_ALT = 1;
export const MOD_CTRL = 2;
export const MOD_META = 4;
export const MOD_SHIFT = 8;

export interface Rect {
  left: number;
  top: number;
  width: number;
  height: number;
}

export interface Point {
  x: number;
  y: number;
}

/**
 * Maps a pointer position inside the pane onto page CSS pixels.
 *
 * The frame is drawn with `object-contain object-top`: scaled to fit, centred
 * horizontally, pinned to the top. The vertical pin matters because a wide
 * browser window inside a narrow pane leaves a lot of slack, and a page that
 * hangs in the middle of the pane does not read as a browser.
 *
 * This must stay in step with the image's CSS. Clicks landing outside the drawn
 * image return null rather than being clamped to the nearest edge, which would
 * silently click the wrong thing.
 */
export function pageCoordsFromPointer(
  clientX: number,
  clientY: number,
  rect: Rect,
  frame: Pick<BrowserFrame, "width" | "height">,
): Point | null {
  if (frame.width <= 0 || frame.height <= 0) return null;
  if (rect.width <= 0 || rect.height <= 0) return null;

  const scale = Math.min(rect.width / frame.width, rect.height / frame.height);
  const drawnWidth = frame.width * scale;
  const offsetX = (rect.width - drawnWidth) / 2;
  const offsetY = 0;

  const x = (clientX - rect.left - offsetX) / scale;
  const y = (clientY - rect.top - offsetY) / scale;

  if (x < 0 || y < 0 || x > frame.width || y > frame.height) return null;

  return { x, y };
}

interface ModifierSource {
  altKey: boolean;
  ctrlKey: boolean;
  metaKey: boolean;
  shiftKey: boolean;
}

export function modifiersFrom(event: ModifierSource): number {
  return (
    (event.altKey ? MOD_ALT : 0) |
    (event.ctrlKey ? MOD_CTRL : 0) |
    (event.metaKey ? MOD_META : 0) |
    (event.shiftKey ? MOD_SHIFT : 0)
  );
}

/** CDP wants a mouse button name, not the numeric index a DOM event carries. */
export function mouseButtonName(button: number): string {
  switch (button) {
    case 0:
      return "left";
    case 1:
      return "middle";
    case 2:
      return "right";
    default:
      return "none";
  }
}

/** Keys whose virtual key code Chrome needs in order to act on them. Printable
 * characters are delivered as `char` events instead and do not need one. */
const VIRTUAL_KEY_CODES: Record<string, number> = {
  Backspace: 8,
  Tab: 9,
  Enter: 13,
  Escape: 27,
  Space: 32,
  PageUp: 33,
  PageDown: 34,
  End: 35,
  Home: 36,
  ArrowLeft: 37,
  ArrowUp: 38,
  ArrowRight: 39,
  ArrowDown: 40,
  Delete: 46,
};

export function virtualKeyCode(key: string): number {
  if (key === " ") return VIRTUAL_KEY_CODES.Space;
  if (VIRTUAL_KEY_CODES[key] !== undefined) return VIRTUAL_KEY_CODES[key];
  if (key.length === 1) return key.toUpperCase().charCodeAt(0);
  return 0;
}

interface KeyEventSource extends ModifierSource {
  key: string;
  code: string;
}

/**
 * Translates a DOM keydown into the CDP events Chrome expects.
 *
 * A printable character needs both a keyDown and a `char` event: keyDown alone
 * moves focus and fires handlers but inserts nothing, which shows up as a page
 * that will not accept typing. Holding Ctrl or Meta makes the keystroke a
 * shortcut rather than text, so the `char` event is dropped in that case.
 */
export function keyInputsForKeyDown(event: KeyEventSource): KeyInput[] {
  const modifiers = modifiersFrom(event);
  const printable = event.key.length === 1;
  const asShortcut = event.ctrlKey || event.metaKey;

  const down: KeyInput = {
    kind: "key",
    type: "keyDown",
    key: event.key,
    code: event.code,
    windowsVirtualKeyCode: virtualKeyCode(event.key),
    modifiers,
  };

  if (!printable || asShortcut) {
    // Enter must still carry its text or forms will not submit on it.
    if (event.key === "Enter") {
      down.text = "\r";
    }
    return [down];
  }

  down.text = event.key;
  down.unmodifiedText = event.key;

  return [
    down,
    {
      kind: "key",
      type: "char",
      text: event.key,
      unmodifiedText: event.key,
      key: event.key,
      modifiers,
    },
  ];
}

export function keyInputForKeyUp(event: KeyEventSource): KeyInput {
  return {
    kind: "key",
    type: "keyUp",
    key: event.key,
    code: event.code,
    windowsVirtualKeyCode: virtualKeyCode(event.key),
    modifiers: modifiersFrom(event),
  };
}
