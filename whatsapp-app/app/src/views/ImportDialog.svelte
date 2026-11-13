<script lang="ts">
  import { onMount } from "svelte";
  import { api, errorText, type ImportItem } from "../lib/api";
  import type { ParsedCard } from "../lib/vcard";
  import { toast, ui } from "../lib/state.svelte";

  let { cards, tooMany, onclose }: { cards: ParsedCard[]; tooMany: boolean; onclose: () => void } = $props();

  let items = $state<ImportItem[] | null>(null);
  let linkExact = $state(true);
  let busy = $state(false);
  let problem = $state("");
  // The user's choices: which single fields to replace on a match, and which unclear ones to add anyway.
  let take = $state<Record<number, string[]>>({});
  let addAnyway = $state<Record<number, boolean>>({});

  const FIELD: Record<string, string> = {
    display: "name shown", given: "first name", family: "last name", middle: "middle name", prefix: "prefix", suffix: "suffix",
    nickname: "nickname", company: "company", department: "department", title: "title", notes: "notes", phones: "numbers",
    emails: "emails", urls: "websites", dates: "dates", related: "related people", handles: "handles", addresses: "addresses",
  };

  const counts = $derived.by(() => {
    const c = { new: 0, merge: 0, ambiguous: 0, invalid: 0, conflicts: 0 };
    for (const it of items ?? []) {
      c[it.kind]++;
      c.conflicts += it.conflicts?.length ?? 0;
    }
    return c;
  });

  async function preview() {
    busy = true;
    problem = "";
    try {
      items = (await api.cardImport(cards, false, [], linkExact)).items;
    } catch (e) {
      problem = errorText(e);
    }
    busy = false;
  }

  function toggleTake(index: number, field: string, on: boolean) {
    const now = new Set(take[index] ?? []);
    if (on) now.add(field);
    else now.delete(field);
    take[index] = [...now];
  }

  async function apply() {
    busy = true;
    problem = "";
    const choices = (items ?? []).map((it) => ({
      index: it.index,
      action: it.kind === "ambiguous" ? (addAnyway[it.index] ? "add" : "skip") : it.kind === "invalid" ? "skip" : it.kind === "merge" ? "merge" : "add",
      take: take[it.index] ?? [],
    }));
    try {
      const done = await api.cardImport(cards, true, choices, linkExact);
      toast(`Imported: ${done.added ?? 0} new ${done.added === 1 ? "card" : "cards"}, ${done.updated ?? 0} updated.`);
      onclose();
    } catch (e) {
      problem = errorText(e);
      busy = false;
    }
  }

  const nameOf = (jid: string) => ui.chats.find((c) => c.jid === jid)?.name ?? `+${jid.split("@")[0]}`;

  onMount(preview);
</script>

<div class="scrim">
  <div class="dialog" role="dialog" aria-modal="true" aria-labelledby="import-title">
    <h3 id="import-title">Import contact cards</h3>
    <p class="note">Nothing is saved until you press Import. Matches are by exactly the same number or email, never by name alone. Existing details are only replaced where you tick it.</p>
    {#if tooMany}<p class="refused">That file has more than 5,000 cards. Only the first 5,000 are shown here.</p>{/if}
    <label class="check"><input type="checkbox" bind:checked={linkExact} onchange={preview} /> Link new cards to WhatsApp chats with exactly the same number</label>
    {#if items}
      <p class="summary" role="status">
        {counts.new} new, {counts.merge} matching a card you have{counts.conflicts ? ` (${counts.conflicts} differing ${counts.conflicts === 1 ? "detail" : "details"})` : ""}, {counts.ambiguous} unclear, {counts.invalid} unreadable.
      </p>
      <div class="list">
        {#each items as it (it.index)}
          <div class="item {it.kind}">
            <div class="title"><b>{it.display || "(no name)"}</b> <span class="tag">{it.kind === "new" ? "New" : it.kind === "merge" ? `Matches ${it.match_name}` : it.kind === "ambiguous" ? "Unclear" : "Can't import"}</span></div>
            {#if it.kind === "new" && it.links?.length}
              <div class="note">Will link to {it.links.map(nameOf).join(", ")}</div>
            {/if}
            {#if it.kind === "merge"}
              {#if it.adds?.length}<div class="note">Adds: {it.adds.map((f) => FIELD[f] ?? f).join(", ")}</div>{/if}
              {#if !it.adds?.length && !it.conflicts?.length}<div class="note">Nothing new.</div>{/if}
              {#each it.conflicts ?? [] as c (c.field)}
                <label class="check conflict">
                  <input type="checkbox" checked={(take[it.index] ?? []).includes(c.field)} onchange={(e) => toggleTake(it.index, c.field, e.currentTarget.checked)} />
                  <span>Replace {FIELD[c.field] ?? c.field} <s>{c.existing}</s> with <b>{c.incoming}</b></span>
                </label>
              {/each}
            {/if}
            {#if it.kind === "ambiguous"}
              <div class="note">Matches more than one card: {it.matches?.join(", ")}. Skipped unless you add it as a new card.</div>
              <label class="check"><input type="checkbox" bind:checked={addAnyway[it.index]} /> Add as a new card</label>
            {/if}
            {#if it.kind === "invalid"}<div class="refused">{it.error}</div>{/if}
          </div>
        {/each}
      </div>
    {:else if busy}
      <p class="note">Checking against your cards…</p>
    {/if}
    {#if problem}<p class="refused" role="alert">{problem}</p>{/if}
    <div class="actions">
      <button class="btn" onclick={onclose} disabled={busy}>Cancel</button>
      <button class="btn primary" onclick={apply} disabled={busy || !items || counts.new + counts.merge + counts.ambiguous === 0}>{busy ? "Working…" : "Import"}</button>
    </div>
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
    gap: 8px;
  }
  h3 {
    margin: 0;
    font-size: 17px;
  }
  .note {
    color: var(--muted);
    font-size: 12.5px;
    margin: 0;
  }
  .summary {
    margin: 0;
    font-weight: 600;
  }
  .list {
    overflow-y: auto;
    min-height: 0;
    border: 1px solid var(--line);
    border-radius: 9px;
    padding: 4px 10px;
  }
  .item {
    padding: 7px 0;
    border-bottom: 1px solid var(--line);
  }
  .item:last-child {
    border-bottom: none;
  }
  .title b {
    overflow-wrap: anywhere;
  }
  .tag {
    font-size: 12px;
    color: var(--muted);
    margin-left: 6px;
  }
  .check {
    display: flex;
    gap: 7px;
    align-items: flex-start;
    font-size: 13px;
  }
  .conflict span {
    overflow-wrap: anywhere;
  }
  .refused {
    color: var(--danger);
    font-weight: 600;
    margin: 0;
  }
  .actions {
    display: flex;
    justify-content: flex-end;
    gap: 8px;
  }
</style>
