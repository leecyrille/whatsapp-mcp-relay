<script lang="ts">
  import { api, errorText, type Message } from "../lib/api";
  import { clock } from "../lib/format";
  import { toast, ui } from "../lib/state.svelte";
  import Avatar from "../lib/Avatar.svelte";

  let {
    forwarding = $bindable(),
    deleting = $bindable(),
    info = $bindable(),
  }: { forwarding: Message | null; deleting: Message | null; info: Message | null } = $props();

  // WhatsApp's own limits, from its help pages.
  const FORWARD_MAX = 5;
  const REVOKE_MS = 60 * 60 * 60 * 1000;

  let busy = $state(false);
  let problem = $state("");
  let search = $state("");
  let chosen = $state<string[]>([]);

  const listed = $derived.by(() => {
    const q = search.trim().toLocaleLowerCase();
    return ui.chats.filter((c) => !q || c.name.toLocaleLowerCase().includes(q)).slice(0, 300);
  });
  const canRevoke = $derived(!!deleting && deleting.from_me && !deleting.revoked && Date.now() - deleting.ts < REVOKE_MS);

  function reset() {
    busy = false;
    problem = "";
    search = "";
    chosen = [];
  }

  function close() {
    forwarding = deleting = info = null;
    reset();
  }

  function toggle(jid: string) {
    if (chosen.includes(jid)) chosen = chosen.filter((c) => c !== jid);
    else if (chosen.length < FORWARD_MAX) chosen = [...chosen, jid];
  }

  async function forward() {
    if (!forwarding || !chosen.length) return;
    busy = true;
    try {
      await api.forwardMessage(forwarding.raw_chat, forwarding.id, chosen, `fwd-${Date.now()}-${Math.random().toString(36).slice(2, 8)}`);
      toast(chosen.length === 1 ? "Forwarding…" : `Forwarding to ${chosen.length} chats…`);
      close();
    } catch (e) {
      problem = errorText(e);
      busy = false;
    }
  }

  async function remove(everyone: boolean) {
    if (!deleting) return;
    busy = true;
    try {
      if (everyone) await api.revokeMessage(deleting.raw_chat, deleting.id);
      else await api.hideMessage(deleting.raw_chat, deleting.id);
      close();
    } catch (e) {
      problem = errorText(e);
      busy = false;
    }
  }

  function preview(m: Message) {
    return m.text || (m.media ? `[${m.media}${m.filename ? `: ${m.filename}` : ""}]` : "");
  }

  function when(ms: number) {
    return `${new Date(ms).toLocaleDateString()} ${clock(ms)}`;
  }

  const STATUS: Record<string, string> = { sent: "Sent", delivered: "Delivered", read: "Read", played: "Played" };
</script>

<svelte:window onkeydown={(e) => (forwarding || deleting || info) && e.key === "Escape" && !busy && close()} />

