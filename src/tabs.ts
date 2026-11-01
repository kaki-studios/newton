/**
 * Tabs for the side panel: exactly one panel is visible at a time, and the tablist
 * behaves the way a keyboard or screen-reader user expects — roving tabindex, arrow
 * keys, Home/End, and `aria-selected` as the single source of truth for the styling.
 *
 * Markup owns the roles and ids (`role="tab"`, `aria-controls`, `aria-labelledby`);
 * this only keeps them in sync. No DOM queries, so the keyboard logic is testable.
 */
export interface TabPanel {
  /** The element the user clicks: a `<button role="tab">` with an id. */
  tab: HTMLElement;
  /** The panel it reveals: `[role="tabpanel"]` with an id, toggled via `hidden`. */
  panel: HTMLElement;
}

export interface Tabs {
  /** Id of the visible tab. */
  readonly selected: string;
  /** Show the panel behind this tab. Throws on an unknown id rather than hiding everything. */
  select(id: string): void;
}

/** `initialId` defaults to the first pair, which is also the markup order users see. */
export function createTabs(pairs: readonly TabPanel[], initialId?: string): Tabs {
  if (pairs.length === 0) {
    throw new Error('tabs need at least one tab/panel pair: otherwise nothing can be visible');
  }

  const ids = new Set<string>();
  for (const { tab, panel } of pairs) {
    if (!tab.id) throw new Error('a tab has no id: index.html needs one so its panel can be labelled');
    if (!panel.id) throw new Error(`the panel behind tab #${tab.id} has no id: aria-labelledby needs one`);
    if (ids.has(tab.id)) throw new Error(`two tabs share the id "${tab.id}": tab ids have to be unique`);
    ids.add(tab.id);
  }

  function indexOf(id: string): number {
    const index = pairs.findIndex(({ tab }) => tab.id === id);
    if (index < 0) {
      const known = pairs.map(({ tab }) => `"${tab.id}"`).join(', ');
      throw new Error(`no tab with id "${id}": the panel has ${known}`);
    }
    return index;
  }

  let selected = indexOf(initialId ?? pairs[0]!.tab.id);

  function show(index: number): void {
    pairs.forEach(({ tab, panel }, i) => {
      const active = i === index;
      tab.setAttribute('aria-selected', active ? 'true' : 'false');
      tab.tabIndex = active ? 0 : -1;
      panel.hidden = !active;
    });
    selected = index;
  }

  /** Move `delta` tabs, wrapping, and take focus with the selection. */
  function move(from: number, delta: number): void {
    const next = (from + delta + pairs.length) % pairs.length;
    show(next);
    pairs[next]!.tab.focus();
  }

  pairs.forEach(({ tab }, index) => {
    tab.addEventListener('click', () => show(index));
    tab.addEventListener('keydown', (event) => {
      switch (event.key) {
        case 'ArrowRight':
          move(index, 1);
          break;
        case 'ArrowLeft':
          move(index, -1);
          break;
        case 'Home':
          show(0);
          pairs[0]!.tab.focus();
          break;
        case 'End':
          show(pairs.length - 1);
          pairs.at(-1)!.tab.focus();
          break;
        default:
          return; // every other key keeps its normal meaning
      }
      event.preventDefault(); // arrows would otherwise scroll the page
    });
  });

  show(selected);

  return {
    get selected() {
      return pairs[selected]!.tab.id;
    },
    select: (id) => show(indexOf(id)),
  };
}
