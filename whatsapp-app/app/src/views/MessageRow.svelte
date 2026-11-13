<script lang="ts">
  import { api, errorText, fileUrl, thumbUrl, type Message } from "../lib/api";
  import { clock, duration, fileSize, segments } from "../lib/format";
  import { media, toast, ui } from "../lib/state.svelte";
  import type { PersonColour } from "../lib/people";
  import Avatar from "../lib/Avatar.svelte";
  import Icon from "../lib/Icon.svelte";
  import MessageMenu, { type MenuItem } from "./MessageMenu.svelte";
  import ReactionPicker from "./ReactionPicker.svelte";
  import DocPreview from "./DocPreview.svelte";
  import { previewKind } from "../lib/preview";

  let {
    msg,
    person,
    personName,
    colour,
    startsRun,
    gap,
    highlighted,
    showSender,
    onreply,
    onquote,
    onview,
    onforward,
    onedit,
    ondelete,
    oninfo,
  }: {
    msg: Message;
    person: string;
    personName: string;
    colour: PersonColour | undefined;
    startsRun: boolean;
    gap: boolean;
    highlighted: boolean;
    showSender: boolean;
    onreply: (m: Message) => void;
    onquote: (id: string) => void;
    onview: (m: Message) => void;
    onforward: (m: Message) => void;
    onedit: (m: Message) => void;
    ondelete: (m: Message) => void;
    oninfo: (m: Message) => void;
  } = $props();

  // WhatsApp's help pages: an edit is allowed for 15 minutes after sending.
  const EDIT_MS = 15 * 60 * 1000;
  let previewing = $state(false);
  const canPreview = $derived(msg.media === "document" && !msg.revoked && previewKind(msg.filename, msg.mime) !== null);
  const FORWARDABLE = ["image", "video", "audio", "document"];
  let menu = $state<{ x: number; y: number; items: MenuItem[] } | null>(null);

  /** The text as shown, with names for @mentions, for copying. */
  function shownText(): string {
    return msg.text.replace(/(?<![\p{L}\p{N}])@(\d+)/gu, (all, digits) => (msg.mentions?.[digits] ? `@${msg.mentions[digits]}` : all));
  }

  async function copy(text: string) {
    try {
      await navigator.clipboard.writeText(text);
      toast("Copied.");
    } catch {
      toast("Couldn't copy.");
    }
  }

  function menuItems(): MenuItem[] {
    const items: MenuItem[] = [];
    const selected = window.getSelection();
    const picked = selected && !selected.isCollapsed && document.getElementById(`m${msg.rowid}`)?.contains(selected.anchorNode) ? selected.toString() : "";
    if (picked) items.push({ label: "Copy selection", action: () => void copy(picked) });
    if (!msg.revoked) {
      items.push({ label: "Reply", action: () => onreply(msg) });
      items.push({ label: "React", action: () => (picker = true) });
      if ((msg.text.trim() && !msg.media) || FORWARDABLE.includes(msg.media)) items.push({ label: "Forward…", action: () => onforward(msg) });
      if (msg.text) items.push({ label: "Copy text", action: () => void copy(shownText()) });
      if (canPreview) items.push({ label: "Preview", action: () => (previewing = true) });
      if (msg.media) items.push({ label: "Save file…", action: () => void save() });
      if (msg.from_me && !msg.media && Date.now() - msg.ts < EDIT_MS) items.push({ label: "Edit", action: () => onedit(msg) });
      if (msg.from_me) items.push({ label: "Message info", action: () => oninfo(msg) });
    }
    items.push({ label: "Delete…", action: () => ondelete(msg), danger: true });
    return items;
  }

  function openMenu(e: MouseEvent) {
    // A link keeps the browser's own menu out, but never opens it either: links only open from a click.
    e.preventDefault();
    let { clientX: x, clientY: y } = e;
    if (!x && !y) {
      const box = (e.currentTarget as HTMLElement).getBoundingClientRect();
      x = box.left + 40;
      y = box.top + 20;
    }
    menu = { x, y, items: menuItems() };
  }

  function openMenuFrom(button: HTMLElement) {
    const box = button.getBoundingClientRect();
    menu = { x: box.left, y: box.bottom + 4, items: menuItems() };
  }

  const AUTO_IMAGE_LIMIT = 15 * 1024 * 1024;

  let picker = $state(false);
  let smile: HTMLButtonElement | undefined = $state();
  let wantVideo = $state(false);
  let wantAudio = $state(false);
  let imageReady = $state(false);
  let imageFailed = $state(false);

  const parts = $derived(msg.text ? segments(msg.text, msg.mentions) : []);
  const state = $derived(media[msg.rowid]);
  const box = $derived.by(() => {
    const maxW = 330;
    const maxH = 360;
    const w = msg.width || 300;
    const h = msg.height || 220;
    const scale = Math.min(1, maxW / w, maxH / h);
    return { w: Math.max(140, Math.round(w * scale)), h: Math.max(90, Math.round(h * scale)) };
  });
  const autoImage = $derived(ui.settings.auto_images && msg.size <= AUTO_IMAGE_LIMIT);
  const reactionList = $derived(msg.reactions ? Object.values(msg.reactions) : []);
  const reactionGroups = $derived.by(() => {
    const counts = new Map<string, number>();
    for (const r of reactionList) counts.set(r, (counts.get(r) ?? 0) + 1);
    return [...counts.entries()];
  });

  async function fetchThen(action: () => void) {
    try {
      await api.mediaFetch(msg.rowid);
      action();
    } catch (e) {
      toast(errorText(e));
    }
  }

  async function save() {
    try {
      if (await api.mediaSave(msg.rowid)) toast("Saved.");
    } catch (e) {
      toast(errorText(e));
    }
  }

  async function openFile() {
    try {
      await api.mediaOpen(msg.rowid);
    } catch (e) {
      toast(errorText(e));
    }
  }

  async function react(emoji: string) {
    picker = false;
    const mine = msg.reactions?.me === emoji;
    try {
      await api.react(msg.raw_chat, msg.id, msg.sender_jid, mine ? "" : emoji);
    } catch (e) {
      toast(errorText(e));
    }
  }

  function retryImage() {
    imageFailed = false;
    void fetchThen(() => {
      imageReady = false;
    });
  }

  function link(e: MouseEvent, url: string) {
    e.preventDefault();
    api.openLink(url).catch((err) => toast(errorText(err)));
  }

  const progress = $derived(state?.total ? Math.round(((state.done ?? 0) / state.total) * 100) : null);
