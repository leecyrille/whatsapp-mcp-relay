<script lang="ts">
  import { tick, untrack } from "svelte";
  import { api, errorText, type Chat } from "../lib/api";
  import { listTime } from "../lib/format";
  import { groupColor, groupOf, openChat, toast, ui } from "../lib/state.svelte";
  import Avatar from "../lib/Avatar.svelte";
  import Icon from "../lib/Icon.svelte";

  // One list of chats with its own tabs. The app shows one of these for everything, or, in two columns, one
  // for people and one for groups. Each has its own tab selection.
  let {
    kind,
    filter = $bindable(),
    width,
    min,
    max,
    usual,
    label,
    onwidth,
    onsave,
  }: {
    kind: "all" | "people" | "groups";
    filter: string;
    width: number;
    min: number;
    max: () => number;
    usual: number;
    label: string;
    onwidth: (width: number) => void;
    onsave: () => void;
  } = $props();

  const ROW = 70;
  const HEAD = 30;
  let scrollTop = $state(0);
  let viewport = $state(600);
  let menu = $state<{ x: number; y: number; chat: Chat } | null>(null);
  let listEl = $state<HTMLElement | null>(null);
  let columnEl = $state<HTMLElement | null>(null);
  let dragging = $state(false);

  function setWidth(next: number) {
    onwidth(Math.round(Math.max(min, Math.min(max(), next))));
  }
  function startDrag(e: PointerEvent) {
    e.preventDefault();
    (e.currentTarget as HTMLElement).setPointerCapture(e.pointerId);
    dragging = true;
  }
  function drag(e: PointerEvent) {
    if (dragging && columnEl) setWidth(e.clientX - columnEl.getBoundingClientRect().left);
  }
  function endDrag() {
    if (!dragging) return;
    dragging = false;
    onsave();
  }
  function resetWidth() {
    setWidth(usual);
    onsave();
  }
  function resizeKey(e: KeyboardEvent) {
    const steps: Record<string, number> = { ArrowLeft: width - 20, ArrowRight: width + 20, Home: min, End: max() };
    if (!(e.key in steps)) return;
    e.preventDefault();
    setWidth(steps[e.key]);
    onsave();
  }

  const groupIds = $derived(new Set(ui.groups.groups.map((g) => g.id)));
  // People and Groups come from the chat's address, so every chat is in exactly one, and a new chat lands
  // in the right one by itself. Custom groups are separate, and a chat can also be in one of those.
  const isGroupChat = (jid: string) => jid.endsWith("@g.us");
  const ofKind = (jid: string) => kind === "all" || isGroupChat(jid) === (kind === "groups");
  const pool = $derived(ui.chats.filter((c) => ofKind(c.jid)));

  // Opening a chat from Unread keeps it there while it's open, so it doesn't vanish while being read.
  let keptInUnread = $state<string | null>(null);
  $effect(() => {
    const active = ui.active;
    const current = filter;
    untrack(() => {
      const unread = ui.chats.find((c) => c.jid === active)?.unread ?? 0;
      keptInUnread = current === "sys:unread" && active && (unread > 0 || keptInUnread === active) ? active : null;
    });
  });
  const unreadJids = $derived(new Set(ui.chats.filter((c) => c.unread > 0).map((c) => c.jid)));

  function inFilter(jid: string): boolean {
    if (filter === "all") return true;
    if (filter === "sys:unread") return unreadJids.has(jid) || jid === keptInUnread;
    if (filter === "sys:people") return !isGroupChat(jid);
    if (filter === "sys:groups") return isGroupChat(jid);
    const g = groupOf(jid);
    if (filter === "unsorted") return !g || !groupIds.has(g);
    return g === filter;
  }

  const filtered = $derived(pool.filter((c) => inFilter(c.jid)));

  // Strictly newest first, as the list comes from the app, unread or not (The user, 2026-11-10). An unread chat
  // further down or up shows as a glow on that edge of the list instead of being moved.
  const listed = $derived(filtered);

  // Each tab shows how many chats it holds, plus a badge for how many of those have unread messages.
  const tabCounts = $derived.by(() => {
    const chats: Record<string, number> = {};
    const unread: Record<string, number> = {};
    for (const c of pool) {
      const g = groupOf(c.jid);
      const tab = g && groupIds.has(g) ? g : "unsorted";
      const group = isGroupChat(c.jid) ? "sys:groups" : "sys:people";
      for (const key of ["all", group, tab]) {
        chats[key] = (chats[key] ?? 0) + 1;
        if (c.unread > 0) unread[key] = (unread[key] ?? 0) + 1;
      }
      if (c.unread > 0) chats["sys:unread"] = (chats["sys:unread"] ?? 0) + 1;
    }
    return { chats, unread };
  });

  // With one list, a custom group's list is split under People and Groups, worked out the same way as the
  // top tabs. A heading only shows if there's something under it. In two columns they're already apart.
  type Item = { kind: "head"; key: string; label: string; top: number } | { kind: "chat"; key: string; chat: Chat; top: number };
  const layout = $derived.by(() => {
    const items: Item[] = [];
    let top = 0;
    const addChats = (chats: Chat[]) => {
      for (const chat of chats) {
        items.push({ kind: "chat", key: chat.jid, chat, top });
        top += ROW;
      }
    };
    if (kind === "all" && groupIds.has(filter)) {
      for (const [heading, chats] of [
        ["People", listed.filter((c) => !isGroupChat(c.jid))],
        ["Groups", listed.filter((c) => isGroupChat(c.jid))],
      ] as const) {
        if (!chats.length) continue;
        items.push({ kind: "head", key: `head:${heading}`, label: heading, top });
        top += HEAD;
        addChats(chats);
      }
    } else {
      addChats(listed);
    }
    return { items, height: top };
  });

  // The first item that starts at or below this height.
  function indexAt(y: number): number {
    const { items } = layout;
    let low = 0;
    let high = items.length;
    while (low < high) {
      const mid = (low + high) >> 1;
      if (items[mid].top < y) low = mid + 1;
      else high = mid;
    }
    return low;
  }

  // Only rows near the viewport are in the DOM, so a very long chat list stays smooth.
  const visible = $derived(layout.items.slice(indexAt(scrollTop - 6 * ROW), indexAt(scrollTop + viewport + 6 * ROW)));

  // Unread chats more than half out of view, above and below. Each edge with any glows, on the rows' beat,
  // and a click scrolls to the nearest one.
  const unreadTops = $derived(layout.items.filter((i) => i.kind === "chat" && i.chat.unread > 0).map((i) => i.top));
  const above = $derived(unreadTops.filter((top) => top + ROW / 2 < scrollTop));
  const below = $derived(unreadTops.filter((top) => top + ROW / 2 > scrollTop + viewport));
  let listWidth = $state(0);
  // The glow stops short of the scrollbar, so the scrollbar can still be dragged at its ends.
  const scrollbar = $derived(listEl && listWidth ? Math.max(0, listEl.offsetWidth - listEl.clientWidth) : 0);
  const lessMotion = window.matchMedia("(prefers-reduced-motion: reduce)");
  function scrollToUnread(top: number) {
    listEl?.scrollTo({ top: Math.max(0, top - ROW), behavior: lessMotion.matches ? "auto" : "smooth" });
  }
  const chats = (n: number) => (n === 1 ? "1 unread chat" : `${n} unread chats`);

  // Every unread row pulses on the same beat, in every column. A row that's new, moved or scrolled into view
  // would start its own pulse, so each is set against the page's one clock, which doesn't restart anything.
  $effect(() => {
    void visible;
    void above.length;
    void below.length;
    if (!columnEl) return;
    for (const pulse of columnEl.getAnimations({ subtree: true })) {
      if ((pulse as CSSAnimation).animationName === "unread-pulse") pulse.startTime = 0;
    }
  });

  // A clicked notification shows its chat's row: in this column if it's this kind of chat, leaving a tab that
  // doesn't hold it, and scrolling to it if needed.
  $effect(() => {
    const asked = ui.reveal;
    untrack(() => {
      const active = ui.active;
      if (!asked || !active || !ofKind(active)) return;
      if (!filtered.some((c) => c.jid === active)) filter = "all";
      void tick().then(() => {
        const row = layout.items.find((i) => i.key === active);
        if (row && listEl && (row.top < listEl.scrollTop || row.top + ROW > listEl.scrollTop + viewport)) {
          listEl.scrollTop = Math.max(0, row.top - ROW);
        }
      });
    });
  });

  async function move(chat: Chat, group: string) {
    menu = null;
    try {
      ui.groups = await api.assign([chat.jid], group);
    } catch (e) {
      toast(errorText(e));
    }
  }

  function openMenu(e: MouseEvent, chat: Chat) {
    e.preventDefault();
    menu = { x: Math.min(e.clientX, innerWidth - 190), y: Math.min(e.clientY, innerHeight - 40 - 34 * (ui.groups.groups.length + 1)), chat };
  }
