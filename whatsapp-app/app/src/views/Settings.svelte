<script lang="ts">
  import { onMount } from "svelte";
  import { api, errorText, type Group } from "../lib/api";
  import { fileSize } from "../lib/format";
  import { setBlink, setTwoColumns, toast, ui } from "../lib/state.svelte";
  import Icon from "../lib/Icon.svelte";
  import { forgetAllUnsent, held } from "../lib/unsent.svelte";
  import UnsentList from "./UnsentList.svelte";

  let stats = $state<any>(null);
  let newGroup = $state("");
  let confirmErase = $state(false);
  let restarting = $state(false);

  // Changing the relay saves it and starts the app again on it.
  let relayAddress = $state("");
  let relayDraft = $state("");
  let relayProblem = $state("");
  let changingRelay = $state(false);
  onMount(() => {
    api.relayGet().then((r) => (relayAddress = r.address), () => {});
  });
  async function changeRelay(e: SubmitEvent) {
    e.preventDefault();
    relayProblem = "";
    try {
      await api.relaySet(relayDraft);
      changingRelay = false;
    } catch (err) {
      relayProblem = errorText(err);
    }
  }

  const updateText = $derived.by(() => {
    const u = ui.update;
    switch (u.state) {
      case "checking":
        return "Checking for updates…";
      case "up_to_date":
        return "This is the latest version.";
      case "downloading":
        return `Downloading version ${u.version}…`;
      case "ready":
        return ui.settings.auto_update ? `Version ${u.version} is ready.` : `Version ${u.version} is available.`;
      case "need_login":
        return "It will check again after you sign in.";
      case "offline":
        return "Couldn't reach your home PC. It will try again.";
      case "refused":
        return `Version ${u.version} failed its signature check, so it wasn't installed.`;
      case "failed":
        return "The update couldn't be installed. It will try again later.";
      default:
        return "";
    }
  });
  const updateBusy = $derived(ui.update.state === "checking" || ui.update.state === "downloading");

  async function checkNow() {
    try {
      await api.updateCheck();
    } catch (e) {
      toast(errorText(e));
    }
  }

  // What holds back an automatic update. Going ahead by hand is fine when all of it is kept through the restart.
  const holding = $derived(held().filter((h) => h.blocks));
  const allKept = $derived(holding.every((h) => h.kept));

  async function restartNow() {
    restarting = true;
    try {
      await api.updateInstall(holding.length > 0 && allKept);
    } catch (e) {
      toast(errorText(e));
    }
    restarting = false;
  }

  onMount(async () => {
    ui.settings = await api.settingsGet();
    stats = await api.stats();
  });

  async function set(key: string, value: unknown) {
    try {
      await api.settingsSet(key, value);
      ui.settings[key] = value;
    } catch (e) {
      toast(errorText(e));
    }
  }

  const SILENCE = [
    [15, "15 minutes"],
    [30, "30 minutes"],
    [60, "1 hour"],
    [120, "2 hours"],
    [180, "3 hours"],
    [240, "4 hours"],
  ] as const;

  async function silence(minutes: number) {
    try {
      await api.silenceSet(minutes);
    } catch (e) {
      toast(errorText(e));
    }
  }

  async function silenceOff() {
    try {
      await api.silenceClear();
    } catch (e) {
      toast(errorText(e));
    }
  }

  async function addGroup() {
    if (!newGroup.trim()) return;
    try {
      ui.groups = await api.groupCreate(newGroup.trim(), null);
      newGroup = "";
    } catch (e) {
      toast(errorText(e));
    }
  }

  async function rename(g: Group, name: string) {
    if (name.trim() === g.name) return;
    try {
      ui.groups = await api.groupUpdate(g.id, name, null, null);
    } catch (e) {
      toast(errorText(e));
      ui.groups = { ...ui.groups };
    }
  }

  async function recolor(g: Group, color: string) {
    try {
      ui.groups = await api.groupUpdate(g.id, null, color, null);
    } catch (e) {
      toast(errorText(e));
    }
  }

  async function remove(g: Group) {
    try {
      ui.groups = await api.groupDelete(g.id);
      if (ui.filter === g.id) ui.filter = "all";
      if (ui.peopleFilter === g.id) ui.peopleFilter = "all";
      if (ui.groupsFilter === g.id) ui.groupsFilter = "all";
    } catch (e) {
      toast(errorText(e));
    }
  }

  async function signOut(erase: boolean) {
    confirmErase = false;
    try {
      await api.signOut(erase);
      if (erase) forgetAllUnsent();
      ui.active = null;
      stats = await api.stats();
    } catch (e) {
      toast(errorText(e));
    }
  }
</script>

