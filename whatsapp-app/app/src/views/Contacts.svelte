<script lang="ts">
  import { onDestroy, onMount } from "svelte";
  import { api, errorText, type Card, type CardChange, type Contact } from "../lib/api";
  import { groupOf, on, openChat, toast, ui } from "../lib/state.svelte";
  import { parseVcards, type ParsedCard } from "../lib/vcard";
  import Avatar from "../lib/Avatar.svelte";
  import Icon from "../lib/Icon.svelte";
  import ImportDialog from "./ImportDialog.svelte";
  import SortDialog from "./SortDialog.svelte";

  // Contact cards: the user's own details for people, kept on the home PC.
  let tab = $state<"whatsapp" | "cards">("whatsapp");
  let cards = $state<Card[]>([]);
  let cardsOffline = $state(false);
  let cardQuery = $state("");
  let deleted = $state<CardChange[] | null>(null);
  let importing = $state<{ cards: ParsedCard[]; tooMany: boolean } | null>(null);

  async function loadCards() {
    try {
      const doc = await api.cardsGet();
      cards = doc.cards;
      cardsOffline = !!doc.offline;
    } catch (e) {
      toast(errorText(e));
    }
  }

  function cardText(c: Card): string {
    const parts: (string | undefined)[] = [c.display, c.name.given, c.name.middle, c.name.family, c.nickname, c.company, c.department, c.title, c.notes];
    for (const list of [c.phones, c.emails, c.urls, c.related, c.handles, c.dates]) for (const it of list ?? []) parts.push(it.value, it.label);
    for (const a of c.addresses ?? []) parts.push(a.street, a.city, a.region, a.postcode, a.country);
    return parts.filter(Boolean).join("\n").toLowerCase();
  }

  const shownCards = $derived.by(() => {
    const q = cardQuery.trim().toLowerCase();
    if (!q) return cards;
    const digits = q.replace(/\D/g, "");
    return cards.filter(
      (c) => cardText(c).includes(q) || (digits.length >= 4 && [...(c.phones ?? []).map((p) => p.value.replace(/\D/g, "")), ...(c.links ?? [])].some((v) => v.includes(digits))),
    );
  });

  function subtitle(c: Card): string {
    const work = [c.title, c.company].filter(Boolean).join(", ");
    return [work, c.phones?.[0]?.value, c.emails?.[0]?.value].filter(Boolean).join(" · ");
  }

  async function importFile() {
    try {
      const text = await api.pickVcard();
      if (text === null) return;
      const parsed = parseVcards(text);
      if (!parsed.cards.length) {
        toast("No contact cards found in that file.");
        return;
      }
      importing = parsed;
    } catch (e) {
      toast(errorText(e));
    }
  }

  async function showDeleted() {
    try {
      deleted = (await api.cardHistory(null)).entries;
    } catch (e) {
      toast(errorText(e));
    }
  }

  async function restore(entry: CardChange) {
    try {
      await api.cardUndo(entry.seq);
      toast(`${entry.before?.display} is back.`);
      await showDeleted();
    } catch (e) {
      toast(errorText(e));
    }
  }

  const ROW = 58;
  let all = $state<Contact[]>([]);
  let query = $state("");
  let filter = $state("all");
  let scrollTop = $state(0);
  let viewport = $state(600);
  let sorting = $state(false);

  async function load() {
    all = await api.contacts();
  }

  const groupIds = $derived(new Set(ui.groups.groups.map((g) => g.id)));
  const shown = $derived.by(() => {
    const q = query.trim().toLowerCase();
    return all.filter((c) => {
      if (q && !c.name.toLowerCase().includes(q) && !c.number.includes(q)) return false;
      if (filter === "all") return true;
      const g = groupOf(c.jid);
      if (filter === "unsorted") return !g || !groupIds.has(g);
      return g === filter;
    });
  });

  const first = $derived(Math.max(0, Math.floor(scrollTop / ROW) - 6));
  const last = $derived(Math.min(shown.length, Math.ceil((scrollTop + viewport) / ROW) + 6));
  const visible = $derived(shown.slice(first, last));

  async function assign(jid: string, group: string) {
    try {
      ui.groups = await api.assign([jid], group);
    } catch (e) {
      toast(errorText(e));
    }
  }

  let off: () => void;
  let offCards: () => void;
  onMount(() => {
    void load();
    void loadCards();
    off = on("contacts-changed", () => void load());
    offCards = on("cards-changed", () => {
      void loadCards();
      if (deleted) void showDeleted();
    });
  });
  onDestroy(() => {
    off?.();
    offCards?.();
  });
