<script lang="ts">
  import { api, errorText } from "../lib/api";
  import { toast, ui } from "../lib/state.svelte";

  let {
    jid,
    name,
    isGroup,
    person = $bindable(null),
  }: { jid: string; name: string; isGroup: boolean; person?: { jid: string; name: string } | null } = $props();

  let menu = $state<{ x: number; y: number } | null>(null);
  // What WhatsApp says about this group. Null while asking, and anything unsure offers nothing that changes it.
  let access = $state<{ can_change: boolean; why: string } | null>(null);
  let dialog = $state<"rename" | "leave" | null>(null);
  let draft = $state("");
  let busy = $state(false);
  let problem = $state("");

  const member = $derived(access !== null && access.why !== "not_member" && access.why !== "unknown");
  const left = $derived(access?.why === "not_member");

  function openMenu(e: MouseEvent) {
    e.stopPropagation();
    if (menu) {
      menu = null;
      return;
    }
    menu = { x: Math.min(e.clientX - 200, innerWidth - 250), y: e.clientY + 10 };
    if (isGroup) {
      access = null;
      api.groupPhotoAccess(jid).then(
        (answer) => (access = answer),
        () => (access = { can_change: false, why: "unknown" }),
      );
    }
  }

  $effect(() => {
    if (!menu) return;
    const close = () => (menu = null);
    window.addEventListener("click", close);
    return () => window.removeEventListener("click", close);
  });

  function show(which: "rename" | "leave") {
    menu = null;
    problem = "";
    if (which === "rename") draft = name;
    dialog = which;
  }

  // Right-clicking someone in a group's member list asks for this.
  $effect(() => {
    if (person) {
      problem = "";
      draft = person.name;
    }
  });

  function close() {
    if (busy) return;
    dialog = null;
    person = null;
    problem = "";
  }

  async function act(work: () => Promise<void>) {
    busy = true;
    problem = "";
    try {
      await work();
      dialog = null;
      person = null;
    } catch (e) {
      // Stays on screen, so a refusal can't be missed.
      problem = errorText(e);
    }
    busy = false;
  }

  const target = $derived(person ?? (!isGroup ? { jid, name } : null));
  const renamingGroup = $derived(dialog === "rename" && isGroup && !person);
  const renamingPerson = $derived(person !== null || (dialog === "rename" && !isGroup));

  const renameGroup = () =>
    act(async () => {
      const done = await api.whatsappGroupRename(jid, draft);
      toast(done.changed ? "Group renamed on WhatsApp." : "It already has that name.");
    });
  const leaveGroup = () =>
    act(async () => {
      await api.whatsappGroupLeave(jid);
      toast("You've left the group. Its messages stay here.");
    });
  const renamePerson = () =>
    act(async () => {
      await api.contactNameSet(target!.jid, draft);
      toast("Name changed in WA Relay, on all your computers.");
    });
  const resetPerson = () =>
    act(async () => {
      await api.contactNameRemove(target!.jid);
      toast("Back to their saved or WhatsApp name.");
    });
</script>

<button class="icon-btn more" title="Chat options" aria-label="Chat options for {name}" aria-haspopup="menu" onclick={openMenu}>
  <span class="dots" aria-hidden="true"><i></i><i></i><i></i></span>
</button>

