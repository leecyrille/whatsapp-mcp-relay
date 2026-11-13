<script lang="ts">
  // Everything the user wrote that hasn't gone: where it is, and a way to send it or let it go.
  import { listTime } from "../lib/format";
  import { openChat, ui } from "../lib/state.svelte";
  import { discard, discardDraft, held, retry, sendDraft, type Held } from "../lib/unsent.svelte";

  const items = $derived(held());
  let asking = $state<string | null>(null);

  function chatName(h: Held): string {
    if (h.orphan) return "A chat that's no longer in your list";
    const chat = ui.chats.find((c) => c.jid === h.chat);
    return chat?.name || `+${h.chat.split("@")[0]}`;
  }

  const WHAT: Record<Held["kind"], string> = {
    draft: "Draft",
    failed: "Didn't send",
    sending: "Sending",
    handed: "With your home PC, waiting for WhatsApp",
  };

  function send(h: Held) {
    if (h.kind === "draft") sendDraft(h.chat);
    else if (h.item) retry(h.item);
  }

  function remove(h: Held) {
    asking = null;
    if (h.kind === "draft") discardDraft(h.chat);
    else if (h.item) discard(h.item);
  }
</script>

{#if items.length}
  <section class="unsent" aria-label="Not sent yet">
    <h3>Not sent yet: {items.length}</h3>
    <ul>
      {#each items as h (h.key)}
        <li class:quiet={!h.blocks}>
          <div class="head">
            <b class="who">{chatName(h)}</b>
            <span class="muted">{WHAT[h.kind]} · {listTime(h.at)}</span>
          </div>
          {#if h.text}<div class="text">{h.text}</div>{/if}
          {#if h.kind === "failed" && h.item?.error}<small class="muted">{h.item.error}</small>{/if}
          {#if !h.blocks && h.kind !== "handed"}
            <small class="muted">{h.orphan ? "It can't be sent from here, so" : "It's over a day old, so"} it doesn't hold back updates{h.kept ? ", and it's kept through them" : ""}.</small>
          {/if}
          {#if asking === h.key}
            <div class="buttons">
              <span class="confirm">Discard this? It can't be brought back.</span>
              <button class="btn danger" onclick={() => remove(h)}>Yes, discard</button>
              <button class="btn" onclick={() => (asking = null)}>Keep it</button>
            </div>
          {:else}
            <div class="buttons">
              {#if !h.orphan}<button class="btn" onclick={() => openChat(h.chat)}>Open chat</button>{/if}
              {#if !h.orphan && (h.kind === "draft" || h.kind === "failed")}
                <button class="btn primary" onclick={() => send(h)}>Send now</button>
              {/if}
              {#if h.kind !== "handed"}<button class="btn" onclick={() => (asking = h.key)}>Discard…</button>{/if}
            </div>
          {/if}
        </li>
      {/each}
    </ul>
  </section>
{/if}

<style>
  .unsent {
    margin: 8px 0 12px;
    border: 1px solid var(--line);
    border-radius: 10px;
    padding: 8px 12px;
  }
  h3 {
    font-size: 13.5px;
    margin: 2px 0 6px;
  }
  ul {
    list-style: none;
    margin: 0;
    padding: 0;
  }
  li {
    padding: 8px 0;
    border-top: 1px solid var(--line);
    display: flex;
    flex-direction: column;
    gap: 4px;
  }
  li:first-child {
    border-top: none;
  }
  li.quiet .who {
    font-weight: 600;
    opacity: 0.8;
  }
  .head {
    display: flex;
    flex-wrap: wrap;
    gap: 8px;
    align-items: baseline;
  }
  .text {
    white-space: pre-wrap;
    overflow-wrap: anywhere;
    max-height: 4.5em;
    overflow: hidden;
  }
  .buttons {
    display: flex;
    flex-wrap: wrap;
    gap: 8px;
    align-items: center;
  }
  .confirm {
    color: var(--danger);
  }
</style>