{#if forwarding}
  <div class="scrim">
    <div class="confirm wide" role="dialog" aria-modal="true" aria-labelledby="fwd-title">
      <h3 id="fwd-title">Forward message</h3>
      <p class="quoted">{preview(forwarding)}</p>
      <p class="note">Up to {FORWARD_MAX} chats, as on WhatsApp. It arrives marked as forwarded.</p>
      <!-- svelte-ignore a11y_autofocus -->
      <input aria-label="Search chats to forward to" placeholder="Search chats" bind:value={search} autofocus />
      <div class="chats" role="group" aria-label="Chats to forward to">
        {#each listed as c (c.jid)}
          {@const on = chosen.includes(c.jid)}
          <label class="chat" class:off={!on && chosen.length >= FORWARD_MAX}>
            <input type="checkbox" checked={on} disabled={!on && chosen.length >= FORWARD_MAX} onchange={() => toggle(c.jid)} />
            <Avatar name={c.name} jid={c.jid} isGroup={c.is_group} size={28} />
            <span>{c.name}</span>
          </label>
        {/each}
      </div>
      {#if problem}<p class="refused" role="alert">{problem}</p>{/if}
      <div class="actions">
        <span class="count">{chosen.length} of {FORWARD_MAX}</span>
        <button class="btn" onclick={close} disabled={busy}>Cancel</button>
        <button class="btn primary" onclick={forward} disabled={busy || !chosen.length}>{busy ? "Forwarding…" : "Forward"}</button>
      </div>
    </div>
  </div>
{:else if deleting}
  <div class="scrim">
    <div class="confirm" role="alertdialog" aria-modal="true" aria-labelledby="del-title">
      <h3 id="del-title">Delete message?</h3>
      <p class="quoted">{preview(deleting) || "This message was deleted"}</p>
      <p><b>Delete for me</b> hides it in WA Relay on all your computers. It stays on your phone, and everyone else still has it.</p>
      {#if canRevoke}
        <p><b>Delete for everyone</b> removes it from everyone's phone. They'll see "This message was deleted" in its place.</p>
      {:else if deleting.from_me && !deleting.revoked}
        <p class="note">Delete for everyone isn't offered: WhatsApp only allows it for about 2 days after sending.</p>
      {/if}
      {#if problem}<p class="refused" role="alert">{problem}</p>{/if}
      <div class="actions">
        <button class="btn" onclick={close} disabled={busy}>{problem ? "Close" : "Cancel"}</button>
        <button class="btn" onclick={() => remove(false)} disabled={busy}>Delete for me</button>
        {#if canRevoke}
          <button class="btn primary destructive" onclick={() => remove(true)} disabled={busy}>Delete for everyone</button>
        {/if}
      </div>
    </div>
  </div>
{:else if info}
  <div class="scrim">
    <div class="confirm" role="dialog" aria-modal="true" aria-labelledby="info-title">
      <h3 id="info-title">Message info</h3>
      <p class="quoted">{preview(info)}</p>
      <dl>
        <dt>Sent</dt>
        <dd>{when(info.ts)}</dd>
        <dt>Status</dt>
        <dd>{STATUS[info.status] ?? "Sent"}</dd>
        {#if info.edited}<dt>Edited</dt><dd>Yes</dd>{/if}
      </dl>
      <p class="note">WA Relay only has the overall status. WhatsApp on your phone lists who has read it, and when.</p>
      <div class="actions">
        <button class="btn primary" onclick={close}>Close</button>
      </div>
    </div>
  </div>
{/if}

<style>
  .scrim {
    position: fixed;
    inset: 0;
    z-index: 70;
    background: rgb(0 0 0 / 0.4);
    display: grid;
    place-items: center;
    padding: 16px;
  }
  .confirm {
    width: min(440px, 100%);
    max-height: calc(100vh - 32px);
    display: flex;
    flex-direction: column;
    background: var(--panel);
    color: var(--text);
    border-radius: 12px;
    box-shadow: var(--shadow);
    padding: 18px 20px;
    font-size: 14px;
  }
  .confirm h3 {
    margin: 0 0 8px;
    font-size: 16px;
  }
  .confirm p {
    margin: 0 0 10px;
    overflow-wrap: anywhere;
  }
  .quoted {
    border-left: 3px solid var(--accent);
    padding: 2px 0 2px 8px;
    color: var(--muted);
    max-height: 4.5em;
    overflow: hidden;
    white-space: pre-wrap;
  }
  .note {
    color: var(--muted);
    font-size: 13px;
  }

  .confirm input:not([type]) {
    width: 100%;
    padding: 7px 9px;
    margin-bottom: 8px;
    border: 1px solid var(--line);
    border-radius: 7px;
    background: var(--panel-2);
    color: var(--text);
    font: inherit;
  }
  .chats {
    overflow-y: auto;
    min-height: 120px;
    max-height: 320px;
    margin-bottom: 10px;
    border: 1px solid var(--line);
    border-radius: 8px;
    padding: 4px;
  }
  .chat {
    display: flex;
    align-items: center;
    gap: 9px;
    padding: 5px 6px;
    border-radius: 6px;
    cursor: pointer;
  }
  .chat:hover {
    background: var(--hover);
  }
  .chat span {
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
  }
  .chat.off {
    opacity: 0.5;
    cursor: default;
  }
  dl {
    display: grid;
    grid-template-columns: auto 1fr;
    gap: 4px 14px;
    margin: 0 0 10px;
  }
  dt {
    color: var(--muted);
  }
  dd {
    margin: 0;
  }
  .refused {
    color: var(--danger);
    font-weight: 600;
  }
  .actions {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    justify-content: flex-end;
    gap: 8px;
  }
  .count {
    margin-right: auto;
    color: var(--muted);
    font-size: 13px;
  }
  .btn.destructive,
  .btn.destructive:hover {
    background: var(--danger);
    color: var(--panel);
  }
</style>