<section class="page">
  <div class="inner">
    <h1>Settings</h1>

    <h2>This computer</h2>
    <label class="row">
      <input type="checkbox" checked={ui.settings.autostart} onchange={(e) => set("autostart", e.currentTarget.checked)} />
      <span>Start with Windows, minimized to the tray</span>
    </label>
    <label class="row">
      <input type="checkbox" checked={ui.settings.notifications} onchange={(e) => set("notifications", e.currentTarget.checked)} />
      <span>Show notifications for new messages</span>
    </label>
    <div class="row silence" role="group" aria-label="Silence notifications">
      {#if ui.silenceUntil}
        <span>Notifications are silenced until <b>{new Date(ui.silenceUntil).toLocaleTimeString([], { hour: "numeric", minute: "2-digit" })}</b>.</span>
        <button class="btn" onclick={silenceOff}>Turn back on</button>
      {:else}
        <span>Silence notifications for</span>
        {#each SILENCE as [minutes, label]}
          <button class="btn" onclick={() => silence(minutes)}>{label}</button>
        {/each}
      {/if}
    </div>
    <p class="note muted">Messages and unread counts carry on. Notifications held back aren't shown afterward, and that includes Claude asking for approval.</p>
    <label class="row">
      <input type="checkbox" checked={ui.blink} onchange={(e) => setBlink(e.currentTarget.checked)} />
      <span>Make unread chats blink <small class="muted">Also the tray icon and taskbar button. Off, chats stay a steady amber, the tray icon stays on its unread picture and the button stays lit. The same happens if Windows is set to show fewer animations.</small></span>
    </label>
    <label class="row">
      <input type="checkbox" checked={ui.twoColumns} onchange={(e) => setTwoColumns(e.currentTarget.checked)} />
      <span>Show groups in their own column <small class="muted">People on the left, then groups, then the chat. A window under 900 px wide shows one list instead.</small></span>
    </label>
    <label class="row">
      <input type="checkbox" checked={ui.settings.auto_images} onchange={(e) => set("auto_images", e.currentTarget.checked)} />
      <span>Download photos automatically <small class="muted">Up to 15 MB each.</small></span>
    </label>
    <label class="row">
      <span>Keep at most</span>
      <select class="input small" value={String(ui.settings.media_cache_gb)} onchange={(e) => set("media_cache_gb", Number(e.currentTarget.value))}>
        {#each [1, 2, 5, 10, 20, 50] as gb}
          <option value={String(gb)}>{gb} GB</option>
        {/each}
      </select>
      <span>of photos, videos and files here</span>
    </label>

    <h2>Updates</h2>
    <label class="row">
      <input type="checkbox" checked={ui.settings.auto_update} onchange={(e) => set("auto_update", e.currentTarget.checked)} />
      <span>
        Install updates automatically
        <small class="muted">Updates download in the background either way. On, one installs once this window has been closed for a few minutes and nothing is in progress. Off, it waits here for you to install it.</small>
      </span>
    </label>
    <p class="muted update" role="status">Version {ui.update.current}. {updateText}</p>
    {#if ui.update.state === "ready" && holding.length > 0}
      <p class="note held" role="status">
        {holding.length === 1 ? "Something you wrote hasn't been sent" : `${holding.length} things you wrote haven't been sent`}, so the update waits.
        {allKept ? "Updating now keeps them: they'll be here afterwards." : "Send or discard the ones still sending or with a file first."}
      </p>
    {/if}
    <UnsentList />
    <div class="buttons">
      {#if ui.update.state === "ready"}
        <button class="btn primary" disabled={restarting} onclick={restartNow}>
          {restarting ? "Restarting…" : ui.settings.auto_update ? "Restart to update" : "Install update"}
        </button>
      {:else}
        <button class="btn" disabled={updateBusy} onclick={checkNow}>Check now</button>
      {/if}
    </div>

    <h2>Your account</h2>
    <div class="row relay-row">
      <span>
        Relay
        <small class="muted">{relayAddress || "Not set"}</small>
      </span>
      {#if changingRelay}
        <form class="relay-change" onsubmit={changeRelay}>
          <input class="input" aria-label="New relay address" bind:value={relayDraft} placeholder="https://wa.example.com" autocomplete="off" spellcheck="false" />
          <button class="btn primary" type="submit" disabled={!relayDraft.trim()}>Save and restart</button>
          <button class="btn" type="button" onclick={() => (changingRelay = false)}>Cancel</button>
        </form>
        {#if relayProblem}<small class="relay-problem" role="alert">{relayProblem}</small>{/if}
      {:else}
        <button class="btn" onclick={() => ((changingRelay = true), (relayDraft = relayAddress), (relayProblem = ""))}>Change…</button>
      {/if}
    </div>
    <label class="row">
      <input type="checkbox" checked={ui.settings.read_receipts} onchange={(e) => set("read_receipts", e.currentTarget.checked)} />
      <span>
        Send read receipts from this computer
        <small class="muted">People see blue ticks when you open their chat here. WhatsApp's own privacy setting still applies.</small>
      </span>
    </label>

    <h2>Your groups</h2>
    <ul class="groups">
      {#each ui.groups.groups as g (g.id)}
        <li>
          <input class="swatch" type="color" value={g.color} aria-label="Color for {g.name}" onchange={(e) => recolor(g, e.currentTarget.value)} />
          <input class="input" value={g.name} aria-label="Name of group {g.name}" onchange={(e) => rename(g, e.currentTarget.value)} />
          <span class="faint count">{Object.values(ui.groups.assign).filter((x) => x === g.id).length}</span>
          <button class="icon-btn" title="Delete group" aria-label="Delete group {g.name}" onclick={() => remove(g)}><Icon name="trash" size={16} /></button>
        </li>
      {/each}
    </ul>
    <form
      class="add"
      onsubmit={(e) => {
        e.preventDefault();
        void addGroup();
      }}
    >
      <input class="input" placeholder="New group" aria-label="New group name" bind:value={newGroup} />
      <button class="btn" disabled={!newGroup.trim()}>Add</button>
    </form>
    <p class="muted note">People and Groups at the top of the chat list are automatic, and can't be edited. A chat can also be in one of these. Deleting one of these only takes its chats out of it. They're saved on your home PC, so every computer shows the same ones.</p>

    <h2>Saved on this computer</h2>
    {#if stats}
      <p class="muted">
        {stats.messages.toLocaleString()} messages in {stats.chats.toLocaleString()} chats ({fileSize(stats.db_bytes)}), plus {fileSize(stats.media_bytes) || "0 B"} of media.
      </p>
    {/if}
    <div class="buttons">
      <button class="btn" onclick={() => signOut(false)}>Sign out</button>
      {#if confirmErase}
        <span class="confirm">Remove all saved chats and media from this computer?</span>
        <button class="btn danger" onclick={() => signOut(true)}>Yes, remove them</button>
        <button class="btn" onclick={() => (confirmErase = false)}>Keep them</button>
      {:else}
        <button class="btn danger" onclick={() => (confirmErase = true)}>Sign out and remove saved chats</button>
      {/if}
    </div>
    <p class="muted note">This only affects this computer. WhatsApp, your home PC and your other computers aren't changed.</p>
  </div>
</section>

<style>
  .page {
    flex: 1;
    min-width: 0;
    overflow-y: auto;
    background: var(--panel);
  }
  .inner {
    max-width: 620px;
    margin: 0 auto;
    padding: 18px 22px 40px;
  }
  h1 {
    font-size: 20px;
    margin: 0 0 8px;
  }
  h2 {
    font-size: 13px;
    text-transform: uppercase;
    letter-spacing: 0.04em;
    color: var(--muted);
    margin: 24px 0 6px;
  }
  .row {
    display: flex;
    gap: 9px;
    align-items: center;
    padding: 6px 0;
  }
  .row.silence {
    flex-wrap: wrap;
  }
  .relay-row {
    flex-wrap: wrap;
    justify-content: space-between;
  }
  .relay-row small {
    word-break: break-all;
  }
  .relay-change {
    display: flex;
    flex-wrap: wrap;
    gap: 6px;
    width: 100%;
  }
  .relay-change .input {
    flex: 1 1 220px;
  }
  .relay-problem {
    color: var(--danger);
    width: 100%;
  }
  .row small {
    display: block;
    font-size: 12.5px;
  }
  .small {
    width: 88px;
  }
  select option {
    background: var(--panel);
    color: var(--text);
  }
  .groups {
    list-style: none;
    margin: 0;
    padding: 0;
  }
  .groups li {
    display: flex;
    gap: 8px;
    align-items: center;
    padding: 4px 0;
  }
  .swatch {
    width: 34px;
    height: 32px;
    padding: 2px;
    border: 1px solid var(--line);
    border-radius: 7px;
    background: var(--panel-2);
    flex: none;
  }
  .count {
    width: 36px;
    text-align: right;
    flex: none;
  }
  .add {
    display: flex;
    gap: 8px;
    margin-top: 6px;
  }
  .update {
    margin: 4px 0 8px;
  }
  .note {
    font-size: 12.5px;
    margin: 8px 0 0;
  }
  .buttons {
    display: flex;
    flex-wrap: wrap;
    gap: 8px;
    align-items: center;
  }
  .confirm {
    width: 100%;
    color: var(--danger);
  }
</style>
