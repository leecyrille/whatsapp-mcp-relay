<script lang="ts">
  import { onDestroy, onMount } from "svelte";
  import { api, errorText, fileUrl, type Message } from "../lib/api";
  import { fileSize } from "../lib/format";
  import { LIMITS, previewKind } from "../lib/preview";
  import { toast } from "../lib/state.svelte";

  let { msg, onclose }: { msg: Message; onclose: () => void } = $props();

  // The viewer runs in a sandbox of its own, served with rules that keep it away from the app. See media.rs.
  const VIEWER = "http://wamedia.localhost/viewer";
  const kind = $derived(previewKind(msg.filename, msg.mime));
  let frame = $state<HTMLIFrameElement>();
  let status = $state<"loading" | "shown" | "failed">("loading");
  let problem = $state("");
  let pendingLink = $state<string | null>(null);
  let bytes: ArrayBuffer | null = null;
  let ready = false;
  let sent = false;

  function theme(): "dark" | "light" {
    const set = document.documentElement.dataset.theme;
    if (set === "dark" || set === "light") return set;
    return matchMedia("(prefers-color-scheme: dark)").matches ? "dark" : "light";
  }

  function send() {
    if (sent || !ready || !bytes || !frame?.contentWindow) return;
    sent = true;
    // The bytes move to the viewer, which reads them; it never gets a way to fetch files itself.
    frame.contentWindow.postMessage({ type: "open", kind, name: msg.filename, bytes, theme: theme() }, "*", [bytes]);
    bytes = null;
  }

  function onMessage(e: MessageEvent) {
    if (!frame || e.source !== frame.contentWindow) return;
    const data = e.data as { type?: string; url?: string; message?: string };
    if (data?.type === "ready") {
      ready = true;
      send();
    } else if (data?.type === "first" || data?.type === "rendered") {
      status = "shown";
    } else if (data?.type === "error") {
      status = "failed";
      problem = String(data.message ?? "").slice(0, 300);
    } else if (data?.type === "close") {
      if (pendingLink) pendingLink = null;
      else onclose();
    } else if (data?.type === "link" && typeof data.url === "string") {
      pendingLink = data.url.slice(0, 2000);
    }
  }

  async function load() {
    if (!kind) return;
    if (msg.size > LIMITS[kind]) {
      status = "failed";
      problem = `It's ${fileSize(msg.size)}, which is more than this preview takes. Open it in its own app instead.`;
      return;
    }
    try {
      await api.mediaFetch(msg.rowid);
      const r = await fetch(fileUrl(msg.rowid));
      if (!r.ok) throw new Error(await r.text());
      const got = await r.arrayBuffer();
      if (got.byteLength > LIMITS[kind]) throw new Error("It's bigger than this preview takes. Open it in its own app instead.");
      bytes = got;
      send();
    } catch (e) {
      status = "failed";
      problem = errorText(e);
    }
  }

  async function openFile() {
    try {
      await api.mediaOpen(msg.rowid);
    } catch (e) {
      toast(errorText(e));
    }
  }

  async function save() {
    try {
      if (await api.mediaSave(msg.rowid)) toast("Saved.");
    } catch (e) {
      toast(errorText(e));
    }
  }

  async function followLink() {
    const url = pendingLink;
    pendingLink = null;
    if (!url) return;
    try {
      await api.openLink(url);
    } catch (e) {
      toast(errorText(e));
    }
  }

  onMount(() => {
    window.addEventListener("message", onMessage);
    void load();
  });
  onDestroy(() => window.removeEventListener("message", onMessage));
</script>

<svelte:window onkeydown={(e) => e.key === "Escape" && (pendingLink ? (pendingLink = null) : onclose())} />

<div class="scrim">
  <div class="box" role="dialog" aria-modal="true" aria-label="Preview of {msg.filename}">
    <header>
      <div class="title">
        <b>{msg.filename || "File"}</b>
        <span class="sub">{fileSize(msg.size)} · a preview. Nothing in the file runs.</span>
      </div>
      <button class="btn" onclick={openFile}>Open in default app</button>
      <button class="btn" onclick={save}>Save…</button>
      <button class="icon-btn" aria-label="Close preview" title="Close (Esc)" onclick={onclose}>×</button>
    </header>
    {#if status === "failed"}
      <div class="failed" role="alert">
        <p><b>Couldn't preview this file.</b> {problem}</p>
        <button class="btn primary" onclick={openFile}>Open in default app</button>
      </div>
    {:else}
      {#if status === "loading"}<p class="loading" role="status">Getting it ready…</p>{/if}
      <iframe
        bind:this={frame}
        class:hidden={status === "loading"}
        src={VIEWER}
        title="Preview of {msg.filename}"
        sandbox="allow-scripts"
        referrerpolicy="no-referrer"
      ></iframe>
    {/if}
    {#if pendingLink}
      <div class="link" role="alertdialog" aria-label="Open this link?">
        <p>The document links to:</p>
        <code>{pendingLink}</code>
        <p class="sub">It opens in your browser. Only open it if you trust where it goes.</p>
        <div class="actions">
          <button class="btn" onclick={() => (pendingLink = null)}>Cancel</button>
          <button class="btn primary" onclick={followLink}>Open link</button>
        </div>
      </div>
    {/if}
  </div>
</div>

<style>
  .scrim {
    position: fixed;
    inset: 0;
    z-index: 80;
    background: rgb(0 0 0 / 0.55);
    display: grid;
    place-items: center;
    padding: 14px;
  }
  .box {
    width: min(1180px, 100%);
    height: calc(100vh - 28px);
    background: var(--panel);
    color: var(--text);
    border-radius: 12px;
    box-shadow: var(--shadow);
    display: flex;
    flex-direction: column;
    overflow: hidden;
    position: relative;
  }
  header {
    display: flex;
    align-items: center;
    gap: 8px;
    padding: 10px 12px;
    border-bottom: 1px solid var(--line);
  }
  .title {
    flex: 1;
    min-width: 0;
    display: flex;
    flex-direction: column;
  }
  .title b {
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
  }
  .sub {
    color: var(--muted);
    font-size: 12.5px;
  }
  iframe {
    flex: 1;
    border: 0;
    width: 100%;
    background: var(--bg, #e9edef);
  }
  iframe.hidden {
    visibility: hidden;
  }
  .loading {
    position: absolute;
    top: 50%;
    width: 100%;
    text-align: center;
    color: var(--muted);
  }
  .failed {
    padding: 28px;
    text-align: center;
  }
  .link {
    position: absolute;
    left: 50%;
    top: 30%;
    transform: translateX(-50%);
    width: min(520px, calc(100% - 32px));
    background: var(--panel);
    border: 1px solid var(--line);
    border-radius: 12px;
    box-shadow: var(--shadow);
    padding: 16px 18px;
  }
  .link p {
    margin: 0 0 6px;
  }
  .link code {
    display: block;
    overflow-wrap: anywhere;
    background: var(--panel-2);
    border-radius: 6px;
    padding: 6px 8px;
    margin-bottom: 8px;
  }
  .actions {
    display: flex;
    justify-content: flex-end;
    gap: 8px;
  }
</style>
