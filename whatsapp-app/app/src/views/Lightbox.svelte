<script lang="ts">
  // A photo filling the window, with the rest of its chat's photos a key away: Left is older, Right is newer,
  // Home and End are the ends. The next one is asked for from the local store, not the rows the conversation
  // has loaded, so it reaches every photo this computer has.
  import { onDestroy, onMount, untrack } from "svelte";
  import { api, errorText, fileUrl, thumbUrl, type Message, type Photo, type PhotoStep } from "../lib/api";
  import { clock, dayLabel, segments } from "../lib/format";
  import { media, on } from "../lib/state.svelte";
  import Icon from "../lib/Icon.svelte";

  let { msg, name = "", onclose }: { msg: Message; name?: string; onclose: () => void } = $props();

  // A key held down passes photos faster than this, and only the one it stops on is loaded.
  const SETTLE_MS = 140;

  let el: HTMLDivElement | undefined = $state();
  // Kept here rather than taken from the conversation, so a new message can't change what's on show.
  let shown = $state.raw<Message>(untrack(() => msg));
  let older = $state.raw<Photo | null>(null);
  let newer = $state.raw<Photo | null>(null);
  let phase = $state<"waiting" | "fetching" | "ready" | "failed" | "deleted">("ready");
  let loaded = $state(false);
  let error = $state("");
  let attempt = $state(0);

  let closed = false;
  let landedAt = -Infinity;
  let timer: ReturnType<typeof setTimeout> | undefined;
  let preloaded: HTMLImageElement[] = [];

  const caption = $derived(shown.text ? segments(shown.text, shown.mentions).map((s) => s.text).join("") : "");
  const who = $derived(shown.from_me ? "You" : name || shown.sender_name);
  const progress = $derived.by(() => {
    const s = media[shown.rowid];
    return s?.state === "downloading" && s.total ? Math.round(((s.done ?? 0) / s.total) * 100) : null;
  });

  // Steps are taken in the order they were pressed. A held key only adds one once the last has been taken, so
  // letting go stops there.
  let turns: PhotoStep[] = [];
  let walking = false;

  function go(turn: PhotoStep, repeat = false) {
    if (repeat && walking) return;
    if (turn === "oldest" || turn === "newest") turns = [];
    turns.push(turn);
    void walk();
  }

  async function walk() {
    if (walking) return;
    walking = true;
    try {
      while (turns.length && !closed) {
        const turn = turns[0];
        const spot = await api.photoSpot(shown.chat, turn, shown.ts, shown.rowid);
        if (closed) return;
        // Home or End, pressed meanwhile, replaced what was waiting.
        if (turns[0] !== turn) continue;
        turns.shift();
        older = spot.older;
        newer = spot.newer;
        if (spot.photo && spot.photo.msg.rowid !== shown.rowid) land(spot.photo);
      }
    } catch {
      turns = [];
    } finally {
      walking = false;
    }
  }

  function land(photo: Photo) {
    const now = performance.now();
    const passing = now - landedAt < SETTLE_MS;
    landedAt = now;
    clearTimeout(timer);
    shown = photo.msg;
    loaded = false;
    error = "";
    if (photo.cached && !passing) {
      phase = "ready";
    } else {
      phase = "waiting";
      timer = setTimeout(() => (photo.cached ? (phase = "ready") : void fetchFile()), SETTLE_MS);
    }
  }

  async function fetchFile() {
    const { rowid } = shown;
    phase = "fetching";
    try {
      await api.mediaFetch(rowid);
      if (shown.rowid === rowid && phase === "fetching") phase = "ready";
    } catch (e) {
      if (shown.rowid === rowid && phase === "fetching") {
        phase = "failed";
        error = errorText(e);
      }
    }
  }

  function retry() {
    attempt++;
    loaded = false;
    el?.focus({ preventScroll: true });
    void fetchFile();
  }

  const isShown = (e: Event) => Number((e.currentTarget as HTMLElement).dataset.rowid) === shown.rowid;

  function imageLoaded(e: Event) {
    if (!isShown(e)) return;
    loaded = true;
    preload();
  }

  function imageFailed(e: Event) {
    if (!isShown(e) || phase !== "ready") return;
    const s = media[shown.rowid];
    phase = "failed";
    error = (s?.state === "failed" && s.error) || "The file may be damaged, or a kind that can't be shown here.";
  }

  /** The photos either side, if they're already on this computer. Nothing is downloaded for this. */
  function preload() {
    preloaded = [older, newer]
      .filter((p) => p?.cached)
      .map((p) => {
        const img = new Image();
        img.src = fileUrl(p!.msg.rowid);
        return img;
      });
  }

  /** What's either side now, and whether this photo is still here. Never moves to another one. */
  async function refresh() {
    const at = shown;
    const spot = await api.photoSpot(at.chat, null, at.ts, at.rowid).catch(() => null);
    if (!spot || closed || shown.rowid !== at.rowid) return;
    older = spot.older;
    newer = spot.newer;
    if (spot.photo) {
      shown = spot.photo.msg;
      if (loaded) preload();
    } else {
      clearTimeout(timer);
      phase = "deleted";
    }
  }

  /** Escape still held once this has closed would go on to cancel a reply or an edit behind it. */
  function dropHeldEscape() {
    const done = () => {
      window.removeEventListener("keydown", held, true);
      window.removeEventListener("keyup", done, true);
      window.removeEventListener("blur", done);
    };
    const held = (e: KeyboardEvent) => {
      if (e.key !== "Escape" || !e.repeat) return done();
      e.preventDefault();
      e.stopPropagation();
    };
    window.addEventListener("keydown", held, true);
    window.addEventListener("keyup", done, true);
    window.addEventListener("blur", done);
  }

  const STEPS: Record<string, PhotoStep> = { ArrowLeft: "older", ArrowRight: "newer", Home: "oldest", End: "newest" };

  // Every key stops here, so nothing behind moves, scrolls or opens.
  function onKey(e: KeyboardEvent) {
    e.stopPropagation();
    const target = e.target instanceof Element ? e.target : null;
    if (!target || !el?.contains(target)) {
      e.preventDefault();
      el?.focus({ preventScroll: true });
    }
    const plain = !e.ctrlKey && !e.altKey && !e.metaKey && !e.shiftKey;
    if (e.key === "Escape") {
      e.preventDefault();
      dropHeldEscape();
      onclose();
    } else if (e.key === "Tab") {
      e.preventDefault();
      const stops = [...(el?.querySelectorAll<HTMLElement>("button") ?? [])];
      const at = stops.indexOf(document.activeElement as HTMLElement);
      const next = e.shiftKey ? (at <= 0 ? stops.length - 1 : at - 1) : (at + 1) % stops.length;
      stops[next]?.focus();
    } else if (STEPS[e.key]) {
      e.preventDefault();
      if (plain) go(STEPS[e.key], e.repeat);
    } else if (["ArrowUp", "ArrowDown", "PageUp", "PageDown"].includes(e.key)) {
      e.preventDefault();
    } else if ((e.key === " " || e.key === "Enter") && !target?.closest("button")) {
      e.preventDefault();
    } else if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === "k") {
      e.preventDefault();
    }
  }
  const stop = (e: KeyboardEvent) => e.stopPropagation();

  const before = document.activeElement as HTMLElement | null;
  let root: HTMLElement | undefined;
  onMount(() => {
    root = el;
    root?.focus({ preventScroll: true });
    window.addEventListener("keydown", onKey, true);
    window.addEventListener("keypress", stop, true);
    window.addEventListener("keyup", stop, true);
    void refresh();
  });
  const unsubscribers = [
    on("chats-changed", (changed: string[]) => {
      if (changed.length === 0 || changed.includes(shown.chat)) void refresh();
    }),
    on("contacts-changed", () => void refresh()),
  ];
  onDestroy(() => {
    closed = true;
    clearTimeout(timer);
    preloaded = [];
    unsubscribers.forEach((off) => off());
    window.removeEventListener("keydown", onKey, true);
    window.removeEventListener("keypress", stop, true);
    window.removeEventListener("keyup", stop, true);
    // Back to where it was, unless something else has taken it since.
    const now = document.activeElement;
    if (before?.isConnected && (!now || now === document.body || root?.contains(now))) before.focus({ preventScroll: true });
  });
