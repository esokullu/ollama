import { describe, expect, it } from "vitest";
import {
  MOD_CTRL,
  MOD_META,
  MOD_SHIFT,
  keyInputForKeyUp,
  keyInputsForKeyDown,
  mouseButtonName,
  modifiersFrom,
  pageCoordsFromPointer,
  virtualKeyCode,
} from "./browser-input";

const noModifiers = {
  altKey: false,
  ctrlKey: false,
  metaKey: false,
  shiftKey: false,
};

describe("pageCoordsFromPointer", () => {
  it("maps a click to page pixels when the frame fills the pane", () => {
    // Element is exactly half the page's size, so coordinates double.
    const point = pageCoordsFromPointer(
      50,
      100,
      { left: 0, top: 0, width: 400, height: 300 },
      { width: 800, height: 600 },
    );
    expect(point).toEqual({ x: 100, y: 200 });
  });

  it("accounts for the element's position on screen", () => {
    const point = pageCoordsFromPointer(
      130,
      70,
      { left: 30, top: 20, width: 800, height: 600 },
      { width: 800, height: 600 },
    );
    expect(point).toEqual({ x: 100, y: 50 });
  });

  it("pins the frame to the top, leaving the slack below", () => {
    // A 800x600 page in an 800x800 element: drawn at the top, 200px spare
    // underneath. A wide browser window in a narrow pane is the common case,
    // and centring it vertically does not read as a browser.
    const rect = { left: 0, top: 0, width: 800, height: 800 };
    const frame = { width: 800, height: 600 };

    // The very top of the element is the top of the page, not a bar.
    expect(pageCoordsFromPointer(400, 0, rect, frame)).toEqual({ x: 400, y: 0 });
    expect(pageCoordsFromPointer(400, 300, rect, frame)).toEqual({
      x: 400,
      y: 300,
    });
    // Below the drawn image belongs to no page coordinate.
    expect(pageCoordsFromPointer(400, 700, rect, frame)).toBeNull();
  });

  it("still centres horizontally, so side bars are excluded", () => {
    // A 600x800 page in an 800x800 element leaves 100px bars either side.
    const rect = { left: 0, top: 0, width: 800, height: 800 };
    const frame = { width: 600, height: 800 };

    expect(pageCoordsFromPointer(50, 400, rect, frame)).toBeNull();
    expect(pageCoordsFromPointer(750, 400, rect, frame)).toBeNull();
    expect(pageCoordsFromPointer(400, 400, rect, frame)).toEqual({
      x: 300,
      y: 400,
    });
  });

  it("returns null rather than guessing on a degenerate frame", () => {
    const rect = { left: 0, top: 0, width: 800, height: 600 };
    expect(pageCoordsFromPointer(10, 10, rect, { width: 0, height: 0 })).toBeNull();
    expect(
      pageCoordsFromPointer(10, 10, { ...rect, width: 0 }, { width: 8, height: 6 }),
    ).toBeNull();
  });
});

describe("modifiersFrom", () => {
  it("packs the CDP bitmask", () => {
    expect(modifiersFrom(noModifiers)).toBe(0);
    expect(modifiersFrom({ ...noModifiers, shiftKey: true })).toBe(MOD_SHIFT);
    expect(
      modifiersFrom({ ...noModifiers, ctrlKey: true, metaKey: true }),
    ).toBe(MOD_CTRL | MOD_META);
  });
});

describe("mouseButtonName", () => {
  it("names the buttons CDP knows", () => {
    expect(mouseButtonName(0)).toBe("left");
    expect(mouseButtonName(1)).toBe("middle");
    expect(mouseButtonName(2)).toBe("right");
    expect(mouseButtonName(9)).toBe("none");
  });
});

describe("virtualKeyCode", () => {
  it("uses the well-known codes for navigation keys", () => {
    expect(virtualKeyCode("Enter")).toBe(13);
    expect(virtualKeyCode("Backspace")).toBe(8);
    expect(virtualKeyCode("ArrowDown")).toBe(40);
    expect(virtualKeyCode(" ")).toBe(32);
  });

  it("uppercases single characters", () => {
    expect(virtualKeyCode("a")).toBe(65);
    expect(virtualKeyCode("Z")).toBe(90);
  });
});

describe("keyInputsForKeyDown", () => {
  it("sends a char event alongside a printable key", () => {
    // Without the char event the page receives the keystroke but inserts
    // nothing, which reads as "typing does not work".
    const events = keyInputsForKeyDown({ ...noModifiers, key: "a", code: "KeyA" });
    expect(events).toHaveLength(2);
    expect(events[0].type).toBe("keyDown");
    expect(events[0].text).toBe("a");
    expect(events[1].type).toBe("char");
    expect(events[1].text).toBe("a");
  });

  it("omits the char event for a shortcut", () => {
    const events = keyInputsForKeyDown({
      ...noModifiers,
      metaKey: true,
      key: "a",
      code: "KeyA",
    });
    expect(events).toHaveLength(1);
    expect(events[0].modifiers).toBe(MOD_META);
  });

  it("omits the char event for a non-printable key", () => {
    const events = keyInputsForKeyDown({
      ...noModifiers,
      key: "ArrowLeft",
      code: "ArrowLeft",
    });
    expect(events).toHaveLength(1);
    expect(events[0].windowsVirtualKeyCode).toBe(37);
  });

  it("gives Enter its carriage return so forms submit", () => {
    const events = keyInputsForKeyDown({
      ...noModifiers,
      key: "Enter",
      code: "Enter",
    });
    expect(events).toHaveLength(1);
    expect(events[0].text).toBe("\r");
  });
});

describe("keyInputForKeyUp", () => {
  it("mirrors the key down", () => {
    const event = keyInputForKeyUp({
      ...noModifiers,
      shiftKey: true,
      key: "b",
      code: "KeyB",
    });
    expect(event.type).toBe("keyUp");
    expect(event.code).toBe("KeyB");
    expect(event.modifiers).toBe(MOD_SHIFT);
  });
});
