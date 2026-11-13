<script lang="ts">
  import { api, avatarUrl, errorText } from "./api";
  import { avatarColor, initials } from "./format";
  import { toast, ui } from "./state.svelte";
  import Icon from "./Icon.svelte";

  let {
    name,
    jid,
    isGroup = false,
    size = 42,
    editable = false,
    fill,
    ink,
  }: { name: string; jid: string; isGroup?: boolean; size?: number; editable?: boolean; fill?: string; ink?: string } = $props();

  // Square for a group and round for a person, wherever an avatar appears.
  const group = $derived(isGroup || jid.endsWith("@g.us"));
  const unnamed = $derived(name.startsWith("+") || name === "Unknown");
  const version = $derived(ui.pictures.versions[jid] ?? 0);
  const key = $derived(`${jid}:${version}`);
  // Many people hide their picture, so a failed load just leaves the initials showing.
  let failed = $state("");

  let menu = $state<{ x: number; y: number } | null>(null);
  const own = $derived(ui.pictures.own.includes(jid));

  // What WhatsApp says about this group's real photo. Null while asking, and anything unsure is a no.
  let access = $state<{ can_change: boolean; why: string } | null>(null);
  let confirming = $state<{ token: string; name: string } | null>(null);
  let photoBusy = $state(false);
  let photoError = $state("");
  const noRealPhoto = $derived(
    access?.why === "admins_only"
      ? "Only this group's admins can change its photo."
      : access?.why === "not_member"
        ? "You aren't a member of this group, so you can't change its photo."
        : "Couldn't tell if you may change this group's photo on WhatsApp, so only a WA Relay picture is offered.",
  );

  function openMenu(e: MouseEvent) {
    e.stopPropagation();
    if (menu) {
      menu = null;
      return;
    }
    menu = { x: Math.min(e.clientX, innerWidth - 260), y: Math.min(e.clientY + 8, innerHeight - (group ? 280 : 150)) };
    if (group) {
      access = null;
      api.groupPhotoAccess(jid).then(
        (answer) => (access = answer),
        () => (access = { can_change: false, why: "unknown" }),
      );
    }
  }

  // A chat shows many of these, so only an open menu listens for clicks.
  $effect(() => {
    if (!menu) return;
    const close = () => (menu = null);
    window.addEventListener("click", close);
    return () => window.removeEventListener("click", close);
  });

  async function chooseRealPhoto() {
    menu = null;
    photoError = "";
    try {
      const file = await api.pickPicture();
      if (file) confirming = { token: file.token, name: file.name };
    } catch (e) {
      toast(errorText(e));
    }
  }

  function cancelRealPhoto() {
    if (confirming) void api.forgetToken(confirming.token);
    confirming = null;
    photoError = "";
  }

  // A refusal stays on screen, and nothing falls back to a WA Relay-only picture.
  async function changeRealPhoto() {
    if (!confirming) return;
    photoBusy = true;
    photoError = "";
    try {
      const done = await api.groupPhotoSet(jid, confirming.token);
      confirming = null;
      toast(
        done.custom_showing
          ? "Changed on WhatsApp. Your own WA Relay picture is still showing here. Choose Go back to the WhatsApp picture to see the real one."
          : "Group photo changed on WhatsApp.",
      );
    } catch (e) {
      photoError = errorText(e);
    }
    photoBusy = false;
  }

  async function choose() {
    menu = null;
    try {
      const file = await api.pickPicture();
      if (file) {
        await api.pictureSet(jid, file.token);
        toast("Picture changed in WA Relay, on all your computers.");
      }
    } catch (e) {
      toast(errorText(e));
    }
  }

  async function remove() {
    menu = null;
    try {
      await api.pictureRemove(jid);
    } catch (e) {
      toast(errorText(e));
    }
  }
</script>