</script>

<!-- svelte-ignore a11y_no_static_element_interactions -->
<div class="row" class:mine={msg.from_me} class:hl={highlighted} class:gap id="m{msg.rowid}" style:--who-light={colour?.light} style:--who-dark={colour?.dark} oncontextmenu={openMenu}>
  {#if !msg.from_me}
    <div class="face" title={person ? undefined : "An older message. This copy doesn't know who sent it."}>
      {#if startsRun}
        <Avatar name={personName || "Unknown"} jid={person} size={28} fill={colour ? "var(--who)" : person ? undefined : "var(--faint)"} ink={colour ? "var(--who-ink)" : undefined} />
      {/if}
    </div>
  {/if}
  <div class="bubble" class:coded={colour && !msg.from_me} class:sticker={msg.media === "sticker"} class:bare={msg.media === "image" || msg.media === "video"}>
    {#if showSender && msg.sender_name}
      <div class="sender">{msg.sender_name}</div>
    {/if}

    {#if msg.quoted_id && !msg.revoked}
      <button class="quote" onclick={() => onquote(msg.quoted_id)}>
        {#if msg.quoted_name}<b>{msg.quoted_name}</b>{/if}
        <span>{msg.quoted_text || "Earlier message"}</span>
      </button>
    {/if}

    {#if msg.revoked}
      <div class="gone"><Icon name="close" size={13} /> This message was deleted</div>
    {:else if msg.media === "image" || msg.media === "sticker"}
      <button
        class="pic"
        class:stick={msg.media === "sticker"}
        style:width="{msg.media === 'sticker' ? 160 : box.w}px"
        style:height="{msg.media === 'sticker' ? 160 : box.h}px"
        onclick={() => (imageReady ? onview(msg) : retryImage())}
        aria-label="View image"
      >
        {#if msg.has_thumb}
          <img src={thumbUrl(msg.rowid)} alt="" class="thumb" />
        {/if}
        {#if !imageFailed && (autoImage || imageReady)}
          <img
            src={fileUrl(msg.rowid)}
            alt=""
            class="full"
            class:ready={imageReady}
            loading="lazy"
            decoding="async"
            onload={() => (imageReady = true)}
            onerror={() => (imageFailed = true)}
          />
        {/if}
        {#if !imageReady}
          <span class="pic-state">
            {#if imageFailed}
              Retry
            {:else if state?.state === "preparing" || state?.state === "downloading"}
              {progress !== null ? `${progress}%` : "Loading…"}
            {:else if !autoImage}
              <Icon name="download" size={18} /> {fileSize(msg.size)}
            {/if}
          </span>
        {/if}
      </button>
      {#if state?.state === "failed" && !imageReady}
        <div class="media-err">{state.error}</div>
      {/if}
    {:else if msg.media === "video"}
      <div class="pic" style:width="{box.w}px" style:height="{box.h}px">
        {#if wantVideo && state?.state === "ready"}
          <!-- svelte-ignore a11y_media_has_caption -->
          <video src={fileUrl(msg.rowid)} controls autoplay width={box.w} height={box.h} poster={msg.has_thumb ? thumbUrl(msg.rowid) : undefined}></video>
        {:else}
          {#if msg.has_thumb}<img src={thumbUrl(msg.rowid)} alt="" class="thumb" />{/if}
          <button
            class="play"
            aria-label="Play video"
            onclick={() => {
              wantVideo = true;
              fetchThen(() => {});
            }}
          >
            {#if wantVideo && state?.state !== "failed"}
              <span>{progress !== null ? `${progress}%` : "Loading…"}</span>
            {:else}
              <Icon name="play" size={24} />
            {/if}
          </button>
          <span class="meta-chip">{duration(msg.seconds)} {fileSize(msg.size)}</span>
        {/if}
      </div>
      {#if state?.state === "failed"}
        <div class="media-err">{state.error}</div>
      {/if}
    {:else if msg.media === "audio"}
      {#if wantAudio && state?.state === "ready"}
        <audio src={fileUrl(msg.rowid)} controls autoplay></audio>
      {:else}
        <button
          class="doc"
          onclick={() => {
            wantAudio = true;
            fetchThen(() => {});
          }}
        >
          <span class="doc-icon"><Icon name="play" size={18} /></span>
          <span class="doc-main">
            <span class="doc-name">Audio {duration(msg.seconds)}</span>
            <span class="doc-sub">
              {#if wantAudio && state?.state !== "failed"}{progress !== null ? `${progress}%` : "Loading…"}{:else}{fileSize(msg.size)}{/if}
            </span>
          </span>
        </button>
      {/if}
      {#if state?.state === "failed"}
        <div class="media-err">{state.error}</div>
      {/if}
    {:else if msg.media === "document"}
      <div class="doc">
        <span class="doc-icon"><Icon name="file" size={18} /></span>
        <span class="doc-main">
          {#if canPreview}
            <button class="doc-name as-link" title="Preview" onclick={() => (previewing = true)}>{msg.filename || "File"}</button>
          {:else}
            <span class="doc-name">{msg.filename || "File"}</span>
          {/if}
          <span class="doc-sub">
            {#if state?.state === "preparing" || state?.state === "downloading"}
              {progress !== null ? `${progress}%` : "Preparing…"}
            {:else}
              {fileSize(msg.size)}
            {/if}
          </span>
        </span>
        {#if canPreview}
          <button class="icon-btn" title="Preview" aria-label="Preview file" onclick={() => (previewing = true)}><Icon name="search" size={17} /></button>
        {/if}
        <button class="icon-btn" title="Save" aria-label="Save file" onclick={save}><Icon name="download" size={17} /></button>
        <button class="icon-btn" title="Open" aria-label="Open file" onclick={openFile}><Icon name="open" size={17} /></button>
      </div>
      {#if state?.state === "failed"}
        <div class="media-err">{state.error}</div>
      {/if}
    {/if}

    {#if msg.text && !msg.revoked}
      <div class="text">
        {#each parts as p}{#if p.url}<a href={p.url} onclick={(e) => link(e, p.url!)} onauxclick={(e) => e.preventDefault()}>{p.text}</a>{:else if p.mention}<span class="mention">{p.text}</span>{:else if p.bold}<b>{p.text}</b>{:else if p.italic}<i>{p.text}</i>{:else if p.strike}<s>{p.text}</s>{:else if p.code}<code>{p.text}</code>{:else}{p.text}{/if}{/each}
        <span class="spacer" class:edited={msg.edited}></span>
      </div>
    {/if}

    <div class="stamp">
      {#if msg.edited}<span>edited</span>{/if}
      <time>{clock(msg.ts)}</time>
      {#if msg.from_me && !msg.revoked}
        <span class="tick" class:read={msg.status === "read" || msg.status === "played"} title={msg.status || "sent"}>
          <Icon name={msg.status === "delivered" || msg.status === "read" || msg.status === "played" ? "checks" : "check"} size={14} />
        </span>
      {/if}
    </div>

    {#if reactionGroups.length}
      <div class="reactions">
        {#each reactionGroups as [emoji, count]}
          <span>{emoji}{#if count > 1}<small>{count}</small>{/if}</span>
        {/each}
      </div>
    {/if}
  </div>

  <div class="tools" class:open={picker || !!menu}>
    {#if !msg.revoked}
      <button class="icon-btn" title="Reply" aria-label="Reply" onclick={() => onreply(msg)}><Icon name="reply" size={16} /></button>
      <button class="icon-btn" title="React" aria-label="React" bind:this={smile} onclick={() => (picker = !picker)}><Icon name="smile" size={16} /></button>
      {#if msg.media && msg.media !== "document"}
        <button class="icon-btn" title="Save" aria-label="Save file" onclick={save}><Icon name="download" size={16} /></button>
      {/if}
    {/if}
    <button class="icon-btn more" title="More" aria-label="More options for this message" aria-haspopup="menu" onclick={(e) => openMenuFrom(e.currentTarget)}>
      <span class="dots" aria-hidden="true"><i></i><i></i><i></i></span>
    </button>
  </div>
</div>

{#if picker && !msg.revoked && smile}
  <ReactionPicker anchor={smile} alignRight={msg.from_me} current={msg.reactions?.me ?? ""} onpick={react} onclose={() => (picker = false)} />
{/if}

{#if previewing}
  <DocPreview {msg} onclose={() => (previewing = false)} />
{/if}

{#if menu}
  <MessageMenu x={menu.x} y={menu.y} items={menu.items} label="Message options" onclose={() => (menu = null)} />
{/if}

<style>
  .as-link {
    text-align: left;
    color: inherit;
    padding: 0;
    cursor: pointer;
  }
  .as-link:hover {
    text-decoration: underline;
  }
  .dots {
    display: inline-flex;
    flex-direction: column;
    gap: 2.5px;
  }
  .dots i {
    width: 3px;
    height: 3px;
    border-radius: 50%;
    background: currentColor;
  }
  .row {
    --who: var(--who-light);
    --who-ink: #fff;
    display: flex;
    align-items: center;
    gap: 4px;
    padding: 1.5px 0;
    flex: none;
    /* Off-screen rows skip layout and paint, which keeps long chats smooth. */
    content-visibility: auto;
    contain-intrinsic-size: auto 52px;
    border-radius: 6px;
    transition: background 0.4s;
  }
  @media (prefers-color-scheme: dark) {
    .row {
      --who: var(--who-dark);
      --who-ink: #101316;
    }
  }
  .row.mine {
    flex-direction: row-reverse;
  }
  /* A different person is talking. */
  .row.gap {
    margin-top: 8px;
  }
  /* The same width whether or not there's a picture, so a person's messages line up under it. */
  .face {
    flex: none;
    width: 28px;
    margin-right: 2px;
    align-self: flex-start;
    user-select: none;
  }
  .row.hl {
    background: var(--active);
    content-visibility: visible;
  }
  .bubble {
    max-width: min(72%, 600px);
    background: var(--theirs);
    border-radius: 10px;
    padding: 5px 9px 4px;
    position: relative;
    box-shadow: 0 1px 1px rgb(0 0 0 / 0.07);
    min-width: 76px;
  }
  .mine .bubble {
    background: var(--mine);
  }
  .bubble.bare {
    padding: 3px 3px 4px;
  }
  .bubble.bare .text,
  .bubble.bare .stamp,
  .bubble.bare .sender,
  .bubble.bare .quote {
    margin-left: 6px;
    margin-right: 6px;
  }
  /* The person's colour runs down every message, since the name only shows on the first of a run. */
  .bubble.coded {
    box-shadow:
      inset 3px 0 0 var(--who),
      0 1px 1px rgb(0 0 0 / 0.07);
  }
  .bubble.coded:not(.bare) {
    padding-left: 11px;
  }
  .bubble.sticker {
    background: none;
    box-shadow: none;
  }
  .sender {
    font-weight: 600;
    font-size: 12.5px;
    margin-bottom: 1px;
    color: var(--muted);
  }
  .coded .sender {
    color: var(--who);
  }
  /* A sticker has no bubble, and the colours are only checked against one. */
  .sticker .sender {
    width: fit-content;
    padding: 0 6px;
    border-radius: 6px;
    background: var(--theirs);
  }
  .text {
    white-space: pre-wrap;
    overflow-wrap: anywhere;
  }
  .mention {
    color: var(--accent);
    font-weight: 600;
  }
  .text a {
    color: var(--accent);
  }
  .text code {
    background: rgb(127 127 127 / 0.17);
    padding: 0 4px;
    border-radius: 4px;
    font-family: Consolas, monospace;
    font-size: 13px;
  }
  .spacer {
    display: inline-block;
    width: 72px;
    height: 1px;
  }
  /* Room for "edited" beside the time, so it never sits on the last words. */
  .spacer.edited {
    width: 116px;
  }
  .stamp {
    float: right;
    margin: -14px 0 0 8px;
    display: flex;
    align-items: center;
    gap: 4px;
    font-size: 11px;
    color: var(--faint);
    user-select: none;
    position: relative;
  }
  .bare .stamp,
  .doc + .stamp {
    float: none;
    margin-top: 2px;
    justify-content: flex-end;
  }
  .tick {
    display: inline-flex;
    color: var(--faint);
  }
  .tick.read {
    color: #2b8fd6;
  }
  .gone {
    color: var(--muted);
    font-style: italic;
    display: inline-flex;
    align-items: center;
    gap: 5px;
    padding-right: 62px;
  }
  .quote {
    display: flex;
    flex-direction: column;
    text-align: left;
    width: calc(100% + 4px);
    margin: 1px -2px 4px;
    padding: 4px 8px;
    border-left: 3px solid var(--accent);
    background: rgb(127 127 127 / 0.11);
    border-radius: 5px;
    font-size: 12.5px;
    color: var(--muted);
  }
  .quote b {
    color: var(--accent);
    font-weight: 600;
  }
  .quote span {
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
    max-width: 480px;
  }
  .pic {
    position: relative;
    display: block;
    border-radius: 8px;
    overflow: hidden;
    background: rgb(127 127 127 / 0.2);
    padding: 0;
    max-width: 100%;
  }
  .pic.stick {
    background: none;
  }
  .pic img,
  .pic video {
    position: absolute;
    inset: 0;
    width: 100%;
    height: 100%;
    object-fit: cover;
    display: block;
  }
  .pic.stick img {
    object-fit: contain;
  }
  .thumb {
    filter: blur(6px);
    transform: scale(1.06);
  }
  .full {
    opacity: 0;
    transition: opacity 0.12s;
  }
  .full.ready {
    opacity: 1;
  }
  .pic-state {
    position: absolute;
    left: 50%;
    top: 50%;
    transform: translate(-50%, -50%);
    background: rgb(0 0 0 / 0.55);
    color: #fff;
    padding: 4px 10px;
    border-radius: 14px;
    font-size: 12.5px;
    display: inline-flex;
    gap: 5px;
    align-items: center;
    white-space: nowrap;
  }
  .pic-state:empty {
    display: none;
  }
  .play {
    position: absolute;
    left: 50%;
    top: 50%;
    transform: translate(-50%, -50%);
    width: 54px;
    height: 54px;
    border-radius: 50%;
    background: rgb(0 0 0 / 0.55);
    color: #fff;
    display: grid;
    place-items: center;
    font-size: 12.5px;
  }
  .meta-chip {
    position: absolute;
    left: 6px;
    bottom: 6px;
    background: rgb(0 0 0 / 0.55);
    color: #fff;
    font-size: 11px;
    padding: 1px 7px;
    border-radius: 9px;
  }
  .media-err {
    color: var(--danger);
    font-size: 12.5px;
    margin: 4px 6px 0;
  }
  .doc {
    display: flex;
    align-items: center;
    gap: 9px;
    background: rgb(127 127 127 / 0.1);
    padding: 7px 8px;
    border-radius: 8px;
    margin-bottom: 2px;
    text-align: left;
    width: 100%;
    min-width: 230px;
  }
  .doc-icon {
    width: 36px;
    height: 36px;
    border-radius: 8px;
    background: var(--accent);
    color: var(--accent-text);
    display: grid;
    place-items: center;
    flex: none;
  }
  .doc-main {
    flex: 1;
    min-width: 0;
    display: flex;
    flex-direction: column;
  }
  .doc-name {
    font-weight: 600;
    overflow-wrap: anywhere;
  }
  .doc-sub {
    font-size: 12px;
    color: var(--muted);
  }
  audio {
    width: 280px;
    max-width: 100%;
    height: 40px;
    display: block;
  }
  .reactions {
    position: absolute;
    bottom: -13px;
    left: 8px;
    display: flex;
    gap: 2px;
    background: var(--panel);
    border-radius: 11px;
    padding: 0 6px;
    font-size: 13px;
    box-shadow: 0 1px 3px rgb(0 0 0 / 0.22);
    user-select: none;
    z-index: 1;
  }
  .reactions small {
    font-size: 10.5px;
    margin-left: 1px;
    color: var(--muted);
  }
  .row:has(.reactions) {
    padding-bottom: 15px;
  }
  .tools {
    display: flex;
    opacity: 0;
    position: relative;
    user-select: none;
  }
  .row:hover .tools,
  .tools.open,
  .tools:focus-within {
    opacity: 1;
  }
  .tools .icon-btn {
    width: 28px;
    height: 28px;
  }
</style>
