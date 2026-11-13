<script lang="ts">
  import { onMount } from "svelte";
  import { api, errorText, type Card, type CardChange, type LinkSuggestion, type Labeled } from "../lib/api";
  import { toast, ui } from "../lib/state.svelte";
  import Avatar from "../lib/Avatar.svelte";

  let { open, onclose }: { open: { id?: string; jid?: string; name?: string }; onclose: () => void } = $props();

  type ListField = "phones" | "emails" | "urls" | "dates" | "related" | "handles";
  const LISTS: { field: ListField; title: string; add: string; labels: string[]; placeholder: string }[] = [
    { field: "phones", title: "Phone numbers", add: "Add a number", labels: ["mobile", "work", "home", "main", "other"], placeholder: "+1 780 555 0100" },
    { field: "emails", title: "Email addresses", add: "Add an email", labels: ["work", "home", "other"], placeholder: "name@example.com" },
    { field: "urls", title: "Websites", add: "Add a website", labels: ["work", "home", "profile", "other"], placeholder: "https://" },
    { field: "dates", title: "Dates", add: "Add a date", labels: ["birthday", "anniversary", "other"], placeholder: "2024-05-31, or --05-31" },
    { field: "related", title: "Related people", add: "Add a person", labels: ["spouse", "partner", "child", "parent", "assistant", "manager", "other"], placeholder: "Their name" },
    { field: "handles", title: "Social and messaging", add: "Add a handle", labels: ["LinkedIn", "Teams", "Skype", "Signal", "X", "other"], placeholder: "Handle or link" },
  ];

  let card = $state<Card | null>(null);
  let draft = $state<Card>(empty());
  let loading = $state(true);
  let busy = $state(false);
  let problem = $state("");
  let suggestions = $state<LinkSuggestion[] | null>(null);
  let confirmingLoose = $state<LinkSuggestion | null>(null);
  let history = $state<CardChange[] | null>(null);
  let deleting = $state(false);
  let offline = $state(false);

  function empty(): Card {
    return {
      id: "", version: 0, created_ms: 0, updated_ms: 0, display: "", name: {}, nickname: "", phones: [], emails: [], company: "",
      department: "", title: "", addresses: [], urls: [], dates: [], related: [], handles: [], notes: "", links: [],
    };
  }

  function editable(c: Card): Card {
    const copy: Card = JSON.parse(JSON.stringify({ ...empty(), ...c }));
    copy.name = { prefix: "", given: "", middle: "", family: "", suffix: "", ...c.name };
    return copy;
  }

  function nameOf(jid: string): string {
    return ui.chats.find((c) => c.jid === jid)?.name ?? `+${jid.split("@")[0]}`;
  }

  async function load() {
    loading = true;
    try {
      const doc = await api.cardsGet();
      offline = !!doc.offline;
      const found = open.id ? doc.cards.find((c) => c.id === open.id) : open.jid ? doc.cards.find((c) => c.links?.includes(open.jid!)) : null;
      card = found ?? null;
      if (found) draft = editable(found);
      else {
        draft = editable({ ...empty(), display: open.name && !open.name.startsWith("+") ? open.name : "", links: open.jid ? [open.jid] : [] });
        if (open.jid?.endsWith("@s.whatsapp.net")) draft.phones = [{ label: "mobile", value: `+${open.jid.split("@")[0]}` }];
      }
    } catch (e) {
      problem = errorText(e);
    }
    loading = false;
  }

  function addRow(field: ListField) {
    draft[field] = [...(draft[field] ?? []), { label: "", value: "" }];
  }

  function removeRow(field: ListField, i: number) {
    draft[field] = (draft[field] ?? []).filter((_, j) => j !== i);
  }

  function cleaned(): Partial<Card> {
    const keep = (list?: Labeled[]) => (list ?? []).filter((x) => x.value.trim());
    return {
      ...draft,
      phones: keep(draft.phones), emails: keep(draft.emails), urls: keep(draft.urls), dates: keep(draft.dates),
      related: keep(draft.related), handles: keep(draft.handles),
      addresses: (draft.addresses ?? []).filter((a) => a.street || a.city || a.region || a.postcode || a.country),
    };
  }

  async function save() {
    busy = true;
    problem = "";
    try {
      const done = await api.cardSave(cleaned(), card?.version ?? 0);
      toast(card ? "Card saved, on all your computers." : "Card made, on all your computers.");
      card = done.card;
      draft = editable(done.card);
      onclose();
    } catch (e) {
      problem = errorText(e);
    }
    busy = false;
  }

  async function findMatches() {
    problem = "";
    try {
      suggestions = (await api.cardSuggest(cleaned())).suggestions;
    } catch (e) {
      problem = errorText(e);
    }
  }

  function link(s: LinkSuggestion) {
    if (s.why !== "same number" && confirmingLoose?.jid !== s.jid) {
      confirmingLoose = s;
      return;
    }
    confirmingLoose = null;
    draft.links = [...(draft.links ?? []), s.jid];
    suggestions = (suggestions ?? []).filter((x) => x.jid !== s.jid);
  }

  function unlink(jid: string) {
    draft.links = (draft.links ?? []).filter((l) => l !== jid);
  }

  async function showHistory() {
    try {
      history = card ? (await api.cardHistory(card.id)).entries : [];
    } catch (e) {
      problem = errorText(e);
    }
  }

  async function undo(entry: CardChange) {
    busy = true;
    problem = "";
    try {
      const done = await api.cardUndo(entry.seq);
      toast("Change undone.");
      if (done.card) {
        card = done.card;
        draft = editable(done.card);
        await showHistory();
      } else onclose();
    } catch (e) {
      problem = errorText(e);
    }
    busy = false;
  }

  async function remove() {
    if (!card) return;
    busy = true;
    problem = "";
    try {
      await api.cardDelete(card.id, card.version);
      toast("Card deleted. Contacts > Cards > Deleted cards can bring it back.");
      onclose();
    } catch (e) {
      problem = errorText(e);
      busy = false;
    }
  }

  const WHO = (who: string) => (who === "connector" ? "Claude, through the connector" : who.replace(/^app:/, "WA Relay, signed in as "));
  const ACTION: Record<string, string> = { create: "Made", update: "Changed", delete: "Deleted", import: "Imported", undo: "Undone" };

  onMount(load);
