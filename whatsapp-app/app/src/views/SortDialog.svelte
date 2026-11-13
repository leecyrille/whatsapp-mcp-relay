<script lang="ts">
  import { onDestroy, onMount } from "svelte";
  import { api, errorText, type Proposal, type SortJob } from "../lib/api";
  import { on, toast, ui } from "../lib/state.svelte";
  import Icon from "../lib/Icon.svelte";

  let { onclose }: { onclose: () => void } = $props();

  let includeSnippets = $state(true);
  let onlyUnsorted = $state(true);
  let includeContacts = $state(false);
  let stage = $state<"choose" | "running" | "review" | "failed">("choose");
  let error = $state("");
  let job = $state<SortJob | null>(null);
  let chosen = $state<Record<string, boolean>>({});
  let edits = $state<Record<string, string>>({});
  let applying = $state(false);
  let contactNames = $state<Record<string, string>>({});

  const chatName = (jid: string) => ui.chats.find((c) => c.jid === jid)?.name ?? contactNames[jid] ?? jid;
  const groupName = (id: string) => ui.groups.groups.find((g) => g.id === id)?.name ?? id;

  const proposals = $derived(job?.proposals ?? []);
  const selectedCount = $derived(proposals.filter((p) => chosen[p.jid]).length);

  async function start() {
    stage = "running";
    error = "";
    try {
      job = await api.sortStart(includeSnippets, onlyUnsorted, includeContacts);
      if (job.state !== "running") absorb(job);
    } catch (e) {
      stage = "failed";
      error = errorText(e);
    }
  }

  async function absorb(result: SortJob) {
    job = result;
    if (result.state === "failed") {
      stage = "failed";
      error = result.error ?? "Claude couldn't sort these.";
      return;
    }
    if (result.state === "done") {
      if (includeContacts) {
        const list = await api.contacts();
        contactNames = Object.fromEntries(list.map((c) => [c.jid, c.name]));
      }
      const next: Record<string, boolean> = {};
      const nextEdits: Record<string, string> = {};
      for (const p of result.proposals ?? []) {
        next[p.jid] = p.confidence >= 0.6;
        nextEdits[p.jid] = p.group;
      }
      chosen = next;
      edits = nextEdits;
      stage = "review";
    }
  }

  async function apply() {
    applying = true;
    try {
      const byGroup = new Map<string, string[]>();
      for (const p of proposals) {
        if (!chosen[p.jid]) continue;
        const group = edits[p.jid] ?? p.group;
        byGroup.set(group, [...(byGroup.get(group) ?? []), p.jid]);
      }
      for (const [group, jids] of byGroup) {
        ui.groups = await api.assign(jids, group);
      }
      toast(`Sorted ${selectedCount} ${selectedCount === 1 ? "chat" : "chats"}.`);
      if (job) void api.claude("POST", `/sort/${job.id}/dismiss`).catch(() => {});
      onclose();
    } catch (e) {
      toast(errorText(e));
    } finally {
      applying = false;
    }
  }

  let off: () => void;
  onMount(() => {
    off = on("claude-event", (ev) => {
      if (ev.type === "claude_sort" && ev.job.id === job?.id) void absorb(ev.job);
    });
  });
  onDestroy(() => off?.());

  function bandOf(p: Proposal) {
    return p.confidence >= 0.8 ? "Sure" : p.confidence >= 0.6 ? "Likely" : "Unsure";
  }
</script>

<svelte:window onkeydown={(e) => e.key === "Escape" && stage !== "running" && onclose()} />

