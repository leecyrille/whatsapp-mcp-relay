<script lang="ts">
  import { onDestroy, onMount, tick } from "svelte";
  import { api, errorText, type Thread, type ThreadItem } from "../lib/api";
  import { fileSize, listTime, revealed } from "../lib/format";
  import { on, setBusy, toast, ui } from "../lib/state.svelte";
  import Icon from "../lib/Icon.svelte";

  let threads = $state<Thread[]>([]);
  let current = $state<Thread | null>(null);
  let projects = $state<string[]>([]);
  let status = $state<any>(null);
  let reachable = $state(true);
  let newProject = $state("");
  let draft = $state("");
  $effect(() => setBusy("claude-draft", draft.trim().length > 0));
  onDestroy(() => setBusy("claude-draft", false));
  let sending = $state(false);
  let scroller = $state<HTMLDivElement>();
  let creating = $state(false);

  const pending = $derived((current?.items ?? []).filter((i) => i.kind === "approval" && i.state === "pending"));

  async function loadAll() {
    try {
      const [t, p, s] = await Promise.all([
        api.claude("GET", "/threads"),
        api.claude("GET", "/projects"),
        api.claude("GET", "/status"),
      ]);
      threads = t.threads;
      projects = p.projects;
      status = s;
      reachable = true;
      if (!newProject && projects.length) newProject = projects[0];
      if (current) await openThread(current.id);
      countWaiting();
    } catch {
      reachable = false;
    }
  }

  function countWaiting() {
    ui.approvalsWaiting = current ? pending.length : ui.approvalsWaiting;
  }

  async function openThread(id: string) {
    current = await api.claude("GET", `/threads/${id}`);
    creating = false;
    await scrollEnd();
    countWaiting();
  }

  async function scrollEnd() {
    await tick();
    if (scroller) scroller.scrollTop = scroller.scrollHeight;
  }

  function isNearEnd() {
    return !scroller || scroller.scrollHeight - scroller.scrollTop - scroller.clientHeight < 120;
  }

  async function send() {
    const text = draft.trim();
    if (!text || sending) return;
    sending = true;
    try {
      let target = current;
      if (!target) {
        target = (await api.claude("POST", "/threads", { project: newProject, title: text })) as Thread;
        current = target;
        creating = false;
        threads = [target, ...threads];
      }
      await api.claude("POST", `/threads/${target.id}/messages`, { text });
      draft = "";
    } catch (e) {
      toast(errorText(e));
    } finally {
      sending = false;
    }
  }

  async function decide(item: ThreadItem, approve: boolean) {
    try {
      await api.claude("POST", `/approvals/${item.id}`, { approve });
    } catch (e) {
      toast(errorText(e));
      if (current) await openThread(current.id);
    }
  }

  async function cancel() {
    if (!current) return;
    try {
      await api.claude("POST", `/threads/${current.id}/cancel`);
    } catch (e) {
      toast(errorText(e));
    }
  }

  async function remove(thread: Thread) {
    try {
      await api.claude("DELETE", `/threads/${thread.id}`);
      threads = threads.filter((t) => t.id !== thread.id);
      if (current?.id === thread.id) current = null;
    } catch (e) {
      toast(errorText(e));
    }
  }

  function startNew() {
    current = null;
    creating = true;
    draft = "";
  }

  function onKey(e: KeyboardEvent) {
    if (e.key === "Enter" && !e.shiftKey && !e.isComposing) {
      e.preventDefault();
      void send();
    }
  }

  /** Everything the tool was given appears on the card: the main field, then all the others. */
  function describe(item: ThreadItem) {
    const input = item.input ?? {};
    const pick = (summary: string, detail: string, used: string[]) => {
      const rest = Object.fromEntries(Object.entries(input).filter(([k]) => !used.includes(k)));
      const shown = revealed(String(detail));
      return {
        summary: revealed(summary).text,
        detail: shown.text,
        size: shown.lines > 1 || shown.bytes > 400 ? `${shown.lines} lines, ${fileSize(shown.bytes)}` : "",
        other: Object.keys(rest).length ? revealed(JSON.stringify(rest, null, 2)).text : "",
      };
    };
    switch (item.tool) {
      case "Bash":
      case "PowerShell":
        return pick("Run a command", input.command ?? "", ["command"]);
      case "Write":
        return pick(`Write ${input.file_path ?? "a file"}`, input.content ?? "", ["file_path", "content"]);
      case "Edit":
        return pick(`Edit ${input.file_path ?? "a file"}`, `- ${input.old_string ?? ""}\n+ ${input.new_string ?? ""}`, ["file_path", "old_string", "new_string"]);
      case "WebFetch":
        return pick("Fetch a web page", input.url ?? "", ["url"]);
      case "WebSearch":
        return pick("Search the web", input.query ?? "", ["query"]);
      default:
        return pick(item.tool ?? "Use a tool", JSON.stringify(input, null, 2), Object.keys(input));
    }
  }

  function toolLine(item: ThreadItem): string {
    const input = item.input ?? {};
    return input.file_path || input.pattern || input.path || input.command || input.url || input.query || "";
  }

  let off: () => void;
  onMount(() => {
    void loadAll();
    off = on("claude-event", async (ev) => {
      if (ev.type !== "claude") return;
      if (ev.kind === "state") {
        const t = threads.find((x) => x.id === ev.thread);
        if (t) t.running = ev.running;
        if (current?.id === ev.thread) current.running = ev.running;
        return;
      }
      if (current?.id !== ev.thread) {
        if (ev.kind === "item") loadAll();
        return;
      }
      const near = isNearEnd();
      const items = (current.items ??= []);
      if (ev.kind === "delta") {
        const existing = items.find((i) => i.id === ev.item_id);
        if (existing) existing.text = (existing.text ?? "") + ev.text;
        else items.push({ id: ev.item_id, kind: "assistant", text: ev.text, ts: Date.now() });
      } else if (ev.kind === "item") {
        const at = items.findIndex((i) => i.id === ev.item.id);
        if (at >= 0) items[at] = ev.item;
        else items.push(ev.item);
        ui.approvalsWaiting = items.filter((i) => i.kind === "approval" && i.state === "pending").length;
      }
      if (near) await scrollEnd();
    });
  });
  onDestroy(() => off?.());
