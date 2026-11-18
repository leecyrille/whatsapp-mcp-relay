<script lang="ts">
  // Who reacted to a message, as WhatsApp shows it: everyone with their picture, name and emoji, the user
  // first with a way to take theirs back, and a tab per emoji. Drawn fixed to the window like the reaction
  // picker, since message rows clip anything drawn outside them.
  import { onDestroy, onMount, tick } from "svelte";
  import type { Reactor } from "../lib/api";
  import Avatar from "../lib/Avatar.svelte";
  import { emojiPieces } from "../lib/emoji";
  import { ui } from "../lib/state.svelte";

  let {
    anchor,
    alignRight = false,
    reactors,
    onremove,
    onclose,
  }: { anchor: HTMLElement; alignRight?: boolean; reactors: Reactor[]; onremove: () => void; onclose: () => void } = $props();

  let el: HTMLDivElement | undefined = $state();
  let left = $state(-9999);
  let top = $state(-9999);
  let tab = $state("");

  const groups = $derived.by(() => {
    const counts = new Map<string, number>();
    for (const r of reactors) counts.set(r.emoji, (counts.get(r.emoji) ?? 0) + 1);
    return [...counts.entries()].sort((a, b) => b[1] - a[1]);
  });
  const shown = $derived(tab ? reactors.filter((r) => r.emoji === tab) : reactors);

  // A double-click on someone's picture opens their card, which takes over from this list.
  $effect(() => {
    if (ui.card) onclose();
  });

  // Nothing left in a tab once its last reaction goes, so it falls back to everyone.
  $effect(() => {
    if (tab && !groups.some(([e]) => e === tab)) tab = "";
  });

  function place() {
    const a = anchor.getBoundingClientRect();
    const box = el?.getBoundingClientRect();
    const w = box?.width ?? 300;
    const h = box?.height ?? 200;
    // Below the chips when there's room, otherwise above them, and always inside the window.
    const below = a.bottom + 6 + h < window.innerHeight - 8;
    top = below ? a.bottom + 6 : Math.max(8, a.top - h - 6);
    const want = alignRight ? a.right - w : a.left;
    left = Math.max(8, Math.min(want, window.innerWidth - w - 8));
  }

  function outside(e: PointerEvent) {
    if (el && !el.contains(e.target as Node) && !anchor.contains(e.target as Node)) onclose();
  }
  function onKey(e: KeyboardEvent) {
    if (e.key === "Escape") {
      e.preventDefault();
      e.stopPropagation();
      onclose();
      anchor.focus({ preventScroll: true });
    }
  }
  const onScroll = (e: Event) => {
    if (!el?.contains(e.target as Node)) onclose();
  };
  const onBlur = () => onclose();

  onMount(async () => {
    await tick();
    place();
    el?.querySelector<HTMLButtonElement>("[role=tab]")?.focus({ preventScroll: true });
    window.addEventListener("pointerdown", outside, true);
    window.addEventListener("keydown", onKey, true);
    window.addEventListener("scroll", onScroll, true);
    window.addEventListener("resize", onBlur);
    window.addEventListener("blur", onBlur);
  });
  onDestroy(() => {
    window.removeEventListener("pointerdown", outside, true);
    window.removeEventListener("keydown", onKey, true);
    window.removeEventListener("scroll", onScroll, true);
    window.removeEventListener("resize", onBlur);
    window.removeEventListener("blur", onBlur);
  });
</script>

{#snippet glyph(text: string)}{#each emojiPieces(text) as piece}{#if piece.letters}<span class="flag">{piece.letters}</span>{:else if piece.missing}<span class="missing">{piece.text}</span>{:else}{piece.text}{/if}{/each}{/snippet}

<div class="who" role="dialog" aria-label="Reactions" bind:this={el} style:left="{left}px" style:top="{top}px">
  <div class="tabs" role="tablist" aria-label="Reactions by emoji">
    <button role="tab" aria-selected={tab === ""} class:on={tab === ""} onclick={() => (tab = "")}>All {reactors.length}</button>
    {#each groups as [emoji, count] (emoji)}
      <button role="tab" aria-selected={tab === emoji} aria-label="{emoji} {count}" class:on={tab === emoji} onclick={() => (tab = emoji)}>{@render glyph(emoji)} {count}</button>
    {/each}
  </div>
  <ul>
    {#each shown as r (r.jid)}
      <li class="reactor" data-jid={r.jid}>
        <Avatar name={r.name} jid={r.person} size={34} opensCard={!r.you} />
        <span class="name">
          <b>{r.name}</b>
          {#if r.you}<button class="remove" onclick={onremove} aria-label="Remove your reaction">Click to remove</button>{/if}
        </span>
        <span class="emoji" aria-label="Reacted {r.emoji}">{@render glyph(r.emoji)}</span>
      </li>
    {/each}
  </ul>
</div>

<style>
  .who {
    position: fixed;
    z-index: 60;
    width: 300px;
    max-width: calc(100vw - 16px);
    background: var(--panel);
    color: var(--text);
    border: 1px solid var(--line);
    border-radius: 12px;
    box-shadow: var(--shadow);
    padding: 6px 0 4px;
  }
  .tabs {
    display: flex;
    gap: 2px;
    padding: 0 8px 6px;
    border-bottom: 1px solid var(--line);
    overflow-x: auto;
  }
  .tabs button {
    flex: none;
    padding: 4px 9px;
    border-radius: 14px;
    font-size: 13px;
    color: var(--muted);
    white-space: nowrap;
  }
  .tabs button:hover {
    background: var(--hover);
  }
  .tabs button.on {
    background: var(--active);
    color: var(--text);
    font-weight: 600;
  }
  ul {
    list-style: none;
    margin: 0;
    padding: 4px 0 0;
    max-height: 320px;
    overflow-y: auto;
  }
  li {
    display: flex;
    align-items: center;
    gap: 10px;
    padding: 5px 12px;
  }
  .name {
    flex: 1;
    min-width: 0;
    display: flex;
    flex-direction: column;
  }
  .name b {
    font-weight: 600;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .remove {
    align-self: flex-start;
    padding: 0;
    font-size: 12px;
    color: var(--accent);
  }
  .remove:hover {
    text-decoration: underline;
  }
  .emoji {
    font-size: 20px;
    flex: none;
  }
  .flag {
    font-size: 11px;
    font-weight: 700;
    padding: 0 4px;
    border-radius: 4px;
    border: 1px solid var(--line);
    background: var(--hover);
  }
  .missing {
    border: 1.5px dotted var(--muted);
    border-radius: 6px;
    padding: 0 2px;
  }
</style>
