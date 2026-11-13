<script lang="ts" module>
  export interface MenuItem {
    label: string;
    action: () => void;
    danger?: boolean;
  }
</script>

<script lang="ts">
  import { onDestroy, onMount, tick } from "svelte";

  let { x, y, items, label, onclose }: { x: number; y: number; items: MenuItem[]; label: string; onclose: () => void } = $props();

  let el = $state<HTMLDivElement>();
  let left = $state(0);
  let top = $state(0);
  const returnTo = document.activeElement as HTMLElement | null;

  function buttons(): HTMLButtonElement[] {
    return el ? [...el.querySelectorAll<HTMLButtonElement>("button[role=menuitem]")] : [];
  }

  function close(refocus = true) {
    onclose();
    if (refocus) returnTo?.focus?.();
  }

  function pick(item: MenuItem) {
    close(false);
    item.action();
  }

  function onKey(e: KeyboardEvent) {
    const list = buttons();
    const at = list.indexOf(document.activeElement as HTMLButtonElement);
    let next = -1;
    if (e.key === "ArrowDown") next = (at + 1) % list.length;
    else if (e.key === "ArrowUp") next = (at - 1 + list.length) % list.length;
    else if (e.key === "Home") next = 0;
    else if (e.key === "End") next = list.length - 1;
    else if (e.key === "Escape" || e.key === "Tab") {
      e.preventDefault();
      close();
      return;
    } else return;
    e.preventDefault();
    list[next]?.focus();
  }

  function outside(e: PointerEvent) {
    if (el && !el.contains(e.target as Node)) close(false);
  }

  onMount(async () => {
    await tick();
    // Kept on screen, whichever corner it was opened in.
    const box = el?.getBoundingClientRect();
    const w = box?.width ?? 220;
    const h = box?.height ?? 300;
    left = Math.max(8, Math.min(x, window.innerWidth - w - 8));
    top = Math.max(8, Math.min(y, window.innerHeight - h - 8));
    buttons()[0]?.focus();
    window.addEventListener("pointerdown", outside, true);
    window.addEventListener("blur", () => close(false), { once: true });
  });
  onDestroy(() => window.removeEventListener("pointerdown", outside, true));
</script>

<!-- svelte-ignore a11y_interactive_supports_focus -->
<div class="menu" role="menu" aria-label={label} tabindex="-1" bind:this={el} style:left="{left}px" style:top="{top}px" onkeydown={onKey}>
  {#each items as item (item.label)}
    <button role="menuitem" class:danger={item.danger} onclick={() => pick(item)}>{item.label}</button>
  {/each}
</div>

<style>
  .menu {
    position: fixed;
    z-index: 60;
    min-width: 190px;
    background: var(--panel);
    border: 1px solid var(--line);
    border-radius: 9px;
    box-shadow: var(--shadow);
    padding: 4px;
  }
  button {
    display: block;
    width: 100%;
    padding: 7px 12px;
    border-radius: 6px;
    text-align: left;
    color: var(--text);
  }
  button:hover,
  button:focus-visible {
    background: var(--hover);
    outline: none;
  }
  .danger {
    color: var(--danger);
  }
</style>