</script>

{#snippet counts(tab: string)}
  <span class="chat-count">{tabCounts.chats[tab] ?? 0}</span>
  {#if tabCounts.unread[tab]}<b title="{tabCounts.unread[tab]} with unread messages">{tabCounts.unread[tab]}</b>{/if}
{/snippet}

<svelte:window onclick={() => (menu = null)} onkeydown={(e) => e.key === "Escape" && (menu = null)} />

<section class="column {kind}" class:dragging aria-label={label} bind:this={columnEl}>
  <div class="tabs system" role="tablist" aria-label="Filter {label} by type">
    <button role="tab" aria-selected={filter === "all"} class:on={filter === "all"} onclick={() => (filter = "all")}>
      All {@render counts("all")}
    </button>
    {#if kind === "all"}
      <button role="tab" aria-selected={filter === "sys:people"} class:on={filter === "sys:people"} onclick={() => (filter = "sys:people")}>
        People {@render counts("sys:people")}
      </button>
      <button role="tab" aria-selected={filter === "sys:groups"} class:on={filter === "sys:groups"} onclick={() => (filter = "sys:groups")}>
        Groups {@render counts("sys:groups")}
      </button>
    {/if}
    <button role="tab" aria-selected={filter === "sys:unread"} class:on={filter === "sys:unread"} onclick={() => (filter = "sys:unread")}>
      Unread <span class="chat-count">{tabCounts.chats["sys:unread"] ?? 0}</span>
    </button>
    <button role="tab" aria-selected={filter === "unsorted"} class:on={filter === "unsorted"} onclick={() => (filter = "unsorted")}>
      Unsorted {@render counts("unsorted")}
    </button>
  </div>
  {#if ui.groups.groups.length > 0}
    <div class="tabs custom" role="tablist" aria-label="Filter {label} by your own groups">
      {#each ui.groups.groups as g (g.id)}
        <button role="tab" title={g.name} aria-selected={filter === g.id} class:on={filter === g.id} onclick={() => (filter = g.id)}>
          <span class="dot" style:background={g.color}></span>
          <span class="tab-name">{g.name}</span>
          {@render counts(g.id)}
        </button>
      {/each}
    </div>
  {/if}

  <div class="list-wrap">
  <div class="list" bind:this={listEl} bind:clientHeight={viewport} bind:clientWidth={listWidth} onscroll={(e) => (scrollTop = e.currentTarget.scrollTop)}>
    {#if ui.loaded && filtered.length === 0}
      <p class="empty">
        {#if filter === "unsorted"}Everything here is sorted.{:else if filter === "sys:unread"}No unread chats.{:else if filter === "all" || filter.startsWith("sys:")}No chats yet.{:else}Nothing in this group yet. Right-click a chat to move it here.{/if}
      </p>
    {/if}
    <div style:height="{layout.height}px" style:position="relative">
      {#each visible as item (item.key)}
        {#if item.kind === "head"}
          <div class="list-head" role="heading" aria-level="3" style:top="{item.top}px">{item.label}</div>
        {:else}
          {@const chat = item.chat}
          <button
            class="row abs"
            class:on={ui.active === chat.jid}
            class:unread={chat.unread > 0}
            style:top="{item.top}px"
            style:--group-color={groupColor(chat.jid) ?? "transparent"}
            onclick={() => openChat(chat.jid)}
            oncontextmenu={(e) => openMenu(e, chat)}
          >
            <Avatar name={chat.name} jid={chat.jid} isGroup={chat.is_group} />
            <div class="main">
              <div class="line">
                <span class="name">{chat.name}</span>
                <span class="time" class:fresh={chat.unread > 0}>{listTime(chat.last_ms)}</span>
              </div>
              <div class="line">
                <span class="preview">
                  {#if chat.preview_from_me}
                    <span class="tick" class:read={chat.preview_status === "read" || chat.preview_status === "played"}>
                      <Icon name={chat.preview_status === "sent" ? "check" : "checks"} size={14} />
                    </span>
                  {/if}
                  {chat.preview}
                </span>
                {#if chat.unread > 0}
                  <span class="count" aria-label="{chat.unread} unread">{chat.unread > 99 ? "99+" : chat.unread}</span>
                {/if}
              </div>
            </div>
          </button>
        {/if}
      {/each}
    </div>
  </div>
  {#if above.length}
    <button
      class="edge above"
      style:right="{scrollbar}px"
      aria-label="{chats(above.length)} further up"
      title="{chats(above.length)} further up. Click to scroll to the nearest."
      onclick={() => scrollToUnread(above[above.length - 1])}
    ></button>
  {/if}
  {#if below.length}
    <button
      class="edge below"
      style:right="{scrollbar}px"
      aria-label="{chats(below.length)} further down"
      title="{chats(below.length)} further down. Click to scroll to the nearest."
      onclick={() => scrollToUnread(below[0])}
    ></button>
  {/if}
  </div>
  <!-- svelte-ignore a11y_no_noninteractive_tabindex, a11y_no_noninteractive_element_interactions -->
  <div
    class="resize"
    role="separator"
    tabindex="0"
    aria-orientation="vertical"
    aria-label="Resize {label}"
    aria-valuenow={width}
    aria-valuemin={min}
    aria-valuemax={max()}
    title="Drag to resize. Double-click for the usual width."
    onpointerdown={startDrag}
    onpointermove={drag}
    onpointerup={endDrag}
    onpointercancel={endDrag}
    ondblclick={resetWidth}
    onkeydown={resizeKey}
  ></div>
</section>

{#if menu}
  <div class="menu" style:left="{menu.x}px" style:top="{menu.y}px" role="menu">
    {#each ui.groups.groups as g (g.id)}
      <button role="menuitem" onclick={() => move(menu!.chat, g.id)}>
        <span class="dot" style:background={g.color}></span>
        {g.name}
        {#if groupOf(menu.chat.jid) === g.id}<span class="mark"><Icon name="check" size={14} /></span>{/if}
      </button>
    {/each}
    <button role="menuitem" onclick={() => move(menu!.chat, "")}>
      <span class="dot" style:background="var(--line)"></span>
      Unsorted
    </button>
  </div>
{/if}

<style>
  .column {
    position: relative;
    flex: 1 1 0;
    min-width: 0;
    min-height: 0;
    display: flex;
    flex-direction: column;
  }
  /* In two columns, the left one keeps its width while there's room, and both shrink before the chat does. */
  .column.people {
    flex: 0 1 var(--people-width);
    min-width: 220px;
    border-right: 1px solid var(--line);
  }
  .column.groups {
    flex: 1 1 var(--groups-width);
    min-width: 220px;
  }
  .resize {
    position: absolute;
    top: 0;
    bottom: 0;
    right: -4px;
    width: 8px;
    z-index: 5;
    cursor: col-resize;
    touch-action: none;
  }
  .resize:hover,
  .resize:focus-visible,
  .dragging .resize {
    background: linear-gradient(var(--accent), var(--accent)) center / 2px 100% no-repeat;
    outline: none;
  }
  @media (max-width: 760px) {
    .resize {
      display: none;
    }
  }
  .tabs {
    display: flex;
    flex-wrap: wrap;
    gap: 4px;
    padding: 2px 12px 8px;
    overflow-x: hidden;
    flex: none;
  }
  /* Wraps for as many groups as the user wants. Past about 5 lines it scrolls inside this space, so the chat
     list below always keeps room, and the built-in tabs above it stay in view. */
  .tabs.custom {
    max-height: min(32vh, 150px);
    overflow-y: auto;
    scrollbar-width: thin;
  }
  .tabs.system {
    padding-bottom: 4px;
  }
  .tabs button {
    padding: 3px 10px;
    border-radius: 14px;
    background: var(--panel-2);
    color: var(--muted);
    white-space: nowrap;
    font-size: 12.5px;
    display: inline-flex;
    align-items: center;
    gap: 5px;
    flex: none;
    max-width: 100%;
    min-width: 0;
  }
  /* A long name shortens with an ellipsis. The dot and the counts always show. */
  .tab-name {
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
  }
  .tabs button:hover {
    color: var(--text);
  }
  .tabs button.on {
    background: var(--accent);
    color: var(--accent-text);
  }
  .chat-count {
    font-size: 12px;
    opacity: 0.7;
    flex: none;
  }
  .tabs b {
    font-weight: 700;
    font-size: 11px;
    flex: none;
  }
  .list-wrap {
    position: relative;
    flex: 1;
    min-height: 0;
    display: flex;
    flex-direction: column;
  }
  .list {
    flex: 1;
    overflow-y: auto;
    min-height: 0;
  }
  /* The same amber and the same beat as an unread row, as a band along the edge that fades into the list.
     Only its opacity animates. ChatColumn sets it on the rows' clock. */
  .edge {
    position: absolute;
    left: 0;
    height: 22px;
    z-index: 3;
    padding: 0;
    border: 0;
    border-radius: 0;
    background: none;
    cursor: pointer;
  }
  .edge.above {
    top: 0;
  }
  .edge.below {
    bottom: 0;
  }
  .edge::before {
    content: "";
    position: absolute;
    inset: 0;
    pointer-events: none;
    animation: unread-pulse 1.6s ease-in-out infinite;
  }
  .edge.above::before {
    background: linear-gradient(to bottom, rgb(245 170 30 / 0.95), rgb(245 170 30 / 0.45) 35%, transparent);
    box-shadow: inset 0 3px 0 rgb(245 170 30);
  }
  .edge.below::before {
    background: linear-gradient(to top, rgb(245 170 30 / 0.95), rgb(245 170 30 / 0.45) 35%, transparent);
    box-shadow: inset 0 -3px 0 rgb(245 170 30);
  }
  .edge:focus-visible {
    outline: 2px solid var(--accent);
    outline-offset: -2px;
  }
  /* Steady, like the rows, for the setting and for Windows' fewer animations. */
  :global(.side.steady) .edge::before {
    animation: none;
  }
  @media (prefers-reduced-motion: reduce) {
    .edge::before {
      animation: none;
    }
  }
  .list-head {
    position: absolute;
    left: 0;
    right: 0;
    height: 30px;
    display: flex;
    align-items: flex-end;
    padding: 0 14px 4px;
    font-size: 12.5px;
    font-weight: 700;
    color: var(--muted);
    border-bottom: 1px solid var(--line);
  }
  .menu {
    position: fixed;
    background: var(--panel);
    border: 1px solid var(--line);
    border-radius: 9px;
    box-shadow: var(--shadow);
    padding: 4px;
    z-index: 30;
    min-width: 170px;
  }
  .menu button {
    display: flex;
    align-items: center;
    gap: 8px;
    width: 100%;
    padding: 6px 9px;
    border-radius: 6px;
    text-align: left;
  }
  .menu button:hover {
    background: var(--hover);
  }
  .mark {
    margin-left: auto;
    color: var(--accent);
    display: inline-flex;
  }
</style>