</script>

<section class="page" aria-label="Claude">
  <aside class="threads">
    <button class="btn primary new" onclick={startNew}><Icon name="plus" size={15} /> New conversation</button>
    <ul>
      {#each threads as t (t.id)}
        <li class:on={current?.id === t.id}>
          <button class="thread" onclick={() => openThread(t.id)}>
            <span class="t-title">{t.title}</span>
            <span class="t-sub">
              {#if t.running}<span class="running">Working…</span>{:else}{listTime(t.updated)}{/if}
              · {t.project.split(/[\\/]/).pop()}
            </span>
          </button>
          {#if !t.running}
            <button class="icon-btn del" title="Delete" aria-label="Delete conversation" onclick={() => remove(t)}><Icon name="trash" size={15} /></button>
          {/if}
        </li>
      {/each}
    </ul>
  </aside>

  <div class="main">
    {#if !reachable || !ui.conn.claude_up}
      <div class="notice">
        <Icon name="alert" size={16} />
        <span>Claude on your home PC isn't reachable. It runs while someone is logged in to Windows there.</span>
      </div>
    {:else if status && !status.signed_in}
      <div class="notice">
        <Icon name="alert" size={16} />
        <span>Claude isn't signed in on your home PC. Run <code>claude auth login</code> there.</span>
      </div>
    {/if}

    {#if current}
      <header>
        <div class="h-title">{current.title}</div>
        <div class="h-sub" title={current.project}><Icon name="folder" size={13} /> {current.project}</div>
        {#if current.running}
          <button class="btn stop" onclick={cancel}><Icon name="stop" size={13} /> Stop</button>
        {/if}
      </header>
    {/if}

    <div class="log selectable" bind:this={scroller}>
      {#if !current}
        <div class="welcome">
          <Icon name="claude" size={36} />
          <h2>Ask Claude on your home PC</h2>
          <p class="muted">
            It can read files in the folder you choose without asking. Anything else, such as reading elsewhere, running a command or editing a file, waits for you to approve it here, and is refused if you don't answer within {status?.approval_timeout_minutes ?? 10} minutes.
          </p>
          <label class="pick">
            <Icon name="folder" size={15} />
            <select aria-label="Folder for this conversation" bind:value={newProject} class="input">
              {#each projects as p}
                <option value={p}>{p}</option>
              {/each}
            </select>
          </label>
        </div>
      {:else}
        {#each current.items ?? [] as item (item.id)}
          {#if item.kind === "user"}
            <div class="msg user">{item.text}</div>
          {:else if item.kind === "assistant"}
            <div class="msg bot">{item.text}</div>
          {:else if item.kind === "tool"}
            <div class="tool"><Icon name="check" size={13} /> <b>{item.tool}</b> <span>{toolLine(item)}</span></div>
          {:else if item.kind === "approval"}
            {@const d = describe(item)}
            <div class="approval" class:waiting={item.state === "pending"}>
              <div class="a-head">
                <Icon name="alert" size={16} />
                <b>{item.state === "pending" ? "Claude wants to:" : "Claude asked to:"} {d.summary}</b>
              </div>
              {#if d.detail}<pre>{d.detail}</pre>{/if}
              {#if d.size}<div class="a-size">{d.size}</div>{/if}
              {#if d.other}<div class="a-size">Also set:</div><pre class="other">{d.other}</pre>{/if}
              {#if item.state === "pending"}
                <div class="a-actions">
                  <button class="btn" onclick={() => decide(item, false)}>Deny</button>
                  <button class="btn primary" onclick={() => decide(item, true)}>Approve</button>
                </div>
              {:else}
                <div class="a-state" class:ok={item.state === "approved"}>
                  {item.state === "approved" ? "Approved" : item.state === "denied" ? "Denied" : "Expired without an answer, so it was refused"}
                </div>
              {/if}
            </div>
          {:else if item.kind === "error"}
            <div class="err" role="alert"><Icon name="alert" size={14} /> {item.text}</div>
          {:else if item.kind === "result" && item.cost}
            <div class="cost">About ${item.cost.toFixed(3)} of Claude usage</div>
          {/if}
        {/each}
        {#if current.running && pending.length === 0}
          <div class="working" aria-live="polite"><span></span><span></span><span></span></div>
        {/if}
      {/if}
    </div>

    <div class="bar">
      <textarea
        bind:value={draft}
        onkeydown={onKey}
        rows="2"
        placeholder={current ? "Reply to Claude" : "What should Claude do?"}
        aria-label="Message to Claude"
        disabled={current?.running}
      ></textarea>
      <button class="send" class:ready={draft.trim() && !current?.running} disabled={!draft.trim() || current?.running || sending} onclick={send} aria-label="Send">
        <Icon name="send" size={19} />
      </button>
    </div>
  </div>
</section>

<style>
  .page {
    flex: 1;
    display: flex;
    min-width: 0;
    background: var(--panel);
  }
  .threads {
    width: 260px;
    flex: none;
    border-right: 1px solid var(--line);
    display: flex;
    flex-direction: column;
    min-height: 0;
  }
  @media (max-width: 760px) {
    .threads {
      display: none;
    }
  }
  .new {
    margin: 12px;
    display: inline-flex;
    gap: 6px;
    align-items: center;
    justify-content: center;
  }
  ul {
    list-style: none;
    margin: 0;
    padding: 0 6px 8px;
    overflow-y: auto;
    flex: 1;
  }
  li {
    display: flex;
    align-items: center;
    border-radius: 8px;
  }
  li:hover {
    background: var(--hover);
  }
  li.on {
    background: var(--active);
  }
  .thread {
    flex: 1;
    min-width: 0;
    text-align: left;
    padding: 7px 8px;
    display: flex;
    flex-direction: column;
  }
  .t-title {
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
    font-weight: 600;
  }
  .t-sub {
    font-size: 12px;
    color: var(--muted);
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
  }
  .running {
    color: var(--accent);
    font-weight: 600;
  }
  .del {
    opacity: 0;
  }
  li:hover .del,
  .del:focus-visible {
    opacity: 1;
  }
  .main {
    flex: 1;
    min-width: 0;
    display: flex;
    flex-direction: column;
    background: var(--chat-bg);
  }
  .notice {
    display: flex;
    gap: 8px;
    align-items: center;
    padding: 8px 14px;
    background: var(--warn-bg);
    color: var(--warn-text);
    font-size: 13px;
  }
  .notice code {
    font-family: Consolas, monospace;
  }
  header {
    display: flex;
    align-items: center;
    gap: 10px;
    padding: 9px 16px;
    background: var(--panel);
    border-bottom: 1px solid var(--line);
  }
  .h-title {
    font-weight: 600;
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
    flex: 1;
  }
  .h-sub {
    font-size: 12px;
    color: var(--muted);
    display: inline-flex;
    gap: 4px;
    align-items: center;
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
    max-width: 40%;
  }
  .stop {
    display: inline-flex;
    gap: 5px;
    align-items: center;
  }
  .log {
    flex: 1;
    min-height: 0;
    overflow-y: auto;
    padding: 14px max(16px, calc((100% - 820px) / 2));
    display: flex;
    flex-direction: column;
    gap: 8px;
  }
  .welcome {
    margin: auto;
    max-width: 480px;
    text-align: center;
    color: var(--muted);
  }
  .welcome h2 {
    color: var(--text);
    font-size: 18px;
    margin: 8px 0;
  }
  .welcome p {
    margin: 0 0 14px;
  }
  .pick {
    display: flex;
    align-items: center;
    gap: 7px;
  }
  .pick select option {
    background: var(--panel);
    color: var(--text);
  }
  .msg {
    white-space: pre-wrap;
    overflow-wrap: anywhere;
    padding: 7px 11px;
    border-radius: 10px;
    max-width: 88%;
  }
  .user {
    background: var(--mine);
    align-self: flex-end;
  }
  .bot {
    background: var(--theirs);
    align-self: flex-start;
  }
  .tool {
    font-size: 12.5px;
    color: var(--muted);
    display: flex;
    gap: 5px;
    align-items: center;
    padding: 0 4px;
    white-space: nowrap;
    overflow: hidden;
  }
  .tool span {
    overflow: hidden;
    text-overflow: ellipsis;
    font-family: Consolas, monospace;
    font-size: 12px;
  }
  .approval {
    background: var(--panel);
    border: 1px solid var(--line);
    border-radius: 10px;
    padding: 10px 12px;
  }
  .approval.waiting {
    border: 2px solid #d97706;
  }
  .a-head {
    display: flex;
    gap: 7px;
    align-items: center;
  }
  .approval.waiting .a-head {
    color: #d97706;
  }
  .approval pre {
    margin: 8px 0 0;
    background: var(--panel-2);
    padding: 8px 10px;
    border-radius: 7px;
    font: 12.5px/1.45 Consolas, monospace;
    white-space: pre-wrap;
    overflow-wrap: anywhere;
    max-height: 320px;
    overflow-y: auto;
  }
  .a-size {
    font-size: 12px;
    color: var(--muted);
    margin-top: 4px;
  }
  .approval pre.other {
    margin-top: 2px;
  }
  .a-actions {
    display: flex;
    justify-content: flex-end;
    gap: 8px;
    margin-top: 10px;
  }
  .a-state {
    margin-top: 7px;
    font-size: 12.5px;
    color: var(--danger);
  }
  .a-state.ok {
    color: var(--accent);
  }
  .err {
    color: var(--danger);
    display: flex;
    gap: 6px;
    align-items: flex-start;
    font-size: 13px;
  }
  .cost {
    font-size: 11.5px;
    color: var(--faint);
    text-align: center;
  }
  .working {
    display: flex;
    gap: 4px;
    padding: 4px 6px;
  }
  .working span {
    width: 7px;
    height: 7px;
    border-radius: 50%;
    background: var(--faint);
    animation: pulse 1.1s infinite ease-in-out;
  }
  .working span:nth-child(2) {
    animation-delay: 0.15s;
  }
  .working span:nth-child(3) {
    animation-delay: 0.3s;
  }
  @keyframes pulse {
    50% {
      opacity: 0.25;
    }
  }
  .bar {
    display: flex;
    gap: 8px;
    align-items: flex-end;
    padding: 10px 12px;
    background: var(--panel);
    border-top: 1px solid var(--line);
  }
  textarea {
    flex: 1;
    resize: none;
    background: var(--panel-2);
    border: 1px solid transparent;
    border-radius: 9px;
    padding: 7px 11px;
    outline: none;
    user-select: text;
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
</style>
