<script lang="ts" module>
  import type { Outgoing as OutgoingItem } from "./Composer.svelte";
  // Outside the component, so a message that hasn't gone out stays put when the user looks at another tab.
  const outbox = $state<{ items: OutgoingItem[] }>({ items: [] });
</script>

<script lang="ts">
  import { onDestroy, tick, untrack } from "svelte";
  import { api, errorText, newId, type MemberList, type Message, type Picked } from "../lib/api";
  import { dayLabel, sameDay } from "../lib/format";
  import { closeChat, groupOf, on, setBusy, toast, ui } from "../lib/state.svelte";
  import { personColours } from "../lib/people";
  import Avatar from "../lib/Avatar.svelte";
  import Icon from "../lib/Icon.svelte";
  import MessageRow from "./MessageRow.svelte";
  import Composer, { type Outgoing } from "./Composer.svelte";
  import MessageDialogs from "./MessageDialogs.svelte";
  import ChatActions from "./ChatActions.svelte";

  const PAGE = 60;
  const MAX_REFRESH = 300;

  // Newest first: the list is laid out bottom-up, so older messages join without moving what's on screen.
  let rows = $state<Message[]>([]);
  let scroller = $state<HTMLDivElement>();
  let atEnd = $state(false); // No older messages in the cache.
  let loadingOlder = false;
  let detached = $state(false); // Viewing around a search result, not the newest messages.
  let showDown = $state(false);
  let highlight = $state<number | null>(null);
  let replyTo = $state<Message | null>(null);
  let askingPhone = $state(false);
  let lightbox = $state<Message | null>(null);
  let renaming = $state<{ jid: string; name: string } | null>(null);

  const chat = $derived(ui.chats.find((c) => c.jid === ui.active) ?? null);
  const group = $derived(ui.active ? ui.groups.groups.find((g) => g.id === groupOf(ui.active!)) : undefined);
  const groupMode = $derived(ui.active?.endsWith("@g.us") ?? false);
  // Who's in a group. The saved list shows at once, then it's asked for again. Only opening the chat or a
  // change to the group asks, never a message.
  let members = $state<MemberList["members"]>([]);
  let membersStale = $state(false);
  let membersToken = 0;
  async function freshMembers(jid: string, mine: number) {
    try {
      const fresh = await api.groupMembersRefresh(jid);
      if (mine === membersToken) {
        members = fresh.members;
        membersStale = false;
      }
    } catch {
      if (mine === membersToken) membersStale = true;
    }
  }
  // What this copy already knows about who's in a group. Messages wait for it, so they don't open in one
  // set of colours and then change to another.
  async function openRoster(jid: string) {
    const mine = ++membersToken;
    const [saved, spoke] = await Promise.all([api.groupMembers(jid), api.chatSenders(jid).catch(() => [])]);
    if (mine !== membersToken) return;
    members = saved.members;
    senders = spoke;
    membersStale = false;
    void freshMembers(jid, mine);
  }
  let senders = $state<string[]>([]);
  let rosterReady: Promise<void> = Promise.resolve();
  $effect(() => {
    const jid = ui.active;
    untrack(() => {
      members = [];
      senders = [];
      membersStale = false;
      membersToken++;
      rosterReady = jid?.endsWith("@g.us") ? openRoster(jid).catch(() => {}) : Promise.resolve();
    });
  });

  // A hidden ID in a group's member list may be the only link between that ID and the person's number.
  const sameAs = $derived.by(() => {
    const links = new Map<string, string>();
    for (const m of members) {
      if (m.lid && m.lid !== m.person) links.set(m.lid, m.person);
      if (m.jid !== m.person) links.set(m.jid, m.person);
    }
    return links;
  });
  const personOf = (m: Message) => sameAs.get(m.person) ?? m.person;
  const speaker = (m: Message) => (m.from_me ? "me" : personOf(m));
  // Everyone who has a colour here: the whole member list, anyone who has written, and whoever is on
  // screen. The user isn't one of them: their messages are already green and on the right. As a string, so the
  // colours are only worked out again when the people change, not with every message.
  const roster = $derived.by(() => {
    if (!groupMode) return "";
    const everyone = new Set<string>();
    for (const m of members) if (!m.you) everyone.add(m.person);
    for (const s of senders) everyone.add(sameAs.get(s) ?? s);
    for (const m of rows) if (m.person) everyone.add(personOf(m));
    return [...everyone].sort().join(" ");
  });
  const colours = $derived(personColours(roster ? roster.split(" ") : []));

  const chatPending = $derived(outbox.items.filter((p) => p.chat === ui.active));
  const disconnected = $derived(!["ok", "connecting", "catching_up"].includes(ui.conn.link));

  // These only live in memory, so an automatic update waits for them.
  $effect(() => setBusy("outbox", outbox.items.length > 0));

  // Loads when the chat changes. Only ui.active and ui.jumpTo are tracked, so a chat-list refresh
  // doesn't reload the messages.
  let loadToken = 0;
  $effect(() => {
    const jid = ui.active;
    const jump = ui.jumpTo;
    untrack(() => {
      void load(jid, jump);
    });
  });

  let editing = $state<Message | null>(null);
  let forwarding = $state<Message | null>(null);
  let deleting = $state<Message | null>(null);
  let info = $state<Message | null>(null);

  async function saveEdit(m: Message, text: string, mentions: string[]) {
    try {
      await api.editMessage(m.raw_chat, m.id, text, mentions);
      editing = null;
      await refresh();
    } catch (e) {
      toast(errorText(e));
    }
  }

  async function load(jid: string | null, jump: number | null) {
    const mine = ++loadToken;
    replyTo = null;
    editing = null;
    showDown = false;
    atEnd = false;
    if (!jid) {
      rows = [];
      return;
    }
    if (jump) {
      const around = await api.around(jump);
      await rosterReady;
      if (mine !== loadToken) return;
      rows = around.reverse();
      detached = true;
      highlight = jump;
      await tick();
      document.getElementById(`m${jump}`)?.scrollIntoView({ block: "center" });
      setTimeout(() => {
        if (highlight === jump) highlight = null;
      }, 2500);
    } else {
      const page = await api.page(jid, null, null, PAGE);
      await rosterReady;
      if (mine !== loadToken) return;
      rows = page;
      detached = false;
      highlight = null;
      atEnd = page.length < PAGE;
      await tick();
      if (scroller) scroller.scrollTop = 0;
      markReadIfSeen();
      // A short chat may not fill the window, so older messages wouldn't load by scrolling.
      maybeLoadOlder();
    }
  }

  async function refresh() {
    const jid = ui.active;
    if (!jid || detached) return;
    const fresh = await api.page(jid, null, null, Math.min(Math.max(rows.length, PAGE), MAX_REFRESH));
    if (jid !== ui.active || detached) return;
    if (rows.length > fresh.length && fresh.length) {
      const oldest = fresh[fresh.length - 1];
      // Only this chat's rows: a refresh can land while the list still holds the chat the user just left.
      const keep = rows.filter((m) => m.chat === jid && (m.ts < oldest.ts || (m.ts === oldest.ts && m.rowid < oldest.rowid)));
      rows = withoutRepeats([...fresh, ...keep]);
    } else {
      rows = fresh;
    }
    const isNear = !scroller || scroller.scrollTop > -80;
    if (isNear) markReadIfSeen();
  }

  function markReadIfSeen() {
    if (ui.active && !detached && document.hasFocus() && document.visibilityState === "visible" && (!scroller || scroller.scrollTop > -160)) {
      void api.markRead(ui.active);
    }
  }

  /** Keeps the first of any row that's listed twice. A repeat would stop the list drawing. */
  function withoutRepeats(list: Message[]): Message[] {
    const seen = new Set<number>();
    return list.filter((m) => !seen.has(m.rowid) && !!seen.add(m.rowid));
  }

  async function maybeLoadOlder() {
    if (!scroller || atEnd || loadingOlder || !ui.active || rows.length === 0) return;
    if (scroller.scrollHeight + scroller.scrollTop - scroller.clientHeight > 900) return;
    const jid = ui.active;
    const oldest = rows[rows.length - 1];
    // Just after switching chats the list can still hold the last one, whose oldest row means nothing here.
    if (oldest.chat !== jid) return;
    loadingOlder = true;
    try {
      const older = await api.page(jid, oldest.ts, oldest.rowid, PAGE);
      // Anything that replaced the list meanwhile wins, or these would land in the wrong place.
      if (jid !== ui.active || rows[rows.length - 1]?.rowid !== oldest.rowid) return;
      if (older.length < PAGE) atEnd = true;
      rows = withoutRepeats([...rows, ...older]);
    } finally {
      loadingOlder = false;
    }
    await tick();
    maybeLoadOlder();
  }

  async function maybeLoadNewer() {
    if (!detached || !scroller || !ui.active || scroller.scrollTop < -700 || rows.length === 0) return;
    const jid = ui.active;
    const newest = rows[0];
    if (newest.chat !== jid) return;
    const newer = await api.after(jid, newest.ts, newest.rowid, 100);
    if (jid !== ui.active || rows[0]?.rowid !== newest.rowid) return;
    if (newer.length === 0) {
      detached = false;
      return;
    }
    rows = withoutRepeats([...newer.reverse(), ...rows]);
    if (newer.length < 100) detached = false;
  }

  function onScroll() {
    if (!scroller) return;
    showDown = scroller.scrollTop < -300 || detached;
    maybeLoadOlder();
    maybeLoadNewer();
    if (scroller.scrollTop > -80) markReadIfSeen();
  }

  function jumpDown() {
    if (detached) {
      void load(ui.active, null);
      ui.jumpTo = null;
    } else if (scroller) {
      scroller.scrollTop = 0;
    }
  }

  function jumpToQuoted(id: string) {
    const target = rows.find((m) => m.id === id);
    if (!target) {
      toast("That message is further back. Scroll up to find it.");
      return;
    }
    document.getElementById(`m${target.rowid}`)?.scrollIntoView({ block: "center", behavior: "smooth" });
    highlight = target.rowid;
    setTimeout(() => {
      if (highlight === target.rowid) highlight = null;
    }, 1800);
  }

  async function assign(group: string) {
    if (!ui.active) return;
    try {
      ui.groups = await api.assign([ui.active], group);
    } catch (e) {
      toast(errorText(e));
    }
  }

  // Sending. An outgoing message shows straight away and is replaced by the real row when it lands.
  function quoteOf(m: Message | null) {
    if (!m) return null;
    return { id: m.id, sender: m.from_me ? "" : m.sender_jid, text: m.text || (m.media ? `[${m.media}]` : "") };
  }

  async function sendText(text: string, mentions: string[], shown: string) {
    if (!ui.active) return;
    const item: Outgoing = {
      clientId: newId(),
      chat: ui.active,
      text,
      mentions,
      shown,
      state: "sending",
      kind: "text",
      quote: quoteOf(replyTo),
    };
    outbox.items.push(item);
    replyTo = null;
    scrollToEnd();
    void deliverText(item);
  }

  async function deliverText(item: Outgoing) {
    const current = outbox.items.find((p) => p.clientId === item.clientId);
    if (current) {
      current.state = "sending";
      current.error = undefined;
    }
    try {
      await api.sendText(item.chat, item.text, item.quote, item.clientId, item.mentions ?? []);
      outbox.items = outbox.items.filter((p) => p.clientId !== item.clientId);
      await refresh();
    } catch (e) {
      const failed = outbox.items.find((p) => p.clientId === item.clientId);
      if (failed) {
        failed.state = "failed";
        failed.error = errorText(e);
      }
    }
  }

  async function sendFile(
    file: Picked,
    caption: string,
    kind: string,
    thumb: string | null,
    width: number,
    height: number,
    seconds: number,
    mentions: string[],
    shown: string,
  ) {
    if (!ui.active) return;
    const item: Outgoing = {
      clientId: newId(),
      chat: ui.active,
      text: caption,
      mentions,
      shown,
      state: "sending",
      kind: "file",
      name: file.name,
      size: file.size,
      done: 0,
      token: file.token,
      fileKind: kind,
      thumb,
      width,
      height,
      seconds,
      quote: quoteOf(replyTo),
    };
    outbox.items.push(item);
    replyTo = null;
    scrollToEnd();
    void deliverFile(item);
  }

  async function deliverFile(item: Outgoing) {
    const current = outbox.items.find((p) => p.clientId === item.clientId);
    if (current) {
      current.state = "sending";
      current.error = undefined;
    }
    try {
      await api.sendFile({
        chat: item.chat,
        token: item.token!,
        kind: item.fileKind!,
        caption: item.text,
        thumb: item.thumb ?? null,
        width: item.width ?? 0,
        height: item.height ?? 0,
        seconds: item.seconds ?? 0,
        quote: item.quote,
        clientId: item.clientId,
        mentions: item.mentions ?? [],
      });
      // Uploaded. The home PC now sends it to WhatsApp, and a send-result event reports the outcome.
    } catch (e) {
      const failed = outbox.items.find((p) => p.clientId === item.clientId);
      if (failed) {
        failed.state = "failed";
        failed.error = errorText(e);
      }
    }
  }

  function retry(item: Outgoing) {
    if (item.kind === "text") void deliverText(item);
    else void deliverFile(item);
  }

  function discard(item: Outgoing) {
    outbox.items = outbox.items.filter((p) => p.clientId !== item.clientId);
    if (item.token) void api.forgetToken(item.token);
  }

  function scrollToEnd() {
    requestAnimationFrame(() => {
      if (scroller) scroller.scrollTop = 0;
    });
  }

  const unsubscribers = [
    // A person's name changed, here or on another computer. Saved names are worked out each time, so this
    // shows it without asking WhatsApp.
    on("contacts-changed", () => {
      const jid = ui.active;
      if (!jid) return;
      void refresh();
      if (jid.endsWith("@g.us")) {
        const mine = membersToken;
        void api.groupMembers(jid).then((saved) => {
          if (mine === membersToken && saved.members.length) members = saved.members;
        });
      }
    }),
    on("group-members-changed", (jid: string) => {
      if (jid === ui.active) void freshMembers(jid, ++membersToken);
    }),
    on("chats-changed", (changed: string[]) => {
      if (ui.active && (changed.length === 0 || changed.includes(ui.active))) void refresh();
    }),
    on("send-progress", (p) => {
      const item = outbox.items.find((x) => x.clientId === p.client_id);
      if (item) item.done = p.done;
    }),
    on("send-result", (r) => {
      const item = outbox.items.find((x) => x.clientId === r.client_id);
      if (!item) return;
      if (r.ok) {
        outbox.items = outbox.items.filter((x) => x.clientId !== r.client_id);
        void refresh();
      } else {
        item.state = "failed";
        item.error = r.error ?? "Couldn't send that file.";
      }
    }),
  ];
  onDestroy(() => unsubscribers.forEach((off) => off()));

  async function askPhone() {
    const oldest = rows[rows.length - 1];
    if (!oldest) return;
    askingPhone = true;
    try {
      await api.loadOlder(oldest.raw_chat);
      toast("Asked your phone for older messages. They'll appear here if it has them. Your phone needs to be online.");
    } catch (e) {
      toast(errorText(e));
    } finally {
      setTimeout(() => (askingPhone = false), 4000);
    }
  }