</script>

<!-- svelte-ignore a11y_click_events_have_key_events -->
<div
  class="back"
  role="dialog"
  aria-modal="true"
  aria-label="Image"
  tabindex="-1"
  bind:this={el}
  data-rowid={shown.rowid}
  onclick={(e) => e.target === e.currentTarget && onclose()}
>
  <div class="about" aria-live="polite">
    {#if who}<b>{who}</b>{/if}
    <span>{dayLabel(shown.ts)}, {clock(shown.ts)}</span>
  </div>
  <button class="round close" onclick={onclose} aria-label="Close" title="Close (Esc)"><Icon name="close" size={22} /></button>

  <!-- svelte-ignore a11y_no_static_element_interactions -->
  <div class="stage" onclick={(e) => e.target === e.currentTarget && onclose()}>
    {#if phase === "deleted"}
      <div class="note" role="status">This photo was deleted.</div>
    {:else}
      {#key `${shown.rowid}:${attempt}`}
        {#if shown.has_thumb && !loaded}
          <img class="thumb" class:dim={phase === "failed"} src={thumbUrl(shown.rowid)} alt="" width={shown.width || 320} height={shown.height || undefined} draggable="false" />
        {/if}
        {#if phase === "ready"}
          <img class="full" class:loaded src={fileUrl(shown.rowid)} alt={caption || "Photo"} data-rowid={shown.rowid} draggable="false" onload={imageLoaded} onerror={imageFailed} />
        {/if}
      {/key}
      {#if phase === "failed"}
        <div class="note" role="alert">
          <span>Couldn't show this photo. {error}</span>
          <button onclick={retry}>Try again</button>
        </div>
      {:else if !loaded && (phase === "fetching" || !shown.has_thumb)}
        <div class="note small" role="status">{progress !== null ? `${progress}%` : "Loading…"}</div>
      {/if}
    {/if}
  </div>

  <!-- A click here leaves the keyboard where it is. At an end the button stays put but does nothing. -->
  <button class="round prev" class:off={!older} aria-disabled={!older} onmousedown={(e) => e.preventDefault()} onclick={() => older && go("older")} aria-label="Older photo" title="Older photo (Left)">
    <Icon name="back" size={26} />
  </button>
  <button class="round next" class:off={!newer} aria-disabled={!newer} onmousedown={(e) => e.preventDefault()} onclick={() => newer && go("newer")} aria-label="Newer photo" title="Newer photo (Right)">
    <Icon name="back" size={26} />
  </button>

  {#if caption && phase !== "deleted"}<p class="selectable">{caption}</p>{/if}
</div>

<style>
  .back {
    position: fixed;
    inset: 0;
    background: rgb(0 0 0 / 0.86);
    display: flex;
    flex-direction: column;
    align-items: center;
    z-index: 50;
    padding: 60px 68px 20px;
    gap: 10px;
    outline: none;
  }
  .about {
    position: absolute;
    top: 16px;
    left: 20px;
    right: 70px;
    color: #ddd;
    display: flex;
    flex-wrap: wrap;
    gap: 0 10px;
    font-size: 13px;
  }
  .about b {
    color: #fff;
    font-weight: 600;
    max-width: 60%;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .stage {
    flex: 1;
    min-height: 0;
    width: 100%;
    display: grid;
    grid-template: minmax(0, 1fr) / minmax(0, 1fr);
    place-items: center;
  }
  .stage > :global(*) {
    grid-area: 1 / 1;
  }
  img {
    max-width: 100%;
    max-height: 100%;
    object-fit: contain;
  }
  .thumb {
    filter: blur(6px);
  }
  .thumb.dim {
    opacity: 0.35;
  }
  .full {
    opacity: 0;
  }
  .full.loaded {
    opacity: 1;
  }
  .note {
    position: relative;
    background: rgb(0 0 0 / 0.7);
    color: #fff;
    padding: 10px 16px;
    border-radius: 10px;
    max-width: min(90%, 460px);
    text-align: center;
  }
  .note.small {
    padding: 5px 12px;
    font-size: 13px;
  }
  .note button {
    color: #fff;
    text-decoration: underline;
    font-weight: 600;
    margin-left: 6px;
  }
  p {
    color: #eee;
    margin: 0;
    max-width: 780px;
    max-height: 22vh;
    overflow-y: auto;
    text-align: center;
    white-space: pre-wrap;
    overflow-wrap: anywhere;
  }
  .round {
    position: absolute;
    color: #fff;
    width: 40px;
    height: 40px;
    border-radius: 50%;
    display: grid;
    place-items: center;
    background: rgb(255 255 255 / 0.12);
  }
  .round:hover {
    background: rgb(255 255 255 / 0.24);
  }
  .close {
    top: 12px;
    right: 12px;
  }
  .prev,
  .next {
    top: calc(50% - 22px);
    width: 44px;
    height: 44px;
  }
  .prev {
    left: 12px;
  }
  .next {
    right: 12px;
  }
  .next :global(svg) {
    transform: rotate(180deg);
  }
  .off,
  .off:hover {
    opacity: 0.2;
    cursor: default;
    background: rgb(255 255 255 / 0.12);
  }
</style>
