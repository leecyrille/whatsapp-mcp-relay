<script lang="ts">
  import { onMount } from "svelte";
  import { api, errorText, type Contact } from "../lib/api";
  import { groupOf, openChat, reloadChats, toast, ui } from "../lib/state.svelte";

  // A real WhatsApp group reaches other people, so it takes a name, chosen members and a confirmation. Custom
  // groups only exist in WA Relay.
  let { kind, onclose }: { kind: "whatsapp" | "custom"; onclose: () => void } = $props();

  const NAME_MAX = 100;
  const MEMBERS_MAX = 50;

  let name = $state("");
  let color = $state("#8a6fd1");
  let stage = $state<"pick" | "confirm" | "result">("pick");
  let contacts = $state<Contact[]>([]);
  let search = $state("");
  let filter = $state("all");
  let chosen = $state<string[]>([]);
  let addTo = $state("");
  let busy = $state(false);
  let failure = $state("");
  let result = $state<{ jid: string; added: string[]; not_added: { jid: string; code: number; invited?: boolean }[] } | null>(null);

  onMount(async () => {
    if (kind === "whatsapp") contacts = await api.contacts();
  });

  const shown = $derived.by(() => {
    const q = search.trim().toLowerCase();
    return contacts.filter((c) => {
      if (filter !== "all" && groupOf(c.jid) !== filter) return false;
      return !q || c.name.toLowerCase().includes(q) || c.number.includes(q);
    });
  });
  const nameOf = (jid: string) => contacts.find((c) => c.jid === jid)?.name ?? "Someone";
  const canContinue = $derived(name.trim().length > 0 && chosen.length > 0 && chosen.length <= MEMBERS_MAX);

  function toggle(jid: string) {
    chosen = chosen.includes(jid) ? chosen.filter((j) => j !== jid) : [...chosen, jid];
  }

  function why(person: { code: number; invited?: boolean }) {
    if (person.invited) return "WhatsApp offered them an invitation instead.";
    if (person.code === 403) return "Their privacy settings don't allow it.";
    return "WhatsApp didn't add them.";
  }

  async function open(jid: string) {
    if (addTo) {
      try {
        ui.groups = await api.assign([jid], addTo);
      } catch (e) {
        toast(errorText(e));
      }
    }
    reloadChats();
    openChat(jid);
  }

  async function create() {
    if (busy) return;
    busy = true;
    failure = "";
    try {
      const done = await api.whatsappGroupCreate(name.trim(), chosen);
      result = done;
      if (done.not_added.length === 0) {
        await open(done.jid);
        toast(`Created ${name.trim()}.`);
        onclose();
      } else {
        await open(done.jid);
        stage = "result";
      }
    } catch (e) {
      failure = errorText(e);
    }
    busy = false;
  }

  async function createCustom() {
    if (busy || !name.trim()) return;
    busy = true;
    failure = "";
    try {
      ui.groups = await api.groupCreate(name.trim(), color);
      toast(`Added ${name.trim()} to your own groups.`);
      onclose();
    } catch (e) {
      failure = errorText(e);
    }
    busy = false;
  }
</script>

<svelte:window onkeydown={(e) => e.key === "Escape" && !busy && onclose()} />

