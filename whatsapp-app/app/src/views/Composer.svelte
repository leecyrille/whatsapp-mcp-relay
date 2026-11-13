<script lang="ts" module>
  export type { Outgoing } from "../lib/unsent.svelte";
  let lists = 0;
</script>

<script lang="ts">
  import { onDestroy, onMount, tick, untrack } from "svelte";
  import { api, errorText, localUrl, type MemberList, type Message, type Picked } from "../lib/api";
  import { fileSize } from "../lib/format";
  import { activeQuery, findMentions, labelFor, matching, mentionAt, toWire, type Candidate, type Pick } from "../lib/mentions";
  import { on, setBusy, toast } from "../lib/state.svelte";
  import { setDraft, unsent } from "../lib/unsent.svelte";
  import Avatar from "../lib/Avatar.svelte";
  import Icon from "../lib/Icon.svelte";

  let {
    chat,
    replyTo,
    oncancelreply,
    onsend,
    onfile,
    isGroup = false,
    members = [],
    membersStale = false,
    editing = null,
    oncanceledit = () => {},
    onedit = () => {},
  }: {
    chat: string;
    replyTo: Message | null;
    oncancelreply: () => void;
    /** text and mentions are what's sent; shown is what the user typed, with names. */
    onsend: (text: string, mentions: string[], shown: string) => void;
    onfile: (
      file: Picked,
      caption: string,
      kind: string,
      thumb: string | null,
      width: number,
      height: number,
      seconds: number,
      mentions: string[],
      shown: string,
    ) => void;
    isGroup?: boolean;
    members?: MemberList["members"];
    membersStale?: boolean;
    /** A message of the user's being edited. The box holds its text until they save or cancel. */
    editing?: Message | null;
    oncanceledit?: () => void;
    onedit?: (m: Message, text: string, mentions: string[]) => void;
  } = $props();

  interface Attachment extends Picked {
    thumb: string | null;
    width: number;
    height: number;
    seconds: number;
    preview: boolean;
  }

  // The parent recreates this component per chat, so the draft is only read once.
  let text = $state(untrack(() => unsent.drafts[chat]?.text ?? ""));
  let area = $state<HTMLTextAreaElement>();
  let attached = $state<Attachment[]>([]);
  let asFile = $state(false);
  const canSend = $derived(text.trim().length > 0 || attached.length > 0);

  // @mentions. The box shows "@Name"; toWire turns that into what WhatsApp needs when it's sent.
  const listId = `mention-list-${++lists}`;
  let picks = $state<Pick[]>(untrack(() => [...(unsent.drafts[chat]?.picks ?? [])]));
  let query = $state<{ start: number; query: string } | null>(null);
  let selected = $state(0);
  let dismissedAt = -1;
  let listEl = $state<HTMLDivElement>();
  const candidates = $derived<Candidate[]>(members.filter((m) => !m.you).map((m) => ({ jid: m.jid, person: m.person, name: m.name })));
  const found = $derived(query ? matching(candidates, query.query).slice(0, 200) : []);
  const pickerOpen = $derived(!!query && isGroup && (found.length > 0 || candidates.length === 0));
  const mentioning = $derived([...new Set(findMentions(text, picks).map((m) => m.pick.label))]);

  function updatePicker() {
    if (!area || !isGroup) {
      query = null;
      return;
    }
    const caret = area.selectionStart ?? text.length;
    let q = area.selectionEnd === caret ? activeQuery(text, caret) : null;
    // A mention already picked isn't a search, or Enter after it would pick again instead of sending.
    if (q && findMentions(text, picks).some((m) => m.start === q?.start)) q = null;
    if (!q) dismissedAt = -1;
    if (!q || q.start === dismissedAt) {
      query = null;
      return;
    }
    if (!query || query.start !== q.start || query.query !== q.query) selected = 0;
    query = q;
  }

  // Each chat keeps its unsent text, and who its @names are, while the user looks at another one, and
  // through a restart. Settings lists them all.
  function saveDraft() {
    setDraft(chat, text, $state.snapshot(picks));
  }

  async function choose(person: Candidate) {
    if (!query || !area) return;
    const caret = area.selectionStart ?? text.length;
    const label = labelFor(person.name, person.jid, picks);
    const after = text.slice(caret);
    const insert = `@${label}${after.startsWith(" ") ? "" : " "}`;
    text = text.slice(0, query.start) + insert + after;
    if (!picks.some((p) => p.label === label)) picks = [...picks, { label, jid: person.jid }];
    const at = query.start + insert.length + (after.startsWith(" ") ? 1 : 0);
    query = null;
    saveDraft();
    await tick();
    area.focus();
    area.setSelectionRange(at, at);
    grow();
  }

  async function moveSelection(by: number) {
    selected = (selected + by + found.length) % found.length;
    await tick();
    listEl?.querySelector(`#${listId}-${selected}`)?.scrollIntoView({ block: "nearest" });
  }

  async function removeWhole(start: number, end: number) {
    text = text.slice(0, start) + text.slice(end);
    saveDraft();
    await tick();
    area?.setSelectionRange(start, start);
    grow();
    updatePicker();
  }

  // Attachments only live in memory, so an automatic update waits for them.
  $effect(() => setBusy("attachments", attached.length > 0));
  onDestroy(() => setBusy("attachments", false));

  function grow() {
    if (!area) return;
    area.style.height = "auto";
    area.style.height = `${Math.min(area.scrollHeight, 180)}px`;
  }

  // Editing swaps the draft out, and puts it back afterwards.
  let stashed: { text: string; picks: Pick[] } | null = null;
  let editingId = "";
  $effect(() => {
    const m = editing;
    untrack(() => {
      if (m && m.id !== editingId) {
        if (!editingId) stashed = { text, picks };
        editingId = m.id;
        const fresh = fromMessage(m);
        text = fresh.text;
        picks = fresh.picks;
        query = null;
        void tick().then(() => {
          area?.focus();
          area?.setSelectionRange(text.length, text.length);
          grow();
        });
      } else if (!m && editingId) {
        editingId = "";
        text = stashed?.text ?? "";
        picks = stashed?.picks ?? [];
        stashed = null;
        void tick().then(grow);
      }
    });
  });

  /** A sent message's "@digits" back to "@Name", for the people the app can match to this group's members. */
  function fromMessage(m: Message): { text: string; picks: Pick[] } {
    const found: Pick[] = [];
    const out = m.text.replace(/(?<![\p{L}\p{N}])@(\d+)(?![\p{L}\p{N}])/gu, (all, digits: string) => {
      const member = members.find((x) => [x.jid, x.lid, x.person].some((a) => a && a.split("@")[0] === digits));
      if (!member) return all;
      const label = labelFor(m.mentions?.[digits] ?? member.name, member.jid, found);
      if (!found.some((p) => p.label === label)) found.push({ label, jid: member.jid });
      return `@${label}`;
    });
    return { text: out, picks: found };
  }

  function onInput() {
    if (editing) {
      grow();
      updatePicker();
      return;
    }
    saveDraft();
    grow();
    updatePicker();
  }

  function submit() {
    if (editing) {
      const edited = text.trim();
      if (!edited) return;
      const wire = toWire(edited, isGroup ? picks : []);
      onedit(editing, wire.text, wire.mentions);
      return;
    }
    if (!canSend) return;
    const caption = text.trim();
    // Worked out from the final text, so only names still whole in it are mentioned.
    const wire = toWire(caption, isGroup ? picks : []);
    if (attached.length > 0) {
      attached.forEach((file, i) => {
        const kind = asFile ? "document" : file.kind;
        const first = i === 0;
        onfile(file, first ? wire.text : "", kind, file.thumb, file.width, file.height, file.seconds, first ? wire.mentions : [], first ? caption : "");
      });
      attached = [];
      asFile = false;
    } else {
      onsend(wire.text, wire.mentions, caption);
    }
    text = "";
    picks = [];
    query = null;
    setDraft(chat, "", []);
    requestAnimationFrame(grow);
    area?.focus();
  }

  function onKey(e: KeyboardEvent) {
    if (pickerOpen && !e.isComposing) {
      if (found.length && (e.key === "ArrowDown" || e.key === "ArrowUp")) {
        e.preventDefault();
        void moveSelection(e.key === "ArrowDown" ? 1 : -1);
        return;
      }
      if (found.length && ((e.key === "Enter" && !e.shiftKey) || e.key === "Tab")) {
        e.preventDefault();
        void choose(found[selected]);
        return;
      }
      if (e.key === "Escape") {
        e.preventDefault();
        e.stopPropagation();
        dismissedAt = query?.start ?? -1;
        query = null;
        return;
      }
    }
    if ((e.key === "Backspace" || e.key === "Delete") && area && area.selectionStart === area.selectionEnd) {
      // A mention goes in one press, as on the phone, so half a name is never left looking like one.
      const whole = mentionAt(text, picks, area.selectionStart, e.key);
      if (whole) {
        e.preventDefault();
        void removeWhole(whole.start, whole.end);
        return;
      }
    }
    if (e.key === "Enter" && !e.shiftKey && !e.isComposing) {
      e.preventDefault();
      submit();
    } else if (e.key === "Escape" && editing) {
      oncanceledit();
    } else if (e.key === "Escape" && replyTo) {
      oncancelreply();
    }
  }

  function toBase64Jpeg(canvas: HTMLCanvasElement): string | null {
    try {
      return canvas.toDataURL("image/jpeg", 0.65).split(",")[1] ?? null;
    } catch {
      return null;
    }
  }

  function drawThumb(source: CanvasImageSource, w: number, h: number): string | null {
    const scale = Math.min(1, 100 / Math.max(w, h));
    const canvas = document.createElement("canvas");
    canvas.width = Math.max(1, Math.round(w * scale));
    canvas.height = Math.max(1, Math.round(h * scale));
    canvas.getContext("2d")?.drawImage(source, 0, 0, canvas.width, canvas.height);
    return toBase64Jpeg(canvas);
  }

  function imageMeta(file: Picked): Promise<Partial<Attachment>> {
    return new Promise((resolve) => {
      const img = new Image();
      img.crossOrigin = "anonymous";
      img.onload = () =>
        resolve({ width: img.naturalWidth, height: img.naturalHeight, thumb: drawThumb(img, img.naturalWidth, img.naturalHeight), preview: true });
      img.onerror = () => resolve({ preview: false });
      img.src = localUrl(file.token);
    });
  }

  function videoMeta(file: Picked): Promise<Partial<Attachment>> {
    return new Promise((resolve) => {
      const video = document.createElement("video");
      video.crossOrigin = "anonymous";
      video.muted = true;
      video.preload = "metadata";
      const give_up = setTimeout(() => resolve({ preview: false }), 6000);
      video.onloadedmetadata = () => {
        video.currentTime = Math.min(0.2, (video.duration || 1) / 2);
      };
      video.onseeked = () => {
        clearTimeout(give_up);
        resolve({
          width: video.videoWidth,
          height: video.videoHeight,
          seconds: Math.round(video.duration || 0),
          thumb: drawThumb(video, video.videoWidth, video.videoHeight),
          preview: false,
        });
      };
      video.onerror = () => {
        clearTimeout(give_up);
        resolve({ preview: false });
      };
      video.src = localUrl(file.token);
    });
  }

  async function add(files: Picked[]) {
    for (const file of files) {
      const item: Attachment = { ...file, thumb: null, width: 0, height: 0, seconds: 0, preview: false };
      attached.push(item);
      const meta = file.kind === "image" ? await imageMeta(file) : file.kind === "video" ? await videoMeta(file) : {};
      const current = attached.find((a) => a.token === file.token);
      if (current) Object.assign(current, meta);
    }
  }

  async function pick() {
    try {
      await add(await api.pickFiles());
    } catch (e) {
      toast(errorText(e));
    }
  }

  function remove(token: string) {
    attached = attached.filter((a) => a.token !== token);
    void api.forgetToken(token);
  }

  async function onPaste(e: ClipboardEvent) {
    const item = [...(e.clipboardData?.items ?? [])].find((i) => i.type.startsWith("image/"));
    const file = item?.getAsFile();
    if (!file) return;
    e.preventDefault();
    try {
      const bytes = Array.from(new Uint8Array(await file.arrayBuffer()));
      const ext = file.type.split("/")[1] ?? "png";
      await add([await api.savePasted(bytes, ext === "jpeg" ? "jpg" : ext)]);
    } catch (err) {
      toast(errorText(err));
    }
  }

  let unlisten: () => void;
  onMount(() => {
    unlisten = on("files-dropped", (files: Picked[]) => void add(files));
    grow();
    area?.focus();
  });
  onDestroy(() => unlisten?.());

  $effect(() => {
    if (replyTo) area?.focus();
  });
