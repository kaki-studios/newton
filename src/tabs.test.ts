import { test } from 'node:test';
import assert from 'node:assert/strict';
import { createTabs } from './tabs.js';

/**
 * The tabs only need a handful of DOM members, so the tests drive a small stand-in
 * instead of a browser. `element` is handed to `createTabs`; everything else is
 * observation: which panel is hidden, which tab has focus, what aria-selected says.
 */
interface StubTab {
  element: HTMLElement;
  readonly hidden: boolean;
  readonly tabIndex: number;
  readonly focused: boolean;
  readonly selected: string | null;
  click(): void;
  /** Press a key; returns whether the handler called preventDefault. */
  press(key: string): boolean;
  blur(): void;
}

function stub(id: string): StubTab {
  const attributes = new Map<string, string>();
  const handlers = new Map<string, (event: KeyboardEvent) => void>();
  let focused = false;

  const element = {
    id,
    hidden: false,
    tabIndex: -1,
    focus() {
      focused = true;
    },
    getAttribute(name: string) {
      return attributes.get(name) ?? null;
    },
    setAttribute(name: string, value: string) {
      attributes.set(name, String(value));
    },
    addEventListener(type: string, listener: (event: KeyboardEvent) => void) {
      handlers.set(type, listener);
    },
  };

  return {
    element: element as unknown as HTMLElement,
    get hidden() {
      return element.hidden;
    },
    get tabIndex() {
      return element.tabIndex;
    },
    get focused() {
      return focused;
    },
    get selected() {
      return attributes.get('aria-selected') ?? null;
    },
    click() {
      handlers.get('click')?.({} as KeyboardEvent);
    },
    press(key) {
      let prevented = false;
      const event = {
        key,
        preventDefault() {
          prevented = true;
        },
      } as unknown as KeyboardEvent;
      handlers.get('keydown')?.(event);
      return prevented;
    },
    blur() {
      focused = false;
    },
  };
}

function setup(initialId?: string) {
  const tabChat = stub('tab-chat');
  const viewChat = stub('chat');
  const tabJson = stub('tab-json');
  const viewJson = stub('scene-json');

  const tabs = createTabs(
    [
      { tab: tabChat.element, panel: viewChat.element },
      { tab: tabJson.element, panel: viewJson.element },
    ],
    initialId,
  );

  return { tabs, tabChat, viewChat, tabJson, viewJson };
}

test('the first tab starts selected and every other panel hidden', () => {
  const { tabChat, viewChat, tabJson, viewJson } = setup();

  assert.equal(tabChat.selected, 'true');
  assert.equal(tabChat.tabIndex, 0);
  assert.equal(viewChat.hidden, false);

  assert.equal(tabJson.selected, 'false');
  assert.equal(tabJson.tabIndex, -1);
  assert.equal(viewJson.hidden, true);
});

test('clicking a tab reveals its panel and hides the other one', () => {
  const { tabChat, viewChat, tabJson, viewJson } = setup();

  tabJson.click();

  assert.equal(viewJson.hidden, false);
  assert.equal(viewChat.hidden, true);
  assert.equal(tabJson.selected, 'true');
  assert.equal(tabJson.tabIndex, 0);
  assert.equal(tabChat.selected, 'false');
  assert.equal(tabChat.tabIndex, -1);
});

test('arrow keys move selection and focus, wrapping at both ends', () => {
  const { tabs, tabChat, tabJson } = setup();

  assert.equal(tabChat.press('ArrowRight'), true, 'arrows must not scroll the page');
  assert.equal(tabs.selected, 'tab-json');
  assert.equal(tabJson.focused, true);

  tabChat.blur();
  tabJson.blur();
  assert.equal(tabJson.press('ArrowRight'), true);
  assert.equal(tabs.selected, 'tab-chat', 'ArrowRight on the last tab wraps to the first');
  assert.equal(tabChat.focused, true);

  tabChat.blur();
  assert.equal(tabChat.press('ArrowLeft'), true);
  assert.equal(tabs.selected, 'tab-json', 'ArrowLeft on the first tab wraps to the last');
  assert.equal(tabJson.focused, true);
});

test('Home and End jump to the first and last tab', () => {
  const { tabs, tabChat, tabJson } = setup();

  assert.equal(tabChat.press('End'), true);
  assert.equal(tabs.selected, 'tab-json');
  assert.equal(tabJson.focused, true);

  assert.equal(tabJson.press('Home'), true);
  assert.equal(tabs.selected, 'tab-chat');
  assert.equal(tabChat.focused, true);
});

test('unrelated keys keep their normal meaning and change nothing', () => {
  const { tabs, tabChat } = setup();

  assert.equal(tabChat.press('Tab'), false, 'Tab must move focus, not be swallowed');
  assert.equal(tabChat.press('a'), false);
  assert.equal(tabs.selected, 'tab-chat');
});

test('select() shows a tab by id and rejects an unknown one with the known ids', () => {
  const { tabs, viewChat, viewJson } = setup();

  tabs.select('tab-json');
  assert.equal(tabs.selected, 'tab-json');
  assert.equal(viewJson.hidden, false);
  assert.equal(viewChat.hidden, true);

  assert.throws(
    () => tabs.select('tab-scenes'),
    /no tab with id "tab-scenes": the panel has "tab-chat", "tab-json"/,
  );
});

test('an initial tab can be chosen, and a wrong one fails loudly', () => {
  const { viewChat, viewJson } = setup('tab-json');
  assert.equal(viewJson.hidden, false);
  assert.equal(viewChat.hidden, true);

  assert.throws(() => setup('tab-nope'), /no tab with id "tab-nope"/);
});

test('markup mistakes are reported by their fix, not silently ignored', () => {
  assert.throws(() => createTabs([]), /at least one tab\/panel pair/);

  assert.throws(
    () => createTabs([{ tab: stub('').element, panel: stub('chat').element }]),
    /a tab has no id: index\.html needs one/,
  );

  assert.throws(
    () => createTabs([{ tab: stub('tab-chat').element, panel: stub('').element }]),
    /the panel behind tab #tab-chat has no id/,
  );

  assert.throws(
    () =>
      createTabs([
        { tab: stub('tab-chat').element, panel: stub('chat').element },
        { tab: stub('tab-chat').element, panel: stub('scene-json').element },
      ]),
    /two tabs share the id "tab-chat": tab ids have to be unique/,
  );
});