</script>

<svelte:window onfocus={markReadIfSeen} />

<section class="convo" class:hidden-narrow={!ui.active}>
  {#if !ui.active}
    <div class="blank">
      <Icon name="chats" size={46} />
      <p>Choose a chat</p>
      <p class="muted small">Ctrl+K searches all your messages.</p>
    </div>
  {:else}
    <header class:group-header={groupMode}>
      <button class="icon-btn back" onclick={closeChat} aria-label="Back to chats">
        <Icon name="back" />
      </button>
      <Avatar name={chat?.name ?? ""} jid={ui.active} isGroup={groupMode} size={38} editable />
      <div class="title">
        <div class="name">{chat?.name ?? ""}</div>
        {#if ui.active.endsWith("@s.whatsapp.net")}
          <div class="sub selectable">+{ui.active.split("@")[0]}</div>
        {:else if groupMode}
          <div class="members selectable" role="list" aria-label="People in this group">
            <span class="members-count">
              {#if members.length}{members.length} {members.length === 1 ? "person" : "people"}{membersStale ? ", couldn't refresh" : ""}{:else if membersStale}Group. Couldn't get its members{:else}Group{/if}
            </span>
            {#each members as person (person.jid)}
              <!-- svelte-ignore a11y_no_noninteractive_element_interactions -->
              <span
                class="member"
                role="listitem"
                title={person.you ? person.name : `${person.name}. Right-click to rename.`}
                oncontextmenu={(e) => {
                  if (person.you) return;
                  e.preventDefault();
                  renaming = { jid: person.person, name: person.name };
                }}
              >
                <span class="member-name" class:you={person.you}>{person.you ? "You" : person.name}</span>{#if person.admin}<small>admin</small>{/if}
              </span>
            {/each}
          </div>
        {/if}
      </div>
      <label class="group-pick" title="Group">
        {#if group}<span class="dot" style:background={group.color}></span>{/if}
        <select aria-label="Group for this chat" value={groupOf(ui.active)} onchange={(e) => assign(e.currentTarget.value)}>
          <option value="">Unsorted</option>
          {#each ui.groups.groups as g (g.id)}
            <option value={g.id}>{g.name}</option>
          {/each}
        </select>
      </label>
      <ChatActions jid={ui.active} name={chat?.name ?? ""} isGroup={groupMode} bind:person={renaming} />
    </header>

    <div class="scroller selectable" bind:this={scroller} onscroll={onScroll}>
      <div class="pad"></div>
      {#each chatPending.toReversed() as item, i (item.clientId)}
        <div class="out" class:gap={i === chatPending.length - 1 && rows[0] && !rows[0].from_me}>
          <div class="pending" class:failed={item.state === "failed"}>
            {#if item.kind === "file"}
              <div class="file-line">
                <Icon name="file" size={18} />
                <span class="fname">{item.name}</span>
              </div>
              {#if item.state === "sending"}
                <div class="bar"><div style:width="{item.size ? Math.round(((item.done ?? 0) / item.size) * 100) : 0}%"></div></div>
              {/if}
            {/if}
            {#if item.shown || item.text}<div class="ptext">{item.shown || item.text}</div>{/if}
            {#if item.state === "failed"}
              <div class="fail">
                <Icon name="alert" size={14} />
                <span><b>Not sent.</b> {item.error}</span>
                <button onclick={() => retry(item)}>Retry</button>
                <button onclick={() => discard(item)}>Discard</button>
              </div>
            {:else}
              <div class="sending" title="Sending"><Icon name="clock" size={12} /></div>
            {/if}
          </div>
        </div>
      {/each}
      {#each rows as m, i (m.rowid)}
        {@const older = rows[i + 1]}
        {@const newDay = !older || !sameDay(older.ts, m.ts)}
        {@const startsRun = newDay || speaker(older) !== speaker(m)}
        <MessageRow
          msg={m}
          person={personOf(m)}
          personName={groupMode ? m.sender_name : (chat?.name ?? "")}
          colour={colours.get(personOf(m))}
          {startsRun}
          gap={startsRun && !newDay}
          highlighted={highlight === m.rowid}
          showSender={groupMode && !m.from_me && startsRun}
          onreply={(msg) => {
            editing = null;
            replyTo = msg;
          }}
          onforward={(msg) => (forwarding = msg)}
          onedit={(msg) => {
            replyTo = null;
            editing = msg;
          }}
          ondelete={(msg) => (deleting = msg)}
          oninfo={(msg) => (info = msg)}
          onquote={jumpToQuoted}
          onview={(msg) => (lightbox = msg)}
        />
        {#if i === rows.length - 1 || !sameDay(rows[i + 1].ts, m.ts)}
          <div class="day"><span>{dayLabel(m.ts)}</span></div>
        {/if}
      {/each}
      {#if rows.length > 0 && atEnd}
        <div class="older">
          <button class="btn" onclick={askPhone} disabled={askingPhone}>
            <Icon name="history" size={15} />
            {askingPhone ? "Asked your phone…" : "Load older messages from phone"}
          </button>
        </div>
      {/if}
    </div>

    {#if showDown}
      <button class="down" onclick={jumpDown} aria-label="Go to newest messages">
        <Icon name="back" size={18} />
      </button>
    {/if}

    {#key ui.active}
      {#if disconnected}
        <p class="offline-note" role="status">Not connected, so anything you send now won't go out. It stays here marked Not sent, and only goes if you press Retry.</p>
      {/if}
      <Composer
        chat={ui.active}
        {replyTo}
        oncancelreply={() => (replyTo = null)}
        onsend={sendText}
        onfile={sendFile}
        isGroup={groupMode}
        {members}
        {membersStale}
        {editing}
        oncanceledit={() => (editing = null)}
        onedit={saveEdit}
      />
    {/key}
  {/if}
</section>

<MessageDialogs bind:forwarding bind:deleting bind:info />

{#if lightbox}
  {#await import("./Lightbox.svelte") then { default: Lightbox }}
    <Lightbox msg={lightbox} onclose={() => (lightbox = null)} />
  {/await}
{/if}

<style>
  .convo {
    flex: 1;
    min-width: 0;
    display: flex;
    flex-direction: column;
    background: var(--chat-bg);
    position: relative;
  }
  @media (max-width: 760px) {
    .hidden-narrow {
      display: none;
    }
  }
  .blank {
    margin: auto;
    text-align: center;
    color: var(--faint);
  }
  .blank p {
    margin: 8px 0 0;
    font-size: 16px;
    color: var(--muted);
  }
  .blank .small {
    font-size: 13px;
  }
  /* Grows to fit everyone in a group, up to a limit that leaves room for the messages. */
  .group-header {
    height: auto;
    min-height: 58px;
    padding-top: 8px;
    padding-bottom: 8px;
    align-items: flex-start;
  }
  .members {
    display: flex;
    flex-wrap: wrap;
    gap: 1px 10px;
    margin-top: 1px;
    max-height: min(26vh, 170px);
    overflow-y: auto;
    scrollbar-width: thin;
    font-size: 12px;
    color: var(--muted);
  }
  .members-count {
    font-weight: 600;
  }
  .member {
    display: inline-flex;
    align-items: baseline;
    gap: 4px;
    min-width: 0;
    max-width: 100%;
  }
  .member-name {
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    max-width: 280px;
  }
  .member-name.you {
    font-weight: 600;
  }
  .member small {
    flex: none;
    font-size: 10.5px;
    color: var(--accent);
  }
  header {
    height: 58px;
    flex: none;
    display: flex;
    align-items: center;
    gap: 10px;
    padding: 0 12px;
    background: var(--panel);
    border-bottom: 1px solid var(--line);
  }
  .back {
    display: none;
    transform: none;
  }
  @media (max-width: 760px) {
    .back {
      display: inline-grid;
    }
  }
  .title {
    flex: 1;
    min-width: 0;
  }
  .title .name {
    font-weight: 600;
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
  }
  .sub {
    font-size: 12px;
    color: var(--muted);
  }
  .group-pick {
    display: inline-flex;
    align-items: center;
    gap: 6px;
    padding: 0 6px;
    border-radius: 7px;
    background: var(--panel-2);
    flex: none;
  }
  .group-pick select {
    background: none;
    border: 0;
    outline: none;
    padding: 5px 2px;
    font-size: 13px;
    max-width: 130px;
  }
  .group-pick select option {
    background: var(--panel);
    color: var(--text);
  }
  .scroller {
    flex: 1;
    min-height: 0;
    overflow-y: auto;
    display: flex;
    flex-direction: column-reverse;
    padding: 0 max(14px, calc((100% - 880px) / 2));
    overflow-anchor: none;
  }
  .pad {
    height: 10px;
    flex: none;
  }
  .day {
    text-align: center;
    padding: 10px 0;
    flex: none;
    user-select: none;
  }
  .day span {
    background: var(--panel);
    color: var(--muted);
    font-size: 12px;
    padding: 3px 12px;
    border-radius: 11px;
  }
  .older {
    padding: 14px 0;
    text-align: center;
    flex: none;
    user-select: none;
  }
  .older .btn {
    display: inline-flex;
    gap: 6px;
    align-items: center;
    font-size: 13px;
  }
  .down {
    position: absolute;
    right: 20px;
    bottom: 84px;
    width: 38px;
    height: 38px;
    border-radius: 50%;
    background: var(--panel);
    box-shadow: var(--shadow);
    display: grid;
    place-items: center;
    color: var(--muted);
  }
  .down :global(svg) {
    transform: rotate(-90deg);
  }
  .out {
    display: flex;
    justify-content: flex-end;
    padding: 1.5px 0;
    flex: none;
  }
  .out.gap {
    margin-top: 8px;
  }
  .pending {
    max-width: min(68%, 560px);
    background: var(--mine);
    border-radius: 10px;
    padding: 6px 10px;
    opacity: 0.8;
  }
  .pending.failed {
    opacity: 1;
    outline: 1px solid var(--danger);
  }
  .file-line {
    display: flex;
    align-items: center;
    gap: 7px;
  }
  .fname {
    font-weight: 600;
    overflow-wrap: anywhere;
  }
  .ptext {
    white-space: pre-wrap;
    overflow-wrap: anywhere;
  }
  .bar {
    height: 4px;
    background: rgb(0 0 0 / 0.12);
    border-radius: 2px;
    margin: 6px 0 2px;
    min-width: 180px;
  }
  .bar > div {
    height: 100%;
    background: var(--accent);
    border-radius: 2px;
    transition: width 0.2s;
  }
  .sending {
    text-align: right;
    color: var(--faint);
    height: 13px;
  }
  .fail {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: 6px;
    color: var(--danger);
    font-size: 12.5px;
    margin-top: 3px;
  }
  .offline-note {
    margin: 0;
    padding: 6px 14px;
    font-size: 12.5px;
    color: var(--danger);
    background: var(--panel);
    border-top: 1px solid var(--line);
  }
  .fail button {
    text-decoration: underline;
    font-weight: 600;
    color: inherit;
  }
</style>