{#snippet face()}
  <div class="avatar" class:group style:width="{size}px" style:height="{size}px" style:background={fill ?? avatarColor(jid)} style:color={ink} style:font-size="{Math.round(size * 0.36)}px">
    {#if group}
      <Icon name="people" size={Math.round(size * 0.52)} />
    {:else if unnamed}
      <Icon name="person" size={Math.round(size * 0.52)} />
    {:else}
      {initials(name)}
    {/if}
    {#if jid && failed !== key}
      <img src={avatarUrl(jid, version)} alt="" loading="lazy" draggable="false" onerror={() => (failed = key)} />
    {/if}
  </div>
{/snippet}

{#if editable}
  <button class="edit" class:group title="Change picture" aria-label="Change picture for {name}" aria-haspopup="menu" onclick={openMenu}>
    {@render face()}
  </button>
  {#if menu}
    <div class="menu" style:left="{menu.x}px" style:top="{menu.y}px" role="menu">
      {#if group}
        <h4>On WhatsApp, for everyone</h4>
        {#if access === null}
          <p>Checking what WhatsApp allows…</p>
        {:else if access.can_change}
          <button role="menuitem" class="real" onclick={chooseRealPhoto}>Change real group photo…</button>
          <p>Everyone in the group sees this.</p>
        {:else}
          <p>{noRealPhoto}</p>
        {/if}
        <h4>Only in WA Relay, for you</h4>
      {/if}
      <button role="menuitem" onclick={choose}>Choose a picture…</button>
      {#if own}
        <button role="menuitem" onclick={remove}>Go back to the WhatsApp picture</button>
      {/if}
      <p>Only changes what WA Relay shows. Nothing changes on WhatsApp.</p>
    </div>
  {/if}
{:else}
  {@render face()}
{/if}

{#if confirming}
  <div class="scrim">
    <div class="confirm" role="alertdialog" aria-modal="true" aria-labelledby="real-photo-title">
      <h3 id="real-photo-title">Change this group's photo on WhatsApp?</h3>
      <p><b>Everyone in {name} will see the new photo</b>, and WhatsApp may show them a notice that you changed it.</p>
      <p class="file">{confirming.name}</p>
      {#if photoError}<p class="refused" role="alert">{photoError}</p>{/if}
      <div class="actions">
        <button class="btn" onclick={cancelRealPhoto} disabled={photoBusy}>{photoError ? "Close" : "Cancel"}</button>
        <button class="btn primary" onclick={changeRealPhoto} disabled={photoBusy}>{photoBusy ? "Changing…" : "Change it for everyone"}</button>
      </div>
    </div>
  </div>
{/if}

<style>
  .avatar {
    position: relative;
    overflow: hidden;
    border-radius: 50%;
    display: grid;
    place-items: center;
    color: #fff;
    font-weight: 600;
    flex: none;
  }
  .avatar.group,
  .edit.group {
    border-radius: 22%;
  }
  img {
    position: absolute;
    inset: 0;
    width: 100%;
    height: 100%;
    object-fit: cover;
  }
  .edit {
    flex: none;
    border-radius: 50%;
    display: block;
  }
  .edit:hover {
    outline: 2px solid var(--accent);
  }
  .menu {
    position: fixed;
    z-index: 50;
    width: 250px;
    background: var(--panel);
    border: 1px solid var(--line);
    border-radius: 9px;
    box-shadow: var(--shadow);
    padding: 4px;
    text-align: left;
  }
  .menu button {
    display: block;
    width: 100%;
    padding: 7px 10px;
    border-radius: 6px;
    text-align: left;
    color: var(--text);
  }
  .menu button:hover {
    background: var(--hover);
  }
  .menu h4 {
    margin: 6px 10px 2px;
    font-size: 11.5px;
    font-weight: 700;
    color: var(--muted);
  }
  .menu .real {
    font-weight: 600;
  }
  .scrim {
    position: fixed;
    inset: 0;
    z-index: 70;
    background: rgb(0 0 0 / 0.4);
    display: grid;
    place-items: center;
    padding: 16px;
    cursor: default;
  }
  .confirm {
    width: min(440px, 100%);
    background: var(--panel);
    color: var(--text);
    border-radius: 12px;
    box-shadow: var(--shadow);
    padding: 18px 20px;
    font-size: 14px;
    font-weight: 400;
    text-align: left;
    user-select: text;
  }
  .confirm h3 {
    margin: 0 0 8px;
    font-size: 16px;
  }
  .confirm p {
    margin: 0 0 10px;
    overflow-wrap: anywhere;
  }
  .confirm .file {
    color: var(--muted);
    font-size: 12.5px;
  }
  .confirm .refused {
    color: var(--danger);
    font-weight: 600;
  }
  .actions {
    display: flex;
    justify-content: flex-end;
    gap: 8px;
  }
  .menu p {
    margin: 4px 10px 6px;
    font-size: 12px;
    font-weight: 400;
    color: var(--muted);
  }
</style>
