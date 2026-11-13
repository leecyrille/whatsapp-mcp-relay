<script lang="ts">
  import { api, type SearchHit } from "../lib/api";
  import { listTime, snippetParts } from "../lib/format";
  import { groupOf, openChat, setTwoColumns, ui } from "../lib/state.svelte";
  import Avatar from "../lib/Avatar.svelte";
  import Icon from "../lib/Icon.svelte";
  import ChatColumn from "./ChatColumn.svelte";
  import SortDialog from "./SortDialog.svelte";
  import NewGroupDialog from "./NewGroupDialog.svelte";

  let query = $state("");
  let hits = $state<SearchHit[]>([]);
  let searching = $state(false);
  let searchMs = $state<number | null>(null);
  let sorting = $state(false);
  let newMenu = $state(false);
  let creating = $state<"whatsapp" | "custom" | null>(null);
  let windowWidth = $state(innerWidth);

  // Widths are remembered on this PC. The stylesheet also keeps room for the chat beside them however the
  // window changes, and below 760 px the single list takes the whole window instead.
  const RAIL = 56;
  const CHAT_MIN = 380;
  const SIDE = { key: "wa-sidebar-width", usual: 340, min: 260, max: 640 };
  const COLUMN = { usual: 300, min: 220, max: 520 };
  // Below this, two columns and a usable chat don't fit, so it shows the single list.
  const TWO_COLUMNS_FROM = 900;

  function saved(key: string, usual: number, min: number, max: number): number {
    try {
      const value = Number(localStorage.getItem(key));
      return value >= min && value <= max ? value : usual;
    } catch {
      return usual;
    }
  }
  function save(key: string, value: number) {
    try {
      localStorage.setItem(key, String(value));
    } catch {}
  }
  let sideWidth = $state(saved(SIDE.key, SIDE.usual, SIDE.min, SIDE.max));
  let peopleWidth = $state(saved("wa-people-width", COLUMN.usual, COLUMN.min, COLUMN.max));
  let groupsWidth = $state(saved("wa-groups-width", COLUMN.usual, COLUMN.min, COLUMN.max));
  const sideMax = () => Math.max(SIDE.min, Math.min(SIDE.max, innerWidth - RAIL - CHAT_MIN));
  const columnMax = (other: number) => Math.max(COLUMN.min, Math.min(COLUMN.max, innerWidth - RAIL - CHAT_MIN - other));

  const two = $derived(ui.twoColumns && windowWidth >= TWO_COLUMNS_FROM);

  let seq = 0;
  async function runSearch(q: string) {
    const mine = ++seq;
    if (!q.trim()) {
      hits = [];
      searching = false;
      searchMs = null;
      return;
    }
    searching = true;
    const start = performance.now();
    try {
      const found = await api.search(q, 100);
      if (mine === seq) {
        hits = found;
        searchMs = Math.round(performance.now() - start);
      }
    } finally {
      if (mine === seq) searching = false;
    }
  }

  // Search is one view over everything in two columns. With one list, it stays within the selected tab.
  const groupIds = $derived(new Set(ui.groups.groups.map((g) => g.id)));
  const unreadJids = $derived(new Set(ui.chats.filter((c) => c.unread > 0).map((c) => c.jid)));
  function inSearch(jid: string): boolean {
    const filter = two ? "all" : ui.filter;
    if (filter === "all") return true;
    if (filter === "sys:unread") return unreadJids.has(jid);
    if (filter === "sys:people") return !jid.endsWith("@g.us");
    if (filter === "sys:groups") return jid.endsWith("@g.us");
    const g = groupOf(jid);
    if (filter === "unsorted") return !g || !groupIds.has(g);
    return g === filter;
  }
  const filtered = $derived.by(() => {
    const q = query.trim().toLowerCase();
    return ui.chats.filter((c) => inSearch(c.jid) && (!q || c.name.toLowerCase().includes(q)));
  });
  const shownHits = $derived(hits.filter((h) => inSearch(h.chat)));

  // A clicked notification has to show its row, so a search in the way is cleared.
  $effect(() => {
    if (ui.reveal) {
      query = "";
      void runSearch("");
    }
  });

  function onKey(e: KeyboardEvent) {
    if (e.key === "Escape") {
      query = "";
      runSearch("");
      (e.target as HTMLElement).blur();
    }
  }
</script>

<svelte:window bind:innerWidth={windowWidth} onclick={() => (newMenu = false)} onkeydown={(e) => e.key === "Escape" && (newMenu = false)} />

