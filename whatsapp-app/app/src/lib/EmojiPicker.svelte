<script lang="ts">
  // Every emoji, as WhatsApp offers them: recently used first, then Unicode's categories, a search by name or
  // everyday word, and a skin tone that applies to every emoji that takes one. Drawn fixed to the window beside
  // the button it opened from. An emoji Windows can't draw yet still shows and still sends: a country flag as
  // its two letters, anything else in a dotted frame, named in its tooltip.
  import { onDestroy, onMount, tick } from "svelte";
  import {
    canDraw,
    EMOJI_FONT,
    flagLetters,
    loadEmoji,
    noteUsed,
    recentEmoji,
    saveTone,
    savedTone,
    searchEmoji,
    TONE_NAMES,
    TONES,
    withTone,
    type Emoji,
    type EmojiGroup,
  } from "./emoji";

  let {
    anchor,
    alignRight = false,
    current = "",
    label = "Emoji",
    listen = true,
    onpick,
    onclose,
  }: {
    anchor: HTMLElement;
    alignRight?: boolean;
    /** Shown as chosen, such as the user's own reaction. */
    current?: string;
    label?: string;
    /** Whether it closes itself on Escape, a click outside, a resize or the window losing focus. */
    listen?: boolean;
    onpick: (emoji: string) => void;
    onclose: () => void;
  } = $props();

  const ICON: Record<string, string> = {
    "Smileys & Emotion": "😀",
    "People & Body": "👋",
    "Animals & Nature": "🐻",
    "Food & Drink": "🍔",
    "Travel & Places": "🚗",
    Activities: "⚽",
    Objects: "💡",
    Symbols: "🔣",
    Flags: "🏁",
  };

  let groups = $state<EmojiGroup[]>([]);
  let query = $state("");
  let tone = $state(savedTone());
  let choosingTone = $state(false);
  let recent = $state(recentEmoji());
  let el: HTMLDivElement | undefined = $state();
  let grid: HTMLDivElement | undefined = $state();
  let search: HTMLInputElement | undefined = $state();
  let left = $state(-9999);
  let top = $state(-9999);
  let showing = $state("Recent");

  const byChar = $derived(new Map(groups.flatMap((g) => g.emoji.map((e) => [e.char, e] as const))));
  const found = $derived(query.trim() ? searchEmoji(groups, query) : []);

  /** The name of an emoji as used, with any tone taken off to find it. */
  function nameOf(char: string): string {
    const plain = char.replace(/[\u{1F3FB}-\u{1F3FF}]/gu, "");
    return byChar.get(char)?.name ?? byChar.get(plain)?.name ?? byChar.get(plain + "️")?.name ?? char;
  }

  // Older emoji are all in Windows' font; only newer ones and flags are worth checking.
  const needsCheck = (e: Emoji | undefined) => !e || e.group === "Flags" || Number(e.version) >= 13;
  function drawn(char: string, e?: Emoji): boolean {
    return needsCheck(e) ? canDraw(char) : true;
  }

  function pick(char: string) {
    noteUsed(char);
    recent = recentEmoji();
    onpick(char);
  }

  function setTone(t: number) {
    tone = t;
    saveTone(t);
    choosingTone = false;
  }

  function jump(name: string) {
    query = "";
    showing = name;
    void tick().then(() => {
      const section = grid?.querySelector<HTMLElement>(`[data-group="${CSS.escape(name)}"]`);
      if (grid && section) grid.scrollTop = section.offsetTop - grid.offsetTop;
    });
  }

  // Which category is in view, for the tabs.
  function onGridScroll() {
    if (!grid || query) return;
    let seen = "Recent";
    for (const s of grid.querySelectorAll<HTMLElement>("[data-group]")) {
      if (s.offsetTop - grid.offsetTop <= grid.scrollTop + 8) seen = s.dataset.group ?? seen;
    }
    showing = seen;
  }

  function place() {
    const a = anchor.getBoundingClientRect();
    const box = el?.getBoundingClientRect();
    const w = box?.width ?? 360;
    const h = box?.height ?? 420;
    const above = a.top - h - 6 >= 8;
    top = above ? a.top - h - 6 : Math.max(8, Math.min(a.bottom + 6, window.innerHeight - h - 8));
    const want = alignRight ? a.right - w : a.left;
    left = Math.max(8, Math.min(want, window.innerWidth - w - 8));
  }

  function outside(e: PointerEvent) {
    if (el && !el.contains(e.target as Node) && !anchor.contains(e.target as Node)) onclose();
  }
  function onKey(e: KeyboardEvent) {
    if (e.key !== "Escape") return;
    e.preventDefault();
    e.stopPropagation();
    if (choosingTone) choosingTone = false;
    else if (query) query = "";
    else onclose();
  }
  const onBlur = () => onclose();

  onMount(async () => {
    groups = await loadEmoji();
    await tick();
    place();
    search?.focus({ preventScroll: true });
    if (listen) {
      window.addEventListener("pointerdown", outside, true);
      window.addEventListener("keydown", onKey, true);
      window.addEventListener("resize", onBlur);
      window.addEventListener("blur", onBlur);
    }
  });
  onDestroy(() => {
    window.removeEventListener("pointerdown", outside, true);
    window.removeEventListener("keydown", onKey, true);
    window.removeEventListener("resize", onBlur);
    window.removeEventListener("blur", onBlur);
  });