</script>

<section class="page" aria-label="Contacts">
  <header>
    <h1>Contacts</h1>
    <div class="tabs" role="tablist" aria-label="Kind of contacts">
      <button role="tab" aria-selected={tab === "whatsapp"} class:on={tab === "whatsapp"} onclick={() => (tab = "whatsapp")}>WhatsApp <span class="faint">{all.length}</span></button>
      <button role="tab" aria-selected={tab === "cards"} class:on={tab === "cards"} onclick={() => {
        tab = "cards";
        void loadCards();
      }}>Cards <span class="faint">{cards.length}</span></button>
    </div>
    <div class="grow"></div>
    {#if tab === "whatsapp"}
      <button class="btn" onclick={() => (sorting = true)}>
        <Icon name="sparkle" size={15} /> Sort with Claude
      </button>
    {:else}
      <button class="btn" onclick={importFile} disabled={cardsOffline}>Import vCard…</button>
      <button class="btn primary" onclick={() => (ui.card = {})} disabled={cardsOffline}>New card</button>
    {/if}
  </header>
  {#if tab === "cards"}
    <div class="filters">
      <input class="input" type="search" placeholder="Search any detail: name, number, email, company, city…" aria-label="Search contact cards" bind:value={cardQuery} />
    </div>
    {#if cardsOffline}<p class="empty">Not connected to your home PC, so this is the last copy saved here.</p>{/if}
    <div class="cards">
      {#if shownCards.length === 0}
        <p class="empty">{cards.length ? "No cards match." : "No contact cards yet. Make one, or import a .vcf file from your phone or address book."}</p>
      {/if}
      {#each shownCards as c (c.id)}
        <button class="card-row" onclick={() => (ui.card = { id: c.id })}>
          <Avatar name={c.display} jid={c.links?.[0] ?? ""} size={38} />
          <span class="who">
            <span class="name">{c.display}</span>
            <span class="num">{subtitle(c)}</span>
          </span>
          {#if c.links?.length}<span class="badge" title="Linked to WhatsApp">WhatsApp</span>{/if}
        </button>
      {/each}
      <div class="deleted">
        {#if deleted === null}
          <button class="btn" onclick={showDeleted}>Deleted cards…</button>
        {:else}
          <h2>Deleted cards</h2>
          {#if deleted.length === 0}<p class="faint">None.</p>{/if}
          {#each deleted as entry (entry.seq)}
            <div class="gone">
              <span>{entry.before?.display} <span class="faint">deleted {new Date(entry.at_ms).toLocaleString()}</span></span>
              <button class="btn" onclick={() => restore(entry)}>Bring back</button>
            </div>
          {/each}
        {/if}
      </div>
    </div>
  {:else}
  <div class="filters">
    <input class="input" type="search" placeholder="Search by name or number" aria-label="Search contacts" bind:value={query} />
    <select class="input narrow" aria-label="Filter by group" bind:value={filter}>
      <option value="all">All groups</option>
      {#each ui.groups.groups as g (g.id)}
        <option value={g.id}>{g.name}</option>
      {/each}
      <option value="unsorted">Unsorted</option>
    </select>
  </div>
  <div class="list" bind:clientHeight={viewport} onscroll={(e) => (scrollTop = e.currentTarget.scrollTop)}>
    {#if shown.length === 0}
      <p class="empty">No contacts match.</p>
    {/if}
    <div style:height="{shown.length * ROW}px" style:position="relative">
      {#each visible as contact, i (contact.jid)}
        <div class="row" style:top="{(first + i) * ROW}px">
          <Avatar name={contact.name} jid={contact.jid} size={38} editable />
          <div class="who">
            <div class="name">{contact.name}</div>
            <div class="num selectable">{contact.number}</div>
          </div>
          <select class="input narrow" aria-label="Group for {contact.name}" value={groupOf(contact.jid)} onchange={(e) => assign(contact.jid, e.currentTarget.value)}>
            <option value="">Unsorted</option>
            {#each ui.groups.groups as g (g.id)}
              <option value={g.id}>{g.name}</option>
            {/each}
          </select>
          <button class="icon-btn" title="Message" aria-label="Message {contact.name}" onclick={() => openChat(contact.jid)}>
            <Icon name="chats" size={18} />
          </button>
        </div>
      {/each}
    </div>
  </div>
  {/if}
</section>

{#if sorting}
  <SortDialog onclose={() => (sorting = false)} />
{/if}

{#if importing}
  <ImportDialog cards={importing.cards} tooMany={importing.tooMany} onclose={() => (importing = null)} />
{/if}

<style>
  .page {
    flex: 1;
    min-width: 0;
    display: flex;
    flex-direction: column;
    background: var(--panel);
  }
  header {
    display: flex;
    align-items: center;
    padding: 14px 18px 6px;
  }
  h1 {
    font-size: 18px;
    margin: 0;
  }
  .grow {
    flex: 1;
  }
  header .btn {
    display: inline-flex;
    gap: 6px;
    align-items: center;
  }
  .filters {
    display: flex;
    gap: 8px;
    padding: 6px 18px 10px;
  }
  .narrow {
    width: 150px;
    flex: none;
  }
  .list {
    flex: 1;
    overflow-y: auto;
    min-height: 0;
  }
  .row {
    position: absolute;
    left: 0;
    right: 0;
    height: 58px;
    display: flex;
    align-items: center;
    gap: 11px;
    padding: 0 18px;
    contain: strict;
  }
  .row:hover {
    background: var(--hover);
  }
  .who {
    flex: 1;
    min-width: 0;
  }
  .name {
    font-weight: 600;
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
  }
  .num {
    font-size: 12.5px;
    color: var(--muted);
  }
  select option {
    background: var(--panel);
    color: var(--text);
  }
  .tabs {
    display: flex;
    gap: 4px;
    margin-left: 14px;
  }
  .tabs button {
    padding: 4px 10px;
    border-radius: 7px;
    color: var(--muted);
  }
  .tabs button.on {
    background: var(--panel-2);
    color: var(--text);
    font-weight: 600;
  }
  header .btn + .btn {
    margin-left: 8px;
  }
  .cards {
    flex: 1;
    overflow-y: auto;
    min-height: 0;
  }
  .card-row {
    display: flex;
    align-items: center;
    gap: 11px;
    width: 100%;
    padding: 9px 18px;
    text-align: left;
  }
  .card-row:hover {
    background: var(--hover);
  }
  .card-row .num {
    display: block;
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
  }
  .card-row .name {
    display: block;
  }
  .badge {
    font-size: 11.5px;
    color: var(--accent);
    border: 1px solid var(--accent);
    border-radius: 9px;
    padding: 1px 7px;
    flex: none;
  }
  .deleted {
    padding: 14px 18px 24px;
  }
  .deleted h2 {
    font-size: 14px;
    margin: 0 0 6px;
  }
  .gone {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 10px;
    padding: 4px 0;
  }
  .empty {
    text-align: center;
    color: var(--muted);
    padding: 28px;
    margin: 0;
  }
  @media (max-width: 560px) {
    .narrow {
      width: 112px;
    }
  }
</style>