<aside class="side" class:two class:steady={!ui.blink} aria-label="Chats" style:--side-width="{sideWidth}px" style:--people-width="{peopleWidth}px" style:--groups-width="{groupsWidth}px">
  <div class="top">
    <div class="search">
      <Icon name="search" size={16} />
      <input
        id="search"
        type="search"
        placeholder="Search chats and messages"
        aria-label="Search chats and messages"
        bind:value={query}
        oninput={() => runSearch(query)}
        onkeydown={onKey}
        spellcheck="false"
        autocomplete="off"
      />
    </div>
    <button class="icon-btn" title="Sort chats with Claude" aria-label="Sort chats with Claude" onclick={() => (sorting = true)}>
      <Icon name="sparkle" size={18} />
    </button>
  </div>

  <div class="new-row">
    <button class="new-group" aria-haspopup="menu" aria-expanded={newMenu} onclick={(e) => (e.stopPropagation(), (newMenu = !newMenu))}>
      <Icon name="plus" size={14} /> New group
    </button>
    <button
      class="layout-toggle"
      aria-pressed={ui.twoColumns}
      title={ui.twoColumns && !two ? "The window is too narrow for two columns, so this shows one list." : "People on the left, groups in their own column."}
      onclick={() => setTwoColumns(!ui.twoColumns)}
    >
      Groups column
    </button>
    {#if newMenu}
      <div class="new-menu" role="menu">
        <button role="menuitem" onclick={() => ((newMenu = false), (creating = "whatsapp"))}>
          <b>New WhatsApp group</b>
          <small>A real group. The people you add are notified.</small>
        </button>
        <button role="menuitem" onclick={() => ((newMenu = false), (creating = "custom"))}>
          <b>New custom group</b>
          <small>Only for sorting chats in WA Relay. Nobody is told.</small>
        </button>
      </div>
    {/if}
  </div>

  {#if query.trim()}
    <div class="results">
      {#if filtered.length}
        <div class="section">Chats</div>
        {#each filtered.slice(0, 8) as chat (chat.jid)}
          <button class="row" class:on={ui.active === chat.jid} onclick={() => openChat(chat.jid)}>
            <Avatar name={chat.name} jid={chat.jid} isGroup={chat.is_group} />
            <div class="main"><div class="name">{chat.name}</div></div>
          </button>
        {/each}
      {/if}
      <div class="section">
        Messages
        {#if searchMs !== null && !searching}<span class="faint">{shownHits.length}{shownHits.length === 100 ? "+" : ""} in {searchMs} ms</span>{/if}
      </div>
      {#each shownHits as hit (hit.rowid)}
        <button class="hit" onclick={() => openChat(hit.chat, hit.rowid)}>
          <div class="hit-top">
            <span class="name">{hit.chat_name}</span>
            <span class="time">{listTime(hit.ts)}</span>
          </div>
          <div class="snippet">
            {#if hit.sender_name}<span class="who">{hit.sender_name}: </span>{/if}{#each snippetParts(hit.snippet) as part}{#if part.hit}<mark>{part.text}</mark>{:else}{part.text}{/if}{/each}
          </div>
        </button>
      {:else}
        {#if !searching}<p class="empty">No messages match.</p>{/if}
      {/each}
    </div>
  {:else if two}
    <div class="columns">
      <ChatColumn
        kind="people"
        label="the people list"
        bind:filter={ui.peopleFilter}
        width={peopleWidth}
        min={COLUMN.min}
        max={() => columnMax(groupsWidth)}
        usual={COLUMN.usual}
        onwidth={(w) => (peopleWidth = w)}
        onsave={() => save("wa-people-width", peopleWidth)}
      />
      <ChatColumn
        kind="groups"
        label="the groups list"
        bind:filter={ui.groupsFilter}
        width={groupsWidth}
        min={COLUMN.min}
        max={() => columnMax(peopleWidth)}
        usual={COLUMN.usual}
        onwidth={(w) => (groupsWidth = w)}
        onsave={() => save("wa-groups-width", groupsWidth)}
      />
    </div>
  {:else}
    <ChatColumn
      kind="all"
      label="the chat list"
      bind:filter={ui.filter}
      width={sideWidth}
      min={SIDE.min}
      max={sideMax}
      usual={SIDE.usual}
      onwidth={(w) => (sideWidth = w)}
      onsave={() => save(SIDE.key, sideWidth)}
    />
  {/if}
</aside>

{#if sorting}
  <SortDialog onclose={() => (sorting = false)} />
{/if}

{#if creating}
  <NewGroupDialog kind={creating} onclose={() => (creating = null)} />
{/if}

<style>
  .side {
    position: relative;
    /* Stays narrow enough for a usable chat beside it, whatever width was saved. 56 px is the rail. */
    width: clamp(260px, var(--side-width), max(260px, calc(100vw - 56px - 380px)));
    flex: none;
    background: var(--panel);
    border-right: 1px solid var(--line);
    display: flex;
    flex-direction: column;
    min-height: 0;
  }
  .side.two {
    width: calc(var(--people-width) + var(--groups-width));
    flex: 0 1 auto;
    min-width: 440px;
  }
  .columns {
    flex: 1;
    min-height: 0;
    display: flex;
  }
  /* The chat shrinks no further than this, so the columns give way first. */
  :global(.content:has(.side.two) .convo) {
    min-width: 380px;
  }
  @media (max-width: 760px) {
    .side {
      width: 100%;
      border-right: 0;
    }
    :global(.in-chat) .side {
      display: none;
    }
  }
  .top {
    display: flex;
    gap: 4px;
    align-items: center;
    padding: 10px 8px 6px 12px;
  }
  .search {
    flex: 1;
    display: flex;
    align-items: center;
    gap: 7px;
    padding: 0 10px;
    background: var(--panel-2);
    border-radius: 9px;
    color: var(--muted);
    border: 1px solid transparent;
    min-width: 0;
  }
  .search:focus-within {
    border-color: var(--accent);
  }
  .search input {
    flex: 1;
    min-width: 0;
    background: none;
    border: 0;
    outline: none;
    padding: 7px 0;
    color: var(--text);
    user-select: text;
  }
  .new-row {
    position: relative;
    display: flex;
    justify-content: space-between;
    align-items: center;
    gap: 6px;
    padding: 0 12px 6px;
    flex: none;
  }
  .new-group {
    display: inline-flex;
    align-items: center;
    gap: 5px;
    padding: 3px 10px;
    border-radius: 14px;
    border: 1px solid var(--accent);
    color: var(--accent);
    font-size: 12.5px;
    font-weight: 600;
    white-space: nowrap;
  }
  .new-group:hover {
    background: var(--active);
  }
  .layout-toggle {
    padding: 3px 10px;
    border-radius: 14px;
    border: 1px solid var(--line);
    color: var(--muted);
    font-size: 12.5px;
    white-space: nowrap;
  }
  .layout-toggle:hover {
    color: var(--text);
  }
  .layout-toggle[aria-pressed="true"] {
    background: var(--accent);
    border-color: var(--accent);
    color: var(--accent-text);
  }
  @media (max-width: 899.98px) {
    .layout-toggle {
      display: none;
    }
  }
  .new-menu {
    position: absolute;
    left: 12px;
    top: calc(100% - 2px);
    z-index: 30;
    width: min(290px, calc(100vw - 80px));
    background: var(--panel);
    border: 1px solid var(--line);
    border-radius: 9px;
    box-shadow: var(--shadow);
    padding: 4px;
  }
  .new-menu button {
    display: block;
    width: 100%;
    padding: 7px 9px;
    border-radius: 6px;
    text-align: left;
  }
  .new-menu button:hover {
    background: var(--hover);
  }
  .new-menu b,
  .new-menu small {
    display: block;
  }
  .new-menu small {
    color: var(--muted);
    font-size: 12px;
  }
  .results {
    flex: 1;
    overflow-y: auto;
    min-height: 0;
  }
  .section {
    padding: 10px 14px 4px;
    font-size: 11.5px;
    font-weight: 700;
    text-transform: uppercase;
    letter-spacing: 0.04em;
    color: var(--muted);
    display: flex;
    justify-content: space-between;
  }
  .section .faint {
    font-weight: 400;
    text-transform: none;
    letter-spacing: 0;
  }
  .hit {
    width: 100%;
    display: block;
    text-align: left;
    padding: 8px 14px;
  }
  .hit:hover {
    background: var(--hover);
  }
  .hit-top {
    display: flex;
    gap: 8px;
    align-items: baseline;
  }
  .snippet {
    color: var(--muted);
    font-size: 13px;
    display: -webkit-box;
    -webkit-line-clamp: 2;
    line-clamp: 2;
    -webkit-box-orient: vertical;
    overflow: hidden;
  }
  .who {
    color: var(--text);
  }
  mark {
    background: var(--mark);
    color: inherit;
    border-radius: 2px;
  }
</style>