<div class="modal-back" role="dialog" aria-modal="true" aria-label="Sort with Claude">
  <div class="modal">
    <h2>Sort with Claude</h2>
    {#if stage === "choose"}
      <div class="body">
        <p class="muted intro">
          Claude suggests a group for each chat, using Claude on your home PC. You review every suggestion. Nothing moves until you accept it.
        </p>
        <label class="opt">
          <input type="checkbox" bind:checked={includeSnippets} />
          <span>
            Include the last few messages of each chat
            <small class="muted">Better guesses. Up to 5 short lines per chat go to Claude. Otherwise only names are sent.</small>
          </span>
        </label>
        <label class="opt">
          <input type="checkbox" bind:checked={onlyUnsorted} />
          <span>Only chats that aren't sorted yet</span>
        </label>
        <label class="opt">
          <input type="checkbox" bind:checked={includeContacts} />
          <span>Include contacts you haven't chatted with <small class="muted">Names only.</small></span>
        </label>
      </div>
      <div class="actions">
        <button class="btn" onclick={onclose}>Cancel</button>
        <button class="btn primary" onclick={start}><Icon name="sparkle" size={14} /> Ask Claude</button>
      </div>
    {:else if stage === "running"}
      <div class="body center">
        <div class="spin" aria-hidden="true"></div>
        <p>Claude is sorting {job?.items ?? ""} {job?.items === 1 ? "chat" : "chats"}. This can take a minute or two.</p>
      </div>
    {:else if stage === "failed"}
      <div class="body"><p class="err" role="alert">{error}</p></div>
      <div class="actions">
        <button class="btn" onclick={onclose}>Close</button>
        <button class="btn primary" onclick={() => (stage = "choose")}>Try again</button>
      </div>
    {:else}
      <div class="body">
        {#if proposals.length === 0}
          <p class="muted">Claude couldn't confidently place any of these.</p>
        {:else}
          <p class="muted intro">
            {proposals.length} suggestions{#if job?.cost}, about ${job.cost.toFixed(2)} of Claude usage{/if}. Unsure ones start unticked. You can change any group.
          </p>
          <ul class="props">
            {#each proposals as p (p.jid)}
              <li>
                <input type="checkbox" bind:checked={chosen[p.jid]} aria-label="Accept for {chatName(p.jid)}" />
                <div class="prop-main">
                  <div class="prop-name">{chatName(p.jid)}</div>
                  <div class="prop-why muted">{bandOf(p)}: {p.reason}</div>
                </div>
                <select class="input" bind:value={edits[p.jid]} aria-label="Group for {chatName(p.jid)}">
                  {#each ui.groups.groups as g (g.id)}
                    <option value={g.id}>{g.name}</option>
                  {/each}
                </select>
              </li>
            {/each}
          </ul>
        {/if}
      </div>
      <div class="actions">
        <button class="btn" onclick={onclose}>Discard</button>
        <button class="btn primary" onclick={apply} disabled={selectedCount === 0 || applying}>
          Accept {selectedCount}
        </button>
      </div>
    {/if}
  </div>
</div>

<style>
  .intro {
    margin: 0 0 12px;
  }
  .opt {
    display: flex;
    gap: 9px;
    padding: 8px 0;
    align-items: flex-start;
  }
  .opt input {
    margin-top: 3px;
  }
  .opt small {
    display: block;
    font-size: 12.5px;
  }
  .center {
    text-align: center;
    padding: 24px 18px 26px;
  }
  .spin {
    width: 30px;
    height: 30px;
    border-radius: 50%;
    border: 3px solid var(--line);
    border-top-color: var(--accent);
    margin: 0 auto 12px;
    animation: spin 0.9s linear infinite;
  }
  @keyframes spin {
    to {
      transform: rotate(360deg);
    }
  }
  .err {
    color: var(--danger);
  }
  .props {
    list-style: none;
    margin: 0;
    padding: 0;
  }
  .props li {
    display: flex;
    align-items: center;
    gap: 10px;
    padding: 7px 0;
    border-top: 1px solid var(--line);
  }
  .prop-main {
    flex: 1;
    min-width: 0;
  }
  .prop-name {
    font-weight: 600;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .prop-why {
    font-size: 12.5px;
  }
  .props select {
    width: 124px;
    flex: none;
  }
  .props select option {
    background: var(--panel);
    color: var(--text);
  }
  .actions .btn {
    display: inline-flex;
    gap: 6px;
    align-items: center;
  }
</style>