<div class="modal-back" role="dialog" aria-modal="true" aria-label={kind === "whatsapp" ? "New WhatsApp group" : "New custom group"}>
  <div class="modal new-group">
    {#if kind === "custom"}
      <h2>New custom group</h2>
      <p class="muted">Only for sorting your chats in WA Relay. Nothing changes on WhatsApp, and nobody is told.</p>
      <label class="field">
        <span>Name</span>
        <input class="input" bind:value={name} maxlength="40" aria-label="Custom group name" onkeydown={(e) => e.key === "Enter" && createCustom()} />
      </label>
      <label class="field">
        <span>Colour</span>
        <input type="color" bind:value={color} aria-label="Custom group colour" />
      </label>
      {#if failure}<p class="refused" role="alert">{failure}</p>{/if}
      <div class="actions">
        <button class="btn" onclick={onclose}>Cancel</button>
        <button class="btn primary" disabled={!name.trim() || busy} onclick={createCustom}>Add group</button>
      </div>
    {:else if stage === "pick"}
      <h2>New WhatsApp group</h2>
      <p class="muted">A real group on WhatsApp. You'll see who it adds before anything happens.</p>
      <label class="field">
        <span>Name</span>
        <input class="input" bind:value={name} maxlength={NAME_MAX} aria-label="WhatsApp group name" />
      </label>
      <div class="filters">
        <input class="input" type="search" placeholder="Search your contacts" aria-label="Search your contacts" bind:value={search} />
        <select class="input narrow" aria-label="Show contacts in" bind:value={filter}>
          <option value="all">All contacts</option>
          {#each ui.groups.groups as g (g.id)}
            <option value={g.id}>{g.name}</option>
          {/each}
        </select>
      </div>
      <div class="people" role="group" aria-label="People to add">
        {#each shown as person (person.jid)}
          <label class="person">
            <input type="checkbox" checked={chosen.includes(person.jid)} onchange={() => toggle(person.jid)} />
            <span class="who">{person.name}</span>
            <small class="muted">{person.number}</small>
          </label>
        {:else}
          <p class="muted empty">No contacts match.</p>
        {/each}
      </div>
      <label class="field">
        <span>Also put it in</span>
        <select class="input narrow" aria-label="Also put it in one of your groups" bind:value={addTo}>
          <option value="">None of your groups</option>
          {#each ui.groups.groups as g (g.id)}
            <option value={g.id}>{g.name}</option>
          {/each}
        </select>
      </label>
      {#if chosen.length > MEMBERS_MAX}<p class="refused">At most {MEMBERS_MAX} people at once.</p>{/if}
      <div class="actions">
        <span class="muted count">{chosen.length} chosen</span>
        <button class="btn" onclick={onclose}>Cancel</button>
        <button class="btn primary" disabled={!canContinue} onclick={() => ((failure = ""), (stage = "confirm"))}>Next</button>
      </div>
    {:else if stage === "confirm"}
      <h2>Create this WhatsApp group?</h2>
      <p class="group-name"><b>{name.trim()}</b></p>
      <p><b>{chosen.length === 1 ? "This person" : `These ${chosen.length} people`} will be added and notified on WhatsApp:</b></p>
      <ul class="final">
        {#each chosen as jid (jid)}<li>{nameOf(jid)}</li>{/each}
      </ul>
      {#if failure}<p class="refused" role="alert">{failure}</p>{/if}
      <div class="actions">
        <button class="btn" disabled={busy} onclick={() => (stage = "pick")}>Back</button>
        <button class="btn primary" disabled={busy} onclick={create}>{busy ? "Creating…" : "Create group and add them"}</button>
      </div>
    {:else if result}
      <h2>Created, but not everyone was added</h2>
      <p class="group-name"><b>{name.trim()}</b></p>
      <p><b>Added:</b></p>
      <ul class="final" aria-label="Added">
        {#each result.added as jid (jid)}<li>{nameOf(jid)}</li>{:else}<li class="muted">Nobody besides you</li>{/each}
      </ul>
      <p><b>Not added:</b></p>
      <ul class="final" aria-label="Not added">
        {#each result.not_added as person (person.jid)}<li>{nameOf(person.jid)}. {why(person)}</li>{/each}
      </ul>
      <div class="actions">
        <button class="btn primary" onclick={onclose}>Done</button>
      </div>
    {/if}
  </div>
</div>

<style>
  .new-group {
    width: min(480px, calc(100vw - 24px));
    max-height: calc(100vh - 24px);
    overflow-y: auto;
    padding: 18px 20px;
    user-select: text;
  }
  h2 {
    margin: 0 0 6px;
    font-size: 17px;
  }
  p {
    margin: 0 0 10px;
    overflow-wrap: anywhere;
  }
  .field {
    display: flex;
    align-items: center;
    gap: 9px;
    margin-bottom: 10px;
  }
  .field span {
    flex: none;
  }
  .field .input {
    flex: 1;
    min-width: 0;
  }
  .filters {
    display: flex;
    gap: 6px;
    margin-bottom: 6px;
  }
  .filters .input[type="search"] {
    flex: 1;
    min-width: 0;
  }
  .narrow {
    max-width: 170px;
  }
  .people {
    height: min(38vh, 280px);
    min-height: 110px;
    overflow-y: auto;
    border: 1px solid var(--line);
    border-radius: 8px;
    padding: 4px;
    margin-bottom: 10px;
  }
  .person {
    display: flex;
    align-items: center;
    gap: 8px;
    padding: 4px 6px;
    border-radius: 6px;
    min-width: 0;
  }
  .person:hover {
    background: var(--hover);
  }
  .who {
    flex: 1;
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .person small {
    flex: none;
  }
  .empty {
    padding: 12px;
    text-align: center;
  }
  .final {
    margin: 0 0 12px;
    padding-left: 20px;
    max-height: min(36vh, 240px);
    overflow-y: auto;
    overflow-wrap: anywhere;
  }
  .refused {
    color: var(--danger);
    font-weight: 600;
  }
  .actions {
    display: flex;
    justify-content: flex-end;
    align-items: center;
    gap: 8px;
  }
  .count {
    margin-right: auto;
  }
</style>
