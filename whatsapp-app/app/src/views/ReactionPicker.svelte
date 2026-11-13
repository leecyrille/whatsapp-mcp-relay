<script lang="ts">
  // The quick reactions. Drawn fixed to the window, outside the message row: rows use content-visibility, which
  // clips anything they draw outside themselves, so a picker inside one was cut off by the message above.
  import { onDestroy, onMount, tick } from "svelte";

  let {
    anchor,
    alignRight = false,
    current = "",
    onpick,
    onclose,
  }: { anchor: HTMLElement; alignRight?: boolean; current?: string; onpick: (emoji: string) => void; onclose: () => void } = $props();

  const QUICK = ["👍", "❤️", "😂", "😮", "😢", "🙏"];
  let el: HTMLDivElement | undefined = $state();
  let left = $state(-9999);
  let top = $state(-9999);
  let below = $state(false);

  function place() {
    const a = anchor.getBoundingClientRect();
    const box = el?.getBoundingClientRect();
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
    el?.querySelector<HTMLButtonElement>("button")?.focus({ preventScroll: true });
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

<div class="picker" class:below role="menu" aria-label="React" bind:this={el} style:left="{left}px" style:top="{top}px">
  {#each QUICK as emoji}
    <button role="menuitem" onclick={() => onpick(emoji)} class:on={current === emoji}>{emoji}</button>
  {/each}
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
</style>
