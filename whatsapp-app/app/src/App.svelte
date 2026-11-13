<script lang="ts">
  import { onMount } from "svelte";
  import { api, errorText } from "./lib/api";
  import { lastUpdated } from "./lib/format";
  import { init, on, toast, ui } from "./lib/state.svelte";
  import Icon from "./lib/Icon.svelte";
  import Sidebar from "./views/Sidebar.svelte";
  import Conversation from "./views/Conversation.svelte";
  import Contacts from "./views/Contacts.svelte";
  import ClaudePanel from "./views/ClaudePanel.svelte";
  import Settings from "./views/Settings.svelte";
  import ContactCard from "./views/ContactCard.svelte";

  // The first run of a copy with no relay built in asks where its relay is, before anything else.
  let relay = $state<{ address: string; configured: boolean } | null>(null);
  let relayDraft = $state("");
  let relayProblem = $state("");
  let relaySaving = $state(false);
  async function saveRelay(e: SubmitEvent) {
    e.preventDefault();
    relaySaving = true;
    relayProblem = "";
    try {
      await api.relaySet(relayDraft);
    } catch (err) {
      relayProblem = errorText(err);
    }
    relaySaving = false;
  }

  onMount(() => {
    api.relayGet().then((r) => (relay = r), () => (relay = { address: "", configured: true }));
    void init();
    return on("claude-event", (ev) => {
      if (ev.type === "claude" && ev.kind === "item" && ev.item.kind === "approval") {
        void refreshApprovals();
      }
    });
  });

  async function refreshApprovals() {
    try {
      const { threads } = await api.claude("GET", "/threads");
      let waiting = 0;
      for (const t of threads.filter((x: any) => x.running)) {
        const full = await api.claude("GET", `/threads/${t.id}`);
        waiting += (full.items ?? []).filter((i: any) => i.kind === "approval" && i.state === "pending").length;
      }
      ui.approvalsWaiting = waiting;
    } catch {
      /* The panel shows why Claude is unreachable. */
    }
  }

  async function signIn() {
    try {
      await api.openLogin();
    } catch (e) {
      toast(errorText(e));
    }
  }

  const WHY: Record<string, string> = {
    login: "You're signed out, so nothing new can arrive.",
    no_internet: "This computer can't get online.",
    home_down: "Can't reach your home PC. It may be off or asleep, or its tunnel may be down.",
    relay_down: "Your home PC is on, but WA Relay isn't running on it.",
    bridge_down: "Your home PC is on, but its WhatsApp service isn't running.",
    unlinked: "WhatsApp isn't linked on your home PC. Re-scan with Pair-WhatsApp.cmd there.",
    wa_down: "Your home PC is on, but it can't reach WhatsApp right now.",
  };
  const banner = $derived.by(() => {
    const c = ui.conn;
    if (c.link === "catching_up") return { kind: "syncing", why: "Catching up on new messages.", detail: "" };
    const why = WHY[c.link];
    if (!why) return null;
    const when = c.last_updated_ms ? `Chats may be out of date: last updated ${lastUpdated(c.last_updated_ms)}.` : "Nothing has synced to this computer yet.";
    return { kind: c.link === "login" ? "login" : "down", why, detail: `${when} Saved chats and search still work. Messages can't be sent.` };
  });
  const current = $derived(ui.conn.link === "ok");

  const firstRun = $derived(ui.loaded && ui.conn.need_login && ui.chats.length === 0);
  const unread = $derived(ui.chats.reduce((sum, c) => sum + c.unread, 0));
  const syncPct = $derived(ui.syncHead ? Math.min(100, Math.round((ui.syncDone / ui.syncHead) * 100)) : 0);

  function onKey(e: KeyboardEvent) {
    if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === "k") {
      e.preventDefault();
      ui.view = "chats";
      requestAnimationFrame(() => document.getElementById("search")?.focus());
    }
  }

  const tabs = [
    { id: "chats", label: "Chats", icon: "chats" },
    { id: "contacts", label: "Contacts", icon: "contacts" },
    { id: "claude", label: "Claude", icon: "claude" },
  ] as const;
</script>

<svelte:window onkeydown={onKey} />

