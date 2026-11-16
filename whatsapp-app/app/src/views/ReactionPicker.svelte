<script lang="ts">
  // The quick reactions. Drawn fixed to the window, outside the message row: rows use content-visibility, which
  // clips anything they draw outside themselves, so a picker inside one was cut off by the message above.
  import { onDestroy, onMount, tick } from "svelte";
  import EmojiPicker from "../lib/EmojiPicker.svelte";
  import { noteUsed } from "../lib/emoji";

  let {
    anchor,
    alignRight = false,
    current = "",
    onpick,
    onclose,
  }: { anchor: HTMLElement; alignRight?: boolean; current?: string; onpick: (emoji: string) => void; onclose: () => void } = $props();

  const QUICK = ["👍", "❤️", "😂", "😮", "😢", "🙏"];
  // The whole picker sits in here once "+" is pressed, so a click in it counts as inside.
  let el: HTMLDivElement | undefined = $state();
  let bar: HTMLDivElement | undefined = $state();
  let full = $state(false);
  let left = $state(-9999);
  let top = $state(-9999);
  let below = $state(false);

  function place() {
    const a = anchor.getBoundingClientRect();
    const box = bar?.getBoundingClientRect();
    const w = box?.width ?? 240;
    const h = box?.height ?? 40;
    // Above the button when there's room, otherwise below it, and always inside the window.
    below = a.top - h - 6 < 8;
    top = below ? Math.min(a.bottom + 6, window.innerHeight - h - 8) : a.top - h - 6;
    const want = alignRight ? a.right - w : a.left;
    left = Math.max(8, Math.min(want, window.innerWidth - w - 8));
  }

  function outside(e: PointerEvent) {
    if (el && !el.contains(e.target as Node) && !anchor.contains(e.target as Node)) onclose();
  }
  function onKey(e: KeyboardEvent) {
    // The full picker clears its search with the first Escape, and closes with the next.
    if (full) return;
    if (e.key === "Escape") {
      e.preventDefault();
      onclose();
    }
  }
  // The button it hangs from moves when the chat scrolls, so it closes rather than float away.
  const onScroll = (e: Event) => {
    if (!el?.contains(e.target as Node)) onclose();
  };
  const onBlur = () => onclose();

  onMount(async () => {
    await tick();
    place();
    bar?.querySelector<HTMLButtonElement>("button")?.focus({ preventScroll: true });
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

<div bind:this={el}>
  {#if full}
    <EmojiPicker {anchor} {alignRight} {current} label="React with any emoji" {onpick} {onclose} />
  {:else}
    <div class="picker" class:below role="menu" aria-label="React" bind:this={bar} style:left="{left}px" style:top="{top}px">
      {#each QUICK as emoji}
        <button role="menuitem" onclick={() => { noteUsed(emoji); onpick(emoji); }} class:on={current === emoji}>{emoji}</button>
      {/each}
      {#if current && !QUICK.includes(current)}
        <button role="menuitem" onclick={() => onpick(current)} class="on" title="Your reaction">{current}</button>
      {/if}
      <button role="menuitem" class="more" title="More emoji" aria-label="More emoji" onclick={() => (full = true)}>+</button>
    </div>
  {/if}
</div>

<style>
  .picker {
    position: fixed;
    z-index: 60;
    background: var(--panel);
    border-radius: 20px;
    box-shadow: var(--shadow);
    display: flex;
    padding: 3px;
  }
  .picker button {
    font-size: 18px;
    width: 34px;
    height: 34px;
    border-radius: 50%;
  }
  .picker button:hover,
  .picker button.on {
    background: var(--active);
  }
  .picker .more {
    font-size: 20px;
    font-weight: 600;
    color: var(--muted);
    background: var(--hover);
  }
</style>