</script>

<svelte:window onkeydown={(e) => e.key === "Escape" && !busy && onclose()} />

<div class="scrim">
  <div class="dialog" role="dialog" aria-modal="true" aria-labelledby="card-title">
    <header>
      <Avatar name={draft.display || open.name || "?"} jid={draft.links?.[0] ?? ""} size={44} />
      <div class="head">
        <h3 id="card-title">{card ? draft.display || "Contact card" : "New contact card"}</h3>
        <p class="note">Kept on your home PC and shared by all your computers. It doesn't change WhatsApp or your phone, and it doesn't sync to your phone's contacts.</p>
      </div>
    </header>

    {#if loading}
      <p class="note">Loading…</p>
    {:else if history}
      <section class="body">
        <h4>History</h4>
        {#if history.length === 0}<p class="note">No changes yet.</p>{/if}
        <ul class="history">
          {#each history as entry (entry.seq)}
            <li>
              <div>
                <b>{ACTION[entry.action] ?? entry.action}</b> {new Date(entry.at_ms).toLocaleString()}
                <div class="note">By {WHO(entry.who)}</div>
              </div>
              <button class="btn" onclick={() => undo(entry)} disabled={busy}>Undo this change</button>
            </li>
          {/each}
        </ul>
      </section>
      <div class="actions">
        <button class="btn" onclick={() => (history = null)}>Back to the card</button>
      </div>
    {:else}
      <form class="body" onsubmit={(e) => { e.preventDefault(); void save(); }}>
        {#if offline}<p class="refused" role="status">Not connected to your home PC, so this is the last copy saved here and can't be changed now.</p>{/if}
        <label class="wide">Name shown <input bind:value={draft.display} placeholder="Leave empty to use the name below" maxlength="80" /></label>
        <div class="grid">
          <label>Prefix <input bind:value={draft.name.prefix} /></label>
          <label>First <input bind:value={draft.name.given} /></label>
          <label>Middle <input bind:value={draft.name.middle} /></label>
          <label>Last <input bind:value={draft.name.family} /></label>
          <label>Suffix <input bind:value={draft.name.suffix} /></label>
          <label>Nickname <input bind:value={draft.nickname} /></label>
          <label>Company <input bind:value={draft.company} /></label>
          <label>Department <input bind:value={draft.department} /></label>
          <label>Title <input bind:value={draft.title} /></label>
        </div>

        {#each LISTS as list (list.field)}
          <fieldset>
            <legend>{list.title}</legend>
            {#each draft[list.field] ?? [] as row, i}
              <div class="row">
                <input class="label" aria-label="{list.title}: label" list="labels-{list.field}" bind:value={row.label} placeholder="label" />
                <input class="value" aria-label="{list.title}: value" bind:value={row.value} placeholder={list.placeholder} />
                <button type="button" class="icon-btn" aria-label="Remove" onclick={() => removeRow(list.field, i)}>×</button>
              </div>
            {/each}
            <datalist id="labels-{list.field}">
              {#each list.labels as l}<option value={l}></option>{/each}
            </datalist>
            <button type="button" class="link" onclick={() => addRow(list.field)}>+ {list.add}</button>
          </fieldset>
        {/each}

        <fieldset>
          <legend>Addresses</legend>
          {#each draft.addresses ?? [] as a, i}
            <div class="address">
              <input aria-label="Address: label" bind:value={a.label} placeholder="label" />
              <input aria-label="Street" bind:value={a.street} placeholder="Street" />
              <input aria-label="City" bind:value={a.city} placeholder="City" />
              <input aria-label="Province or state" bind:value={a.region} placeholder="Province or state" />
              <input aria-label="Postal code" bind:value={a.postcode} placeholder="Postal code" />
              <input aria-label="Country" bind:value={a.country} placeholder="Country" />
              <button type="button" class="link" onclick={() => (draft.addresses = (draft.addresses ?? []).filter((_, j) => j !== i))}>Remove this address</button>
            </div>
          {/each}
          <button type="button" class="link" onclick={() => (draft.addresses = [...(draft.addresses ?? []), { label: "", street: "", city: "", region: "", postcode: "", country: "" }])}>+ Add an address</button>
        </fieldset>

        <label class="wide">Notes <textarea bind:value={draft.notes} rows="3"></textarea></label>

        <fieldset>
          <legend>WhatsApp</legend>
          <p class="note">The card's name shows for everyone linked here, in WA Relay and the connector.</p>
          {#each draft.links ?? [] as jid (jid)}
            <div class="linked">
              <Avatar name={nameOf(jid)} jid={jid} size={28} />
              <span>{nameOf(jid)} <span class="note">{jid.split("@")[0]}</span></span>
              <button type="button" class="btn" onclick={() => unlink(jid)}>Unlink</button>
            </div>
          {/each}
          <button type="button" class="link" onclick={findMatches}>Find matching WhatsApp chats</button>
          {#if suggestions}
            {#if suggestions.length === 0}<p class="note">No matches. Only exactly the same number counts as a match.</p>{/if}
            {#each suggestions as s (s.jid)}
              <div class="linked">
                <Avatar name={s.name || s.jid} jid={s.jid} size={28} />
                <span>{s.name || `+${s.jid.split("@")[0]}`} <span class="note">{s.why}</span></span>
                {#if s.linked_to}
                  <span class="note">On {s.linked_to}'s card</span>
                {:else}
                  <button type="button" class="btn" onclick={() => link(s)}>{confirmingLoose?.jid === s.jid ? "Yes, link them" : "Link"}</button>
                {/if}
              </div>
              {#if confirmingLoose?.jid === s.jid}
                <p class="refused" role="alert">Only the name matches, which doesn't mean it's the same person. Link them only if you're sure.</p>
              {/if}
            {/each}
          {/if}
        </fieldset>

        {#if problem}<p class="refused" role="alert">{problem}</p>{/if}
        {#if deleting}
          <p class="refused" role="alert">Delete this card? Its name stops showing for the people linked to it. It can be brought back from Deleted cards.</p>
        {/if}
        <div class="actions">
          {#if card}
            {#if deleting}
              <button type="button" class="btn left" onclick={() => (deleting = false)} disabled={busy}>Keep it</button>
              <button type="button" class="btn destructive" onclick={remove} disabled={busy}>Delete card</button>
            {:else}
              <button type="button" class="btn left danger" onclick={() => (deleting = true)} disabled={busy || offline}>Delete…</button>
              <button type="button" class="btn" onclick={showHistory} disabled={busy || offline}>History</button>
            {/if}
          {/if}
          <button type="button" class="btn" onclick={onclose} disabled={busy}>Cancel</button>
          <button type="submit" class="btn primary" disabled={busy || offline}>{busy ? "Saving…" : "Save"}</button>
        </div>
      </form>
    {/if}
  </div>
</div>

<style>
  .scrim {
    position: fixed;
    inset: 0;
    z-index: 75;
    background: rgb(0 0 0 / 0.4);
    display: grid;
    place-items: center;
    padding: 16px;
  }
  .dialog {
    width: min(620px, 100%);
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
  header {
    display: flex;
    gap: 12px;
    align-items: flex-start;
  }
  .head {
    min-width: 0;
  }
  h3 {
    margin: 0 0 4px;
    font-size: 17px;
    overflow-wrap: anywhere;
  }
  h4 {
    margin: 4px 0 8px;
  }
  .note {
    color: var(--muted);
    font-size: 12.5px;
    margin: 0 0 6px;
  }
  .body {
    overflow-y: auto;
    min-height: 0;
    margin-top: 10px;
    padding-right: 4px;
    display: flex;
    flex-direction: column;
    gap: 10px;
  }
  label {
    display: flex;
    flex-direction: column;
    gap: 3px;
    font-size: 12.5px;
    color: var(--muted);
  }
  input,
  textarea {
    padding: 6px 8px;
    border: 1px solid var(--line);
    border-radius: 7px;
    background: var(--panel-2);
    color: var(--text);
    font: inherit;
    font-size: 14px;
    min-width: 0;
  }
  .grid {
    display: grid;
    grid-template-columns: repeat(3, minmax(0, 1fr));
    gap: 8px;
  }
  fieldset {
    border: 1px solid var(--line);
    border-radius: 9px;
    padding: 6px 10px 8px;
    margin: 0;
  }
  legend {
    font-weight: 600;
    padding: 0 4px;
  }
  .row {
    display: flex;
    gap: 6px;
    margin-bottom: 6px;
    align-items: center;
  }
  .row .label {
    width: 120px;
    flex: none;
  }
  .row .value {
    flex: 1;
  }
  .address {
    display: grid;
    grid-template-columns: repeat(3, minmax(0, 1fr));
    gap: 6px;
    margin-bottom: 8px;
  }
  .address .link {
    grid-column: 1 / -1;
    justify-self: start;
  }
  .link {
    color: var(--accent);
    font-size: 13px;
    padding: 2px 0;
  }
  .linked {
    display: flex;
    align-items: center;
    gap: 9px;
    padding: 4px 0;
  }
  .linked span {
    flex: 1;
    min-width: 0;
    overflow-wrap: anywhere;
  }
  .history {
    list-style: none;
    padding: 0;
    margin: 0;
  }
  .history li {
    display: flex;
    gap: 10px;
    align-items: center;
    justify-content: space-between;
    padding: 6px 0;
    border-bottom: 1px solid var(--line);
  }
  .refused {
    color: var(--danger);
    font-weight: 600;
    margin: 0;
  }
  .actions {
    display: flex;
    flex-wrap: wrap;
    justify-content: flex-end;
    gap: 8px;
    padding-top: 10px;
  }
  .actions .left {
    margin-right: auto;
  }
  .btn.danger {
    color: var(--danger);
  }
  .btn.destructive,
  .btn.destructive:hover {
    background: var(--danger);
    color: var(--panel);
  }
  @media (max-width: 560px) {
    .grid,
    .address {
      grid-template-columns: minmax(0, 1fr);
    }
  }
</style>