{#if menu}
  <div class="menu" style:left="{menu.x}px" style:top="{menu.y}px" role="menu">
    {#if !isGroup}
      <button role="menuitem" onclick={() => { menu = null; ui.card = { jid, name }; }}>Contact card…</button>
      <button role="menuitem" onclick={() => show("rename")}>Rename contact…</button>
      <p>Only changes what WA Relay and the connector show. Nothing changes on WhatsApp or your phone.</p>
    {:else if access === null}
      <p>Checking what WhatsApp allows…</p>
    {:else}
      {#if access.can_change}
        <button role="menuitem" onclick={() => show("rename")}>Rename group…</button>
      {:else if access.why === "admins_only"}
        <p>Only this group's admins can rename it.</p>
      {/if}
      {#if member}
        <button role="menuitem" class="danger" onclick={() => show("leave")}>Exit group…</button>
      {:else if left}
        <p>You're no longer in this group. Its messages stay here.</p>
      {:else}
        <p>Couldn't tell what WhatsApp allows for this group right now, so nothing's offered. Try again in a minute.</p>
      {/if}
    {/if}
  </div>
{/if}

{#if renamingGroup}
  <div class="scrim">
    <div class="confirm" role="alertdialog" aria-modal="true" aria-labelledby="action-title">
      <form onsubmit={(e) => { e.preventDefault(); void renameGroup(); }}>
      <h3 id="action-title">Rename this group on WhatsApp?</h3>
      <p><b>Everyone in {name} will see the new name</b>, and WhatsApp may show them a notice that you changed it.</p>
      <!-- svelte-ignore a11y_autofocus -->
      <input aria-label="New group name" maxlength="100" bind:value={draft} autofocus />
      {#if problem}<p class="refused" role="alert">{problem}</p>{/if}
      <div class="actions">
        <button type="button" class="btn" onclick={close} disabled={busy}>{problem ? "Close" : "Cancel"}</button>
        <button type="submit" class="btn primary" disabled={busy || !draft.trim()}>{busy ? "Renaming…" : "Rename it for everyone"}</button>
      </div>
      </form>
    </div>
  </div>
{:else if dialog === "leave"}
  <div class="scrim">
    <div class="confirm" role="alertdialog" aria-modal="true" aria-labelledby="action-title">
      <h3 id="action-title">Exit {name}?</h3>
      <p><b>WhatsApp may tell the group you left</b>, and you can only come back if someone adds you. You stop getting its messages. What's already here stays.</p>
      {#if problem}<p class="refused" role="alert">{problem}</p>{/if}
      <div class="actions">
        <button class="btn" onclick={close} disabled={busy}>{problem ? "Close" : "Cancel"}</button>
        <button class="btn primary destructive" onclick={leaveGroup} disabled={busy}>{busy ? "Leaving…" : "Exit group"}</button>
      </div>
    </div>
  </div>
{:else if renamingPerson && target}
  <div class="scrim">
    <div class="confirm" role="alertdialog" aria-modal="true" aria-labelledby="action-title">
      <form onsubmit={(e) => { e.preventDefault(); void renamePerson(); }}>
      <h3 id="action-title">Rename {target.name}</h3>
      <p>Only changes what WA Relay and the connector show, on all your computers. Nothing changes on WhatsApp or your phone, and nobody else sees it.</p>
      <!-- svelte-ignore a11y_autofocus -->
      <input aria-label="New name" maxlength="80" bind:value={draft} autofocus />
      {#if problem}<p class="refused" role="alert">{problem}</p>{/if}
      <div class="actions">
        <button type="button" class="btn left" onclick={resetPerson} disabled={busy}>Use their own name</button>
        <button type="button" class="btn" onclick={() => { const who = target!; close(); ui.card = { jid: who.jid, name: who.name }; }} disabled={busy}>Contact card…</button>
        <button type="button" class="btn" onclick={close} disabled={busy}>{problem ? "Close" : "Cancel"}</button>
        <button type="submit" class="btn primary" disabled={busy || !draft.trim()}>{busy ? "Saving…" : "Save"}</button>
      </div>
      </form>
    </div>
  </div>
{/if}

<style>
  .more {
    flex: none;
  }
  .dots {
    display: inline-flex;
    flex-direction: column;
    gap: 2.5px;
  }
  .dots i {
    width: 3.5px;
    height: 3.5px;
    border-radius: 50%;
    background: currentColor;
  }
  .menu {
    position: fixed;
    z-index: 50;
    width: 240px;
    background: var(--panel);
    border: 1px solid var(--line);
    border-radius: 9px;
    box-shadow: var(--shadow);
    padding: 4px;
  }
  .menu button {
    display: flex;
    align-items: center;
    gap: 6px;
    width: 100%;
    padding: 7px 10px;
    border-radius: 6px;
    text-align: left;
    color: var(--text);
  }
  .menu button:hover {
    background: var(--hover);
  }
  .menu .danger {
    color: var(--danger);
  }
  .menu p {
    margin: 4px 10px 6px;
    font-size: 12px;
    color: var(--muted);
  }
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
    overflow-wrap: anywhere;
  }
  .confirm p {
    margin: 0 0 10px;
    overflow-wrap: anywhere;
  }
  .confirm input {
    width: 100%;
    padding: 7px 9px;
    margin-bottom: 10px;
    border: 1px solid var(--line);
    border-radius: 7px;
    background: var(--panel-2);
    color: var(--text);
    font: inherit;
  }
  .btn.destructive,
  .btn.destructive:hover {
    background: var(--danger);
    color: var(--panel);
  }
  .refused {
    color: var(--danger);
    font-weight: 600;
  }
  .actions {
    display: flex;
    flex-wrap: wrap;
    justify-content: flex-end;
    gap: 8px;
  }
  .actions .left {
    margin-right: auto;
  }
</style>
