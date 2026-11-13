<script lang="ts">
  import { fileUrl, type Message } from "../lib/api";
  import Icon from "../lib/Icon.svelte";
  let { msg, onclose }: { msg: Message; onclose: () => void } = $props();
</script>

<svelte:window onkeydown={(e) => e.key === "Escape" && onclose()} />

<div
  class="back"
  role="dialog"
  aria-modal="true"
  aria-label="Image"
  tabindex="-1"
  onclick={(e) => e.target === e.currentTarget && onclose()}
  onkeydown={() => {}}
>
  <button class="close" onclick={onclose} aria-label="Close"><Icon name="close" size={22} /></button>
  <img src={fileUrl(msg.rowid)} alt={msg.text || "Image"} />
  {#if msg.text}<p class="selectable">{msg.text}</p>{/if}
</div>

<style>
  .back {
    position: fixed;
    inset: 0;
    background: rgb(0 0 0 / 0.86);
    display: flex;
    flex-direction: column;
    align-items: center;
    justify-content: center;
    z-index: 50;
    padding: 24px;
    gap: 10px;
  }
  img {
    max-width: 100%;
    max-height: calc(100% - 50px);
    object-fit: contain;
    min-height: 0;
  }
  p {
    color: #eee;
    margin: 0;
    max-width: 780px;
    text-align: center;
  }
  .close {
    position: absolute;
    top: 12px;
    right: 12px;
    color: #fff;
    width: 40px;
    height: 40px;
    border-radius: 50%;
    display: grid;
    place-items: center;
    background: rgb(255 255 255 / 0.12);
  }
</style>