{#if relay && !relay.configured}
  <main class="gate">
    <form class="gate-card" onsubmit={saveRelay}>
      <div class="logo"><Icon name="chats" size={30} /></div>
      <h1>WA Relay</h1>
      <p class="muted">Where is your relay? It's the web address your home PC's WA Relay is published at.</p>
      <label class="relay-field">
        <span>Relay address</span>
        <input class="input" bind:value={relayDraft} placeholder="https://wa.example.com" autocomplete="off" spellcheck="false" aria-invalid={!!relayProblem} />
      </label>
      {#if relayProblem}<p class="relay-problem" role="alert">{relayProblem}</p>{/if}
      <button class="btn primary big" type="submit" disabled={relaySaving || !relayDraft.trim()}>Continue</button>
    </form>
  </main>
{:else if firstRun}
  <main class="gate">
    <div class="gate-card">
      <div class="logo"><Icon name="chats" size={30} /></div>
      <h1>WA Relay</h1>
      <p class="muted">Your WhatsApp, through your home PC. Sign in once on each computer.</p>
      <button class="btn primary big" onclick={signIn}>Sign in</button>
    </div>
  </main>
{:else}
  <div class="shell" class:in-chat={!!ui.active && ui.view === "chats"}>
    <nav class="rail" aria-label="Sections">
      {#each tabs as tab}
        <button
          class="rail-btn"
          class:on={ui.view === tab.id}
          onclick={() => (ui.view = tab.id)}
          aria-label={tab.label}
          aria-current={ui.view === tab.id ? "page" : undefined}
          title={tab.label}
        >
          <Icon name={tab.icon} size={22} />
          {#if tab.id === "chats" && unread > 0}
            <span class="badge">{unread > 99 ? "99+" : unread}</span>
          {/if}
          {#if tab.id === "claude" && ui.approvalsWaiting > 0}
            <span class="badge warn">{ui.approvalsWaiting}</span>
          {/if}
        </button>
      {/each}
      <div class="grow"></div>
      <span class="status-dot" class:good={current} title={current ? "Connected and up to date" : "Not up to date"}></span>
      <button
        class="rail-btn"
        class:on={ui.view === "settings"}
        onclick={() => (ui.view = "settings")}
        aria-label="Settings"
        title="Settings"
      >
        <Icon name="settings" size={22} />
        {#if ui.update.state === "ready"}
          <span class="dot" title="Update ready"></span>
        {/if}
      </button>
    </nav>

    <div class="stage">
      {#if banner}
        <div class="banner" class:down={banner.kind !== "syncing"} role={banner.kind === "syncing" ? "status" : "alert"}>
          <Icon name="wifi" size={16} />
          <span><b>{banner.why}</b> {banner.detail}</span>
          {#if banner.kind === "login"}
            <button class="link" onclick={signIn}>Sign in</button>
          {/if}
        </div>
      {/if}
      {#if ui.syncing}
        <div class="progress" role="progressbar" aria-valuenow={syncPct} aria-valuemin="0" aria-valuemax="100" aria-label="Syncing chats">
          <div style:width="{syncPct}%"></div>
        </div>
      {/if}
      <div class="content">
        {#if ui.view === "chats"}
          <Sidebar />
          <Conversation />
        {:else if ui.view === "contacts"}
          <Contacts />
        {:else if ui.view === "claude"}
          <ClaudePanel />
        {:else}
          <Settings />
        {/if}
      </div>
    </div>
  </div>
{/if}

{#if ui.card}
  <ContactCard open={ui.card} onclose={() => (ui.card = null)} />
{/if}

{#if ui.toast}
  <div class="toast" role="alert">{ui.toast}</div>
{/if}

<style>
  .shell {
    display: flex;
    height: 100%;
  }
  .rail {
    width: 56px;
    background: var(--rail);
    border-right: 1px solid var(--line);
    display: flex;
    flex-direction: column;
    align-items: center;
    padding: 8px 0;
    gap: 4px;
    flex: none;
  }
  .grow {
    flex: 1;
  }
  .rail-btn {
    width: 42px;
    height: 42px;
    border-radius: 10px;
    display: grid;
    place-items: center;
    color: var(--muted);
    position: relative;
  }
  .rail-btn:hover {
    background: var(--hover);
    color: var(--text);
  }
  .rail-btn.on {
    background: var(--active);
    color: var(--accent);
  }
  .badge {
    position: absolute;
    top: 2px;
    right: 1px;
    min-width: 17px;
    height: 17px;
    padding: 0 4px;
    border-radius: 9px;
    background: var(--accent);
    color: var(--accent-text);
    font-size: 10.5px;
    font-weight: 700;
    display: grid;
    place-items: center;
  }
  .dot {
    position: absolute;
    top: 7px;
    right: 7px;
    width: 8px;
    height: 8px;
    border-radius: 50%;
    background: var(--accent);
  }
  .badge.warn {
    background: #d97706;
    color: #fff;
  }
  .status-dot {
    width: 8px;
    height: 8px;
    border-radius: 50%;
    background: var(--danger);
    margin-bottom: 6px;
  }
  .status-dot.good {
    background: var(--accent);
  }
  .stage {
    flex: 1;
    min-width: 0;
    display: flex;
    flex-direction: column;
  }
  .content {
    flex: 1;
    min-height: 0;
    display: flex;
  }
  .banner {
    display: flex;
    align-items: center;
    gap: 8px;
    background: var(--warn-bg);
    color: var(--warn-text);
    padding: 7px 14px;
    font-size: 13px;
    border-bottom: 1px solid var(--line);
  }
  .banner.down {
    background: var(--danger);
    color: #fff;
  }
  .banner .link {
    margin-left: auto;
    font-weight: 600;
    text-decoration: underline;
    white-space: nowrap;
  }
  .progress {
    height: 3px;
    background: var(--line);
  }
  .progress > div {
    height: 100%;
    background: var(--accent);
    transition: width 0.2s;
  }
  .gate {
    height: 100%;
    display: grid;
    place-items: center;
    padding: 24px;
  }
  .gate-card {
    text-align: center;
    max-width: 340px;
  }
  .gate-card h1 {
    font-size: 24px;
    margin: 12px 0 6px;
  }
  .gate-card p {
    margin: 0 0 20px;
  }
  .relay-field {
    display: grid;
    gap: 6px;
    text-align: left;
    margin-bottom: 14px;
    font-weight: 600;
    font-size: 13px;
  }
  .gate-card .relay-problem {
    color: var(--danger);
    margin: -6px 0 14px;
    text-align: left;
    font-size: 13px;
  }
  .logo {
    width: 64px;
    height: 64px;
    border-radius: 18px;
    background: var(--accent);
    color: var(--accent-text);
    display: inline-grid;
    place-items: center;
  }
  .big {
    padding: 10px 28px;
    font-size: 15px;
  }
  .toast {
    position: fixed;
    left: 50%;
    bottom: 22px;
    transform: translateX(-50%);
    background: var(--text);
    color: var(--panel);
    padding: 9px 16px;
    border-radius: 9px;
    box-shadow: var(--shadow);
    max-width: min(560px, calc(100vw - 32px));
    z-index: 60;
  }
</style>