</script>

<div class="composer">
  {#if editing}
    <div class="reply">
      <Icon name="check" size={16} />
      <div class="reply-body">
        <b>Edit message</b>
        <span>{editing.text}</span>
      </div>
      <button class="icon-btn" onclick={oncanceledit} aria-label="Cancel editing"><Icon name="close" size={16} /></button>
    </div>
  {:else if replyTo}
    <div class="reply">
      <Icon name="reply" size={16} />
      <div class="reply-body">
        <b>{replyTo.from_me ? "You" : replyTo.sender_name || "Them"}</b>
        <span>{replyTo.text || (replyTo.media ? `[${replyTo.media}]` : "")}</span>
      </div>
      <button class="icon-btn" onclick={oncancelreply} aria-label="Cancel reply"><Icon name="close" size={16} /></button>
    </div>
  {/if}

  {#if attached.length}
    <div class="attached">
      {#each attached as file (file.token)}
        <div class="att">
          {#if file.preview}
            <img src={localUrl(file.token)} alt="" />
          {:else if file.thumb}
            <img src="data:image/jpeg;base64,{file.thumb}" alt="" />
          {:else}
            <span class="att-icon"><Icon name="file" size={24} /></span>
          {/if}
          <div class="att-name" title={file.name}>{file.name}</div>
          <div class="att-size">{fileSize(file.size)}</div>
          <button class="att-x" onclick={() => remove(file.token)} aria-label="Remove {file.name}"><Icon name="close" size={13} /></button>
        </div>
      {/each}
      {#if attached.some((a) => a.kind === "image" || a.kind === "video")}
        <label class="as-file">
          <input type="checkbox" bind:checked={asFile} />
          Send as file (keeps full quality)
        </label>
      {/if}
    </div>
  {/if}

  {#if pickerOpen}
    <div class="picker" id={listId} role="listbox" aria-label="People to mention" bind:this={listEl}>
      {#if candidates.length === 0}
        <div class="picker-note" role="status">
          {membersStale ? "Couldn't get this group's members, so nobody can be mentioned right now." : "Getting this group's members…"}
        </div>
      {:else}
        {#each found as person, i (person.jid)}
          <button
            type="button"
            class="option"
            class:selected={i === selected}
            id="{listId}-{i}"
            role="option"
            aria-selected={i === selected}
            tabindex="-1"
            onmousedown={(e) => e.preventDefault()}
            onmousemove={() => (selected = i)}
            onclick={() => void choose(person)}
          >
            <Avatar name={person.name} jid={person.person} size={28} />
            <span class="option-name">{person.name}</span>
          </button>
        {/each}
      {/if}
    </div>
  {/if}

  {#if mentioning.length}
    <div class="mentioning" role="status">Will mention {mentioning.join(", ")}</div>
  {/if}

  <div class="bar">
    {#if !editing}
      <button class="icon-btn" onclick={pick} title="Send files" aria-label="Send files"><Icon name="attach" /></button>
    {/if}
    <textarea
      bind:this={area}
      bind:value={text}
      oninput={onInput}
      onkeydown={onKey}
      onkeyup={(e) => {
        if (["ArrowLeft", "ArrowRight", "Home", "End"].includes(e.key)) updatePicker();
      }}
      onclick={updatePicker}
      onblur={() => (query = null)}
      onpaste={onPaste}
      aria-autocomplete={isGroup ? "list" : undefined}
      aria-controls={pickerOpen ? listId : undefined}
      aria-activedescendant={pickerOpen && found.length ? `${listId}-${selected}` : undefined}
      rows="1"
      placeholder={attached.length ? "Add a caption" : "Type a message"}
      aria-label="Message"
    ></textarea>
    <button class="send" class:ready={canSend} onclick={submit} disabled={!canSend} aria-label={editing ? "Save edit" : "Send"} title={editing ? "Save (Enter)" : "Send (Enter)"}>
      <Icon name="send" size={19} />
    </button>
  </div>
</div>

<style>
  .composer {
    background: var(--panel);
    border-top: 1px solid var(--line);
    flex: none;
    position: relative;
  }
  .picker {
    position: absolute;
    left: 52px;
    bottom: calc(100% - 4px);
    width: min(340px, calc(100% - 64px));
    max-height: 260px;
    overflow-y: auto;
    background: var(--panel);
    border: 1px solid var(--line);
    border-radius: 10px;
    box-shadow: 0 6px 24px rgb(0 0 0 / 0.25);
    padding: 4px;
    z-index: 5;
  }
  .option {
    display: flex;
    align-items: center;
    gap: 10px;
    width: 100%;
    padding: 5px 8px;
    border-radius: 7px;
    text-align: left;
  }
  .option.selected {
    background: var(--panel-2);
  }
  .option-name {
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
  }
  .picker-note {
    padding: 8px 10px;
    color: var(--muted);
    font-size: 13px;
  }
  .mentioning {
    padding: 6px 14px 0;
    font-size: 12.5px;
    color: var(--muted);
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
  }
  .bar {
    display: flex;
    align-items: flex-end;
    gap: 6px;
    padding: 8px 10px;
  }
  textarea {
    flex: 1;
    resize: none;
    background: var(--panel-2);
    border: 1px solid transparent;
    border-radius: 9px;
    padding: 7px 11px;
    outline: none;
    max-height: 180px;
    min-height: 36px;
    user-select: text;
    line-height: 1.4;
  }
  textarea:focus {
    border-color: var(--accent);
  }
  .send {
    width: 36px;
    height: 36px;
    border-radius: 50%;
    display: grid;
    place-items: center;
    color: var(--faint);
    flex: none;
  }
  .send.ready {
    background: var(--accent);
    color: var(--accent-text);
  }
  .reply {
    display: flex;
    align-items: center;
    gap: 9px;
    padding: 6px 10px 0 14px;
    color: var(--muted);
  }
  .reply-body {
    flex: 1;
    min-width: 0;
    border-left: 3px solid var(--accent);
    padding-left: 8px;
    font-size: 13px;
    display: flex;
    flex-direction: column;
  }
  .reply-body b {
    color: var(--accent);
  }
  .reply-body span {
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
  }
  .attached {
    display: flex;
    flex-wrap: wrap;
    gap: 8px;
    padding: 10px 12px 0;
    align-items: center;
  }
  .att {
    width: 92px;
    position: relative;
    font-size: 11.5px;
  }
  .att img,
  .att-icon {
    width: 92px;
    height: 72px;
    border-radius: 8px;
    object-fit: cover;
    display: grid;
    place-items: center;
    background: var(--panel-2);
    color: var(--muted);
  }
  .att-name {
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
    margin-top: 2px;
  }
  .att-size {
    color: var(--muted);
  }
  .att-x {
    position: absolute;
    top: 3px;
    right: 3px;
    width: 20px;
    height: 20px;
    border-radius: 50%;
    background: rgb(0 0 0 / 0.6);
    color: #fff;
    display: grid;
    place-items: center;
  }
  .as-file {
    font-size: 12.5px;
    color: var(--muted);
    display: inline-flex;
    gap: 5px;
    align-items: center;
  }
</style>