</script>

{#snippet cell(char: string, name: string, e?: Emoji)}
  {@const ok = drawn(char, e)}
  {@const letters = ok ? null : flagLetters(char)}
  <button
    type="button"
    class="cell"
    class:on={current === char}
    class:missing={!ok && !letters}
    title={ok ? name : `${name}. Windows can't draw this one yet; it sends normally and shows on phones.`}
    aria-label={name}
    data-emoji={char}
    onclick={() => pick(char)}
  >
    {#if letters}<span class="flag">{letters}</span>{:else}<span class="glyph">{char}</span>{/if}
  </button>
{/snippet}

<div class="emoji-picker" role="dialog" aria-label={label} bind:this={el} style:left="{left}px" style:top="{top}px" style:--emoji-font={EMOJI_FONT}>
  <div class="top">
    <input class="input" bind:this={search} bind:value={query} placeholder="Search emoji" aria-label="Search emoji" />
    <div class="tone-wrap">
      <button
        type="button"
        class="tone-btn"
        title="Skin tone: {TONE_NAMES[tone]}"
        aria-label="Skin tone: {TONE_NAMES[tone]}"
        aria-expanded={choosingTone}
        onclick={() => (choosingTone = !choosingTone)}
      >
        <span class="glyph">{"✋" + TONES[tone]}</span>
      </button>
      {#if choosingTone}
        <div class="tones" role="menu" aria-label="Skin tones">
          {#each TONES as mod, t}
            <button type="button" role="menuitemradio" aria-checked={tone === t} aria-label={TONE_NAMES[t]} title={TONE_NAMES[t]} onclick={() => setTone(t)}>
              <span class="glyph">{"✋" + mod}</span>
            </button>
          {/each}
        </div>
      {/if}
    </div>
  </div>

  {#if !query}
    <div class="tabs" role="tablist" aria-label="Emoji categories">
      <button type="button" role="tab" aria-selected={showing === "Recent"} title="Recently used" aria-label="Recently used" onclick={() => jump("Recent")}>
        <span class="glyph">🕘</span>
      </button>
      {#each groups as g (g.name)}
        <button type="button" role="tab" aria-selected={showing === g.name} title={g.name} aria-label={g.name} onclick={() => jump(g.name)}>
          <span class="glyph">{ICON[g.name] ?? g.emoji[0]?.char}</span>
        </button>
      {/each}
    </div>
  {/if}

  <div class="grid" bind:this={grid} onscroll={onGridScroll}>
    {#if !groups.length}
      <p class="note">Loading…</p>
    {:else if query}
      <section aria-label="Results">
        {#if found.length}
          <div class="cells">
            {#each found as e (e.char)}{@render cell(withTone(e, tone), e.name, e)}{/each}
          </div>
        {:else}
          <p class="note">No emoji match “{query.trim()}”.</p>
        {/if}
      </section>
    {:else}
      <section data-group="Recent" aria-label="Recently used">
        <h4>Recently used</h4>
        {#if recent.length}
          <div class="cells">
            {#each recent as char (char)}{@render cell(char, nameOf(char))}{/each}
          </div>
        {:else}
          <p class="note">Emoji you use show here.</p>
        {/if}
      </section>
      {#each groups as g (g.name)}
        <section data-group={g.name} aria-label={g.name}>
          <h4>{g.name}</h4>
          <div class="cells">
            {#each g.emoji as e (e.char)}{@render cell(withTone(e, tone), e.name, e)}{/each}
          </div>
        </section>
      {/each}
    {/if}
  </div>
</div>

<style>
  .emoji-picker {
    position: fixed;
    z-index: 65;
    width: min(372px, calc(100vw - 16px));
    height: min(430px, calc(100vh - 16px));
    display: flex;
    flex-direction: column;
    background: var(--panel);
    color: var(--text);
    border: 1px solid var(--line);
    border-radius: 12px;
    box-shadow: var(--shadow);
    padding: 8px;
    gap: 6px;
  }
  .glyph,
  .flag {
    font-family: var(--emoji-font);
  }
  .top {
    display: flex;
    gap: 6px;
    align-items: center;
  }
  .top .input {
    flex: 1;
    min-width: 0;
  }
  .tone-wrap {
    position: relative;
  }
  .tone-btn,
  .tones button {
    width: 34px;
    height: 34px;
    border-radius: 8px;
    font-size: 20px;
  }
  .tone-btn:hover,
  .tones button:hover,
  .tones button[aria-checked="true"] {
    background: var(--active);
  }
  .tones {
    position: absolute;
    right: 0;
    top: 38px;
    z-index: 2;
    display: flex;
    background: var(--panel);
    border: 1px solid var(--line);
    border-radius: 10px;
    box-shadow: var(--shadow);
    padding: 3px;
  }
  .tabs {
    display: flex;
    justify-content: space-between;
    border-bottom: 1px solid var(--line);
    padding-bottom: 4px;
  }
  .tabs button {
    width: 34px;
    height: 30px;
    border-radius: 7px;
    font-size: 18px;
    opacity: 0.6;
  }
  .tabs button[aria-selected="true"] {
    opacity: 1;
    background: var(--active);
  }
  .tabs button:hover {
    opacity: 1;
  }
  .grid {
    flex: 1;
    overflow-y: auto;
    min-height: 0;
  }
  section {
    content-visibility: auto;
    contain-intrinsic-size: auto 300px;
  }
  h4 {
    position: sticky;
    top: 0;
    margin: 0;
    padding: 4px 2px;
    background: var(--panel);
    font-size: 12px;
    font-weight: 700;
    color: var(--muted);
    z-index: 1;
  }
  .cells {
    display: grid;
    grid-template-columns: repeat(auto-fill, minmax(38px, 1fr));
  }
  .cell {
    height: 38px;
    border-radius: 8px;
    font-size: 24px;
    line-height: 1;
    display: grid;
    place-items: center;
  }
  .cell:hover,
  .cell:focus-visible,
  .cell.on {
    background: var(--active);
  }
  .cell.missing .glyph {
    font-size: 15px;
    width: 28px;
    height: 28px;
    display: grid;
    place-items: center;
    border: 1.5px dotted var(--muted);
    border-radius: 7px;
    overflow: hidden;
  }
  .flag {
    font-size: 11px;
    font-weight: 700;
    letter-spacing: 0.5px;
    padding: 3px 4px;
    border-radius: 4px;
    background: var(--hover);
    color: var(--text);
    border: 1px solid var(--line);
  }
  .note {
    color: var(--muted);
    font-size: 12.5px;
    margin: 6px 2px;
  }
</style>
