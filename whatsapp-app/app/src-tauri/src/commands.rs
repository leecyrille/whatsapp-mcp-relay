//! What the UI can ask for. Reads come from the local cache, and only actions go to the home PC.

use crate::db::{ChatRow, ContactRow, MsgRow, SearchHit, WireMsg};
use crate::media::{self, SendRequest};
use crate::net::{self, NetError};
use crate::update;
use crate::{random_id, AppState};
use serde_json::{json, Value};
use std::collections::HashSet;
use std::sync::atomic::Ordering;
use std::sync::Arc;
use tauri::{AppHandle, Emitter, Manager, State};
use tauri_plugin_autostart::ManagerExt;
use tauri_plugin_dialog::DialogExt;
use tauri_plugin_opener::OpenerExt;

type St<'a> = State<'a, Arc<AppState>>;

/// A lapsed login is handled in one place: the UI shows sign-in, and the caller gets the message.
fn net_err(app: &AppHandle, st: &AppState, e: NetError) -> String {
    if matches!(e, NetError::NeedLogin) {
        net::need_login(app, st);
    }
    e.message()
}

#[tauri::command(async)]
pub fn auth_state(st: St<'_>) -> Value {
    st.conn_json()
}

#[tauri::command(async)]
pub fn conn_state(st: St<'_>) -> Value {
    st.conn_json()
}

/// The relay this copy talks to. An empty address means the first run, which asks for one.
#[tauri::command(async)]
pub fn relay_get(st: St<'_>) -> Value {
    json!({ "address": st.relay.base, "configured": !st.relay.base.is_empty() })
}

/// Saves a new relay address and starts the app again on it. Only the user's own UI calls this.
#[tauri::command(async)]
pub fn relay_set(app: AppHandle, st: St<'_>, address: String) -> Result<(), String> {
    let clean = crate::setup::clean_address(&address)?;
    if clean == st.relay.base {
        return Ok(());
    }
    crate::setup::save_address(&st.cache, &clean)?;
    app.restart()
}

#[tauri::command(async)]
pub fn open_login(app: AppHandle) -> Result<(), String> {
    crate::open_login_window(&app).map_err(|e| e.to_string())
}

/// Signs this device out. Removes the login, and with erase also removes the local copy of chats and
/// media. WhatsApp and the home PC aren't affected, and other devices stay signed in.
#[tauri::command]
pub async fn sign_out(app: AppHandle, st: St<'_, >, erase: bool) -> Result<(), String> {
    let st = st.inner().clone();
    st.relay.set_cookie(None);
    if let Some(window) = app.get_webview_window("main") {
        let _ = window.clear_all_browsing_data();
    }
    if erase {
        let cache = st.cache.clone();
        let dir = st.media_dir.clone();
        tauri::async_runtime::spawn_blocking(move || -> Result<(), String> {
            cache.wipe()?;
            let _ = std::fs::remove_dir_all(&dir);
            std::fs::create_dir_all(&dir).map_err(|e| e.to_string())
        })
        .await
        .map_err(|e| e.to_string())??;
    }
    net::need_login(&app, &st);
    let _ = app.emit("chats-changed", json!({"chats": []}));
    crate::update_tray(&app, &st);
    Ok(())
}

#[tauri::command(async)]
pub fn chat_list(st: St<'_>) -> Result<Vec<ChatRow>, String> {
    st.cache.chat_list()
}

#[tauri::command(async)]
pub fn messages_page(st: St<'_>, chat: String, before_ts: Option<i64>, before_rowid: Option<i64>, limit: Option<i64>) -> Result<Vec<MsgRow>, String> {
    st.cache.messages_page(&chat, before_ts, before_rowid, limit.unwrap_or(60).clamp(1, 300))
}

#[tauri::command(async)]
pub fn messages_after(st: St<'_>, chat: String, after_ts: i64, after_rowid: i64, limit: Option<i64>) -> Result<Vec<MsgRow>, String> {
    st.cache.messages_after(&chat, after_ts, after_rowid, limit.unwrap_or(300).clamp(1, 1000))
}

#[tauri::command(async)]
pub fn messages_around(st: St<'_>, rowid: i64) -> Result<Vec<MsgRow>, String> {
    st.cache.messages_around(rowid, 80)
}

#[tauri::command(async)]
pub fn search(st: St<'_>, query: String, limit: Option<i64>) -> Result<Vec<SearchHit>, String> {
    st.cache.search(&query, limit.unwrap_or(100).clamp(1, 500))
}

#[tauri::command(async)]
pub fn set_active_chat(st: St<'_>, chat: Option<String>) {
    *st.active_chat.lock().unwrap() = chat;
}

#[tauri::command]
pub fn take_open_target(st: St<'_>) -> Option<crate::toast::Target> {
    st.open_target.lock().unwrap().take()
}

#[tauri::command]
pub fn silence_state(st: St<'_>) -> Value {
    let mut state = json!({ "until_ms": crate::toast::silenced_until(&st) });
    // Only so a test can check what a build would have shown, and its link. Shipped builds never send these.
    if crate::test_build() {
        state["test_toasts"] = json!(st.test_toasts.load(Ordering::SeqCst));
        state["test_link"] = json!(crate::toast::silence_test_link(&st));
    }
    state
}

/// Whether the tray and taskbar may blink: the Settings switch, and Windows' setting for fewer animations.
#[tauri::command]
pub fn blink_set(st: St<'_>, on: bool) {
    crate::attention::set_motion(&st, on);
}

/// Only so a test can check the blink, and act on the window as the user would. Shipped builds refuse it.
#[tauri::command]
pub fn attention_test(app: AppHandle, st: St<'_>, action: String) -> Result<Value, String> {
    if !crate::test_build() {
        return Err("Only for tests.".into());
    }
    let window = app.get_webview_window("main").ok_or("no window")?;
    match action.as_str() {
        "close" => window.close().map_err(|e| e.to_string())?,
        "show" => crate::attention::test_open(&window),
        "minimize" => crate::attention::test_minimize(&window),
        _ => {}
    }
    let mut state = crate::attention::test_state(&st);
    state["visible"] = json!(window.is_visible().unwrap_or(false));
    state["minimized"] = json!(window.is_minimized().unwrap_or(false));
    state["focused"] = json!(window.is_focused().unwrap_or(false));
    Ok(state)
}

#[tauri::command]
pub fn silence_set(app: AppHandle, st: St<'_>, minutes: u32) -> Result<(), String> {
    crate::toast::silence(&app, &st, minutes)
}

#[tauri::command]
pub fn silence_clear(app: AppHandle, st: St<'_>) -> Result<(), String> {
    crate::toast::silence_off(&app, &st)
}

/// Marks a chat read. The UI calls this only while the chat is on screen and the window is focused,
/// because it sends blue ticks when that setting is on.
#[tauri::command]
pub async fn mark_read(app: AppHandle, st: St<'_, >, chat: String) -> Result<(), String> {
    let st = st.inner().clone();
    let batches = st.cache.read_batches(&chat)?;
    if batches.is_empty() {
        return Ok(());
    }
    let up_to = batches.iter().map(|b| b.up_to_ms).max().unwrap_or(0);
    st.cache.mark_read_local(&chat, up_to)?;
    let _ = app.emit("chats-changed", json!({"chats": [chat]}));
    crate::update_tray(&app, &st);
    let receipts = st.cache.meta_get("setting_read_receipts")?.as_deref() != Some("false");
    let bodies = batches
        .into_iter()
        .map(|b| json!({"chat": b.chat, "up_to_ms": b.up_to_ms, "receipts": receipts, "messages": b.messages}))
        .collect();
    send_reads(&app, &st, bodies).await;
    Ok(())
}

/// Guards the list of reads the home PC hasn't had yet, which lives in meta so it survives a restart.
static PENDING_READS: std::sync::Mutex<()> = std::sync::Mutex::new(());

/// Sends reads to the home PC, which tells WhatsApp so the phone clears too. The chat is already clear on this
/// device, so a read that doesn't get through is kept and sent again after the next sync; otherwise the phone
/// and every other PC would keep showing it unread.
async fn send_reads(app: &AppHandle, st: &Arc<AppState>, bodies: Vec<Value>) {
    let mut failed = vec![];
    for body in bodies {
        match st.relay.post("/api/read", &body).await {
            Ok(_) => {}
            Err(NetError::Server(code, _)) if (400..500).contains(&code) && code != 408 && code != 429 => {
                // Refused outright: sending it again would only be refused again.
            }
            Err(e) => {
                if matches!(e, NetError::NeedLogin) {
                    net::need_login(app, st);
                }
                failed.push(body);
            }
        }
    }
    if !failed.is_empty() {
        let _guard = PENDING_READS.lock().unwrap();
        let mut all = pending_reads(st);
        for body in failed {
            merge_read(&mut all, body);
        }
        let _ = st.cache.meta_set("pending_reads", &Value::Object(all).to_string());
    }
}

fn pending_reads(st: &AppState) -> serde_json::Map<String, Value> {
    st.cache
        .meta_get("pending_reads")
        .ok()
        .flatten()
        .and_then(|s| serde_json::from_str(&s).ok())
        .unwrap_or_default()
}

/// One waiting read per chat: the furthest point read, and every message still owed a receipt, newest first.
pub(crate) fn merge_read(all: &mut serde_json::Map<String, Value>, body: Value) {
    let Some(chat) = body["chat"].as_str().map(str::to_string) else { return };
    let Some(old) = all.remove(&chat) else {
        all.insert(chat, body);
        return;
    };
    let up_to = body["up_to_ms"].as_i64().unwrap_or(0).max(old["up_to_ms"].as_i64().unwrap_or(0));
    let mut seen = HashSet::new();
    let messages: Vec<Value> = body["messages"]
        .as_array()
        .into_iter()
        .chain(old["messages"].as_array())
        .flatten()
        .filter(|m| seen.insert(m["id"].as_str().unwrap_or("").to_string()))
        .take(200)
        .cloned()
        .collect();
    let mut merged = body;
    merged["up_to_ms"] = json!(up_to);
    merged["messages"] = json!(messages);
    all.insert(chat, merged);
}

/// Sends the reads that didn't get through before. Called after each successful sync.
pub async fn flush_pending_reads(app: AppHandle, st: Arc<AppState>) {
    let bodies: Vec<Value> = {
        let _guard = PENDING_READS.lock().unwrap();
        let all = pending_reads(&st);
        if all.is_empty() {
            return;
        }
        let _ = st.cache.meta_set("pending_reads", "{}");
        all.into_iter().map(|(_, v)| v).collect()
    };
    send_reads(&app, &st, bodies).await;
}

/// Sends a text. The user pressed send, so this is their message. The client ID makes a retry safe: the
/// home PC won't send the same one twice.
#[tauri::command]
pub async fn send_text(
    app: AppHandle,
    st: St<'_, >,
    chat: String,
    text: String,
    quote: Option<Value>,
    client_id: Option<String>,
    mentions: Option<Vec<String>>,
) -> Result<MsgRow, String> {
    let st = st.inner().clone();
    let mut body = json!({"chat": chat, "text": text, "quote": quote, "client_id": client_id.unwrap_or_else(random_id)});
    // Only sent when there are some, since the home PC refuses mentions outside a group.
    if let Some(m) = mentions.filter(|m| !m.is_empty()) {
        body["mentions"] = json!(m);
    }
    let reply = st.relay.post("/api/send", &body).await.map_err(|e| net_err(&app, &st, e))?;
    let message: WireMsg = reply
        .get("message")
        .cloned()
        .and_then(|m| serde_json::from_value(m).ok())
        .ok_or("Sent, but the reply was unreadable. It will appear when sync catches up.")?;
    let (raw_chat, id) = (message.chat.clone(), message.id.clone());
    st.cache.apply_batch(&[message], &[], None)?;
    let _ = app.emit("chats-changed", json!({"chats": [st.cache.canon(&raw_chat)]}));
    let canon = st.cache.canon(&raw_chat);
    let page = st.cache.messages_page(&canon, None, None, 20)?;
    page.into_iter()
        .find(|m| m.id == id)
        .ok_or_else(|| "Sent, but not found in the cache.".to_string())
}

/// Applies the changed message the home PC sends back, so the change shows at once.
fn apply_returned(app: &AppHandle, st: &Arc<AppState>, reply: &Value) -> Result<(), String> {
    if let Some(m) = reply.get("message").cloned().and_then(|m| serde_json::from_value::<WireMsg>(m).ok()) {
        let chat = st.cache.canon(&m.chat);
        st.cache.apply_batch(&[m], &[], None)?;
        let _ = app.emit("chats-changed", json!({"chats": [chat]}));
    }
    Ok(())
}

/// Edits one of the user's own text messages. The home PC checks it's their, and within WhatsApp's 15 minutes.
#[tauri::command]
pub async fn edit_message(app: AppHandle, st: St<'_, >, raw_chat: String, id: String, text: String, mentions: Option<Vec<String>>) -> Result<(), String> {
    let st = st.inner().clone();
    let mut body = json!({"chat": raw_chat, "id": id, "text": text});
    if let Some(m) = mentions.filter(|m| !m.is_empty()) {
        body["mentions"] = json!(m);
    }
    let reply = st.relay.post("/api/edit", &body).await.map_err(|e| net_err(&app, &st, e))?;
    apply_returned(&app, &st, &reply)
}

/// Deletes one of the user's own messages for everyone.
#[tauri::command]
pub async fn revoke_message(app: AppHandle, st: St<'_, >, raw_chat: String, id: String) -> Result<(), String> {
    let st = st.inner().clone();
    let reply = st.relay.post("/api/revoke", &json!({"chat": raw_chat, "id": id})).await.map_err(|e| net_err(&app, &st, e))?;
    apply_returned(&app, &st, &reply)
}

/// Delete for me: hidden in WA Relay on every PC. Nothing changes on the phone or for anyone else.
#[tauri::command]
pub async fn hide_message(app: AppHandle, st: St<'_, >, raw_chat: String, id: String) -> Result<(), String> {
    let st = st.inner().clone();
    st.relay.post("/api/hide", &json!({"chat": raw_chat, "id": id})).await.map_err(|e| net_err(&app, &st, e))?;
    let chat = st.cache.canon(&raw_chat);
    st.cache.apply_batch(&[WireMsg { chat: raw_chat, id, hidden: true, ..Default::default() }], &[], None)?;
    let _ = app.emit("chats-changed", json!({"chats": [chat]}));
    Ok(())
}

/// Forwards to up to 5 chats. The home PC sends in the background and reports with a forward-result event.
#[tauri::command]
pub async fn forward_message(app: AppHandle, st: St<'_, >, raw_chat: String, id: String, to: Vec<String>, client_id: String) -> Result<Value, String> {
    let st = st.inner().clone();
    let body = json!({"chat": raw_chat, "id": id, "to": to, "client_id": client_id});
    st.relay.post("/api/forward", &body).await.map_err(|e| net_err(&app, &st, e))
}

/// All contact cards. They live on the home PC; this PC keeps the last copy, for when it's offline. Sign out with
/// "remove saved chats" erases it with the rest.
#[tauri::command]
pub async fn cards_get(app: AppHandle, st: St<'_, >) -> Result<Value, String> {
    let st = st.inner().clone();
    match st.relay.get("/api/cards").await {
        Ok(doc) => {
            let _ = st.cache.meta_set("cards", &doc.to_string());
            Ok(doc)
        }
        Err(e) => {
            if let Some(saved) = st.cache.meta_get("cards").ok().flatten().and_then(|raw| serde_json::from_str::<Value>(&raw).ok()) {
                return Ok(json!({"cards": saved["cards"], "rev": saved["rev"], "offline": true}));
            }
            Err(net_err(&app, &st, e))
        }
    }
}

#[tauri::command]
pub async fn card_save(app: AppHandle, st: St<'_, >, card: Value, expect: i64) -> Result<Value, String> {
    let st = st.inner().clone();
    st.relay.post("/api/cards/save", &json!({"card": card, "expect_version": expect})).await.map_err(|e| net_err(&app, &st, e))
}

#[tauri::command]
pub async fn card_delete(app: AppHandle, st: St<'_, >, id: String, expect: i64) -> Result<(), String> {
    let st = st.inner().clone();
    st.relay.post("/api/cards/delete", &json!({"id": id, "expect_version": expect})).await.map_err(|e| net_err(&app, &st, e))?;
    Ok(())
}

#[tauri::command]
pub async fn card_history(app: AppHandle, st: St<'_, >, id: Option<String>) -> Result<Value, String> {
    let st = st.inner().clone();
    let path = match id {
        Some(id) if id.chars().all(|c| c.is_ascii_alphanumeric()) => format!("/api/cards/history?id={id}"),
        Some(_) => return Err("Bad card.".into()),
        None => "/api/cards/history?deleted=1".to_string(),
    };
    st.relay.get(&path).await.map_err(|e| net_err(&app, &st, e))
}

#[tauri::command]
pub async fn card_undo(app: AppHandle, st: St<'_, >, seq: i64) -> Result<Value, String> {
    let st = st.inner().clone();
    st.relay.post("/api/cards/undo", &json!({"seq": seq})).await.map_err(|e| net_err(&app, &st, e))
}

#[tauri::command]
pub async fn card_suggest(app: AppHandle, st: St<'_, >, card: Value) -> Result<Value, String> {
    let st = st.inner().clone();
    st.relay.post("/api/cards/suggest", &card).await.map_err(|e| net_err(&app, &st, e))
}

#[tauri::command]
pub async fn card_import(app: AppHandle, st: St<'_, >, cards: Value, apply: bool, choices: Value, link_exact: bool) -> Result<Value, String> {
    let st = st.inner().clone();
    let body = json!({"cards": cards, "apply": apply, "choices": choices, "link_exact": link_exact});
    st.relay.post("/api/cards/import", &body).await.map_err(|e| net_err(&app, &st, e))
}

const VCARD_MAX_BYTES: u64 = 5 << 20;

/// A .vcf file's text, read on this PC. Only its text goes anywhere, after the user has seen the preview.
#[tauri::command]
pub async fn pick_vcard(app: AppHandle) -> Result<Option<String>, String> {
    let path = if let Some(path) = crate::test_override("WA_TEST_PICK_VCARD") {
        std::path::PathBuf::from(path)
    } else {
        let (tx, rx) = tokio::sync::oneshot::channel();
        app.dialog()
            .file()
            .set_title("Import contact cards")
            .add_filter("Contact cards", &["vcf", "vcard"])
            .pick_file(move |file| {
                let _ = tx.send(file);
            });
        let Some(file) = rx.await.map_err(|e| e.to_string())? else { return Ok(None) };
        file.into_path().map_err(|e| e.to_string())?
    };
    let size = std::fs::metadata(&path).map_err(|e| e.to_string())?.len();
    if size > VCARD_MAX_BYTES {
        return Err("That file is bigger than 5 MB. Export fewer contacts at a time.".into());
    }
    let bytes = std::fs::read(&path).map_err(|e| e.to_string())?;
    Ok(Some(String::from_utf8_lossy(&bytes).into_owned()))
}

#[tauri::command]
pub async fn react(app: AppHandle, st: St<'_, >, raw_chat: String, id: String, sender_jid: String, emoji: String) -> Result<(), String> {
    let st = st.inner().clone();
    let body = json!({"chat": raw_chat, "id": id, "sender_jid": sender_jid, "emoji": emoji});
    st.relay.post("/api/react", &body).await.map_err(|e| net_err(&app, &st, e))?;
    Ok(())
}

/// Asks the user's phone for older messages in this chat. They arrive later through normal sync.
#[tauri::command]
pub async fn load_older(app: AppHandle, st: St<'_, >, raw_chat: String) -> Result<(), String> {
    let st = st.inner().clone();
    st.relay
        .post("/api/history", &json!({"chat": raw_chat, "count": 100}))
        .await
        .map_err(|e| net_err(&app, &st, e))?;
    Ok(())
}

#[tauri::command]
pub async fn pick_files(app: AppHandle, st: St<'_, >) -> Result<Vec<Value>, String> {
    let st = st.inner().clone();
    let (tx, rx) = tokio::sync::oneshot::channel();
    app.dialog().file().set_title("Send files").pick_files(move |files| {
        let _ = tx.send(files);
    });
    let files = rx.await.map_err(|e| e.to_string())?.unwrap_or_default();
    Ok(files
        .into_iter()
        .filter_map(|f| f.into_path().ok())
        .filter_map(|p| media::register_picked(&st, p))
        .collect())
}

/// Saves a pasted screenshot to a temp file, so it can go through the normal send path.
#[tauri::command]
pub async fn save_pasted(st: St<'_, >, bytes: Vec<u8>, ext: String) -> Result<Value, String> {
    let st = st.inner().clone();
    if bytes.is_empty() || bytes.len() > 64 << 20 {
        return Err("That image can't be pasted.".into());
    }
    let ext = match ext.as_str() {
        "png" => "png",
        "jpg" | "jpeg" => "jpg",
        "webp" => "webp",
        "gif" => "gif",
        _ => return Err("That image type can't be pasted.".into()),
    };
    let dir = st.media_dir.join("pasted");
    tokio::fs::create_dir_all(&dir).await.map_err(|e| e.to_string())?;
    let path = dir.join(format!("Screenshot-{}.{ext}", random_id()));
    tokio::fs::write(&path, bytes).await.map_err(|e| e.to_string())?;
    media::register_picked(&st, path).ok_or_else(|| "Couldn't save that image.".to_string())
}

#[tauri::command(async)]
pub fn forget_token(st: St<'_>, token: String) {
    st.picked.lock().unwrap().remove(&token);
}

#[tauri::command]
#[allow(clippy::too_many_arguments)]
pub async fn send_file(
    app: AppHandle,
    st: St<'_, >,
    chat: String,
    token: String,
    kind: String,
    caption: String,
    thumb: Option<String>,
    width: u32,
    height: u32,
    seconds: u32,
    quote: Option<Value>,
    client_id: String,
    mentions: Option<Vec<String>>,
) -> Result<(), String> {
    let st = st.inner().clone();
    let _sending = st.updates.sending();
    let mentions = mentions.unwrap_or_default();
    media::send_file(
        app,
        st,
        SendRequest { chat, token, kind, caption, thumb_b64: thumb, width, height, seconds, quote, client_id, mentions },
    )
    .await
}

#[tauri::command]
pub async fn media_fetch(app: AppHandle, st: St<'_, >, rowid: i64) -> Result<(), String> {
    let st = st.inner().clone();
    media::ensure_cached(&app, &st, rowid).await.map(|_| ())
}

#[tauri::command]
pub async fn media_save(app: AppHandle, st: St<'_, >, rowid: i64) -> Result<bool, String> {
    let st = st.inner().clone();
    let info = st.cache.media_info(rowid)?.ok_or("That message is gone.")?;
    let source = media::ensure_cached(&app, &st, rowid).await?;
    let (tx, rx) = tokio::sync::oneshot::channel();
    app.dialog()
        .file()
        .set_file_name(media::safe_name(&info.filename))
        .save_file(move |choice| {
            let _ = tx.send(choice);
        });
    let Some(target) = rx.await.map_err(|e| e.to_string())? else {
        return Ok(false);
    };
    let target = target.into_path().map_err(|e| e.to_string())?;
    tokio::fs::copy(&source, &target).await.map_err(|e| e.to_string())?;
    // Some drives, such as USB sticks, can't hold the mark. The file is still saved.
    let _ = media::mark_from_internet(&target);
    Ok(true)
}

/// Opens a received file in its default program, for types that can't run code. It goes under its own
/// name in a private folder, since programs choose how to read a file by its extension.
#[tauri::command]
pub async fn media_open(app: AppHandle, st: St<'_, >, rowid: i64) -> Result<(), String> {
    let st = st.inner().clone();
    let info = st.cache.media_info(rowid)?.ok_or("That message is gone.")?;
    let name = media::safe_name(&info.filename);
    if !media::can_open(&name) {
        return Err("This file type could run code, so it can't be opened from here. Save it first if you trust it.".into());
    }
    let source = media::ensure_cached(&app, &st, rowid).await?;
    let dir = st.media_dir.join("open").join(rowid.to_string());
    tokio::fs::create_dir_all(&dir).await.map_err(|e| e.to_string())?;
    let target = dir.join(&name);
    tokio::fs::copy(&source, &target).await.map_err(|e| e.to_string())?;
    // Opening without the mark would skip Protected View, so no mark means no opening.
    media::mark_from_internet(&target).map_err(|_| "This file can't be opened safely from here. Save it instead.".to_string())?;
    app.opener()
        .open_path(target.to_string_lossy().to_string(), None::<&str>)
        .map_err(|e| e.to_string())
}

/// Opens a link from a message in the default browser. Only web links, so a message can't launch a
/// file or another app through a custom scheme.
#[tauri::command(async)]
pub fn open_link(app: AppHandle, url: String) -> Result<(), String> {
    let parsed: url::Url = url.parse().map_err(|_| "That isn't a valid link.".to_string())?;
    if !matches!(parsed.scheme(), "http" | "https") {
        return Err("Only web links can be opened.".into());
    }
    app.opener().open_url(parsed.as_str(), None::<&str>).map_err(|e| e.to_string())
}

#[tauri::command(async)]
pub fn contacts(st: St<'_>) -> Result<Vec<ContactRow>, String> {
    st.cache.contact_list()
}

#[tauri::command(async)]
pub fn groups_get(st: St<'_>) -> Result<Value, String> {
    let raw = st.cache.meta_get("groups")?;
    Ok(raw
        .and_then(|r| serde_json::from_str(&r).ok())
        .unwrap_or_else(|| json!({"groups": [], "assign": {}})))
}

async fn group_call(app: &AppHandle, st: &Arc<AppState>, req: Result<reqwest::RequestBuilder, NetError>) -> Result<Value, String> {
    let req = req.map_err(|e| net_err(app, st, e))?;
    let doc = st.relay.json(req).await.map_err(|e| net_err(app, st, e))?;
    let _ = st.cache.meta_set("groups", &doc.to_string());
    let _ = app.emit("groups", doc.clone());
    Ok(doc)
}

#[tauri::command]
pub async fn group_create(app: AppHandle, st: St<'_, >, name: String, color: Option<String>) -> Result<Value, String> {
    let st = st.inner().clone();
    let req = st.relay.request(reqwest::Method::POST, "/api/groups").map(|r| r.json(&json!({"name": name, "color": color})));
    group_call(&app, &st, req).await
}

#[tauri::command]
pub async fn group_update(app: AppHandle, st: St<'_, >, id: String, name: Option<String>, color: Option<String>, order: Option<i64>) -> Result<Value, String> {
    let st = st.inner().clone();
    if !id.chars().all(|c| c.is_ascii_alphanumeric()) {
        return Err("Bad group.".into());
    }
    let req = st.relay.request(reqwest::Method::PATCH, &format!("/api/groups/{id}")).map(|r| r.json(&json!({"name": name, "color": color, "order": order})));
    group_call(&app, &st, req).await
}

#[tauri::command]
pub async fn group_delete(app: AppHandle, st: St<'_, >, id: String) -> Result<Value, String> {
    let st = st.inner().clone();
    if !id.chars().all(|c| c.is_ascii_alphanumeric()) {
        return Err("Bad group.".into());
    }
    let req = st.relay.request(reqwest::Method::DELETE, &format!("/api/groups/{id}"));
    group_call(&app, &st, req).await
}

#[tauri::command]
pub async fn assign(app: AppHandle, st: St<'_, >, jids: Vec<String>, group: String) -> Result<Value, String> {
    let st = st.inner().clone();
    let req = st.relay.request(reqwest::Method::POST, "/api/assign").map(|r| r.json(&json!({"jids": jids, "group": group})));
    group_call(&app, &st, req).await
}

/// Calls for the Claude panel. The relay has the real route list and the runner has the rules; this
/// only limits the UI to Claude paths, using a known method.
#[tauri::command]
pub async fn claude_api(app: AppHandle, st: St<'_, >, method: String, path: String, body: Option<Value>) -> Result<Value, String> {
    let st = st.inner().clone();
    if !path.starts_with('/') || path.contains("..") || path.contains('?') || path.contains('#') {
        return Err("Bad path.".into());
    }
    let method = match method.as_str() {
        "GET" => reqwest::Method::GET,
        "POST" => reqwest::Method::POST,
        "DELETE" => reqwest::Method::DELETE,
        _ => return Err("Bad method.".into()),
    };
    let mut req = st
        .relay
        .request(method, &format!("/api/claude{path}"))
        .map_err(|e| net_err(&app, &st, e))?;
    if let Some(body) = body {
        req = req.json(&body);
    }
    st.relay.json(req).await.map_err(|e| net_err(&app, &st, e))
}

/// Starts Claude sorting chats into groups. Only suggestions come back: nothing changes until the user
/// accepts them. Message text goes only if the user chose that, and it's limited to a few recent lines.
#[tauri::command]
pub async fn sort_start(app: AppHandle, st: St<'_, >, include_snippets: bool, only_unsorted: bool, include_contacts: bool) -> Result<Value, String> {
    let st = st.inner().clone();
    let raw = st.cache.meta_get("groups")?.unwrap_or_else(|| "{}".into());
    let doc: Value = serde_json::from_str(&raw).unwrap_or(Value::Null);
    let groups: Vec<Value> = doc
        .get("groups")
        .and_then(Value::as_array)
        .map(|list| {
            list.iter()
                .map(|g| json!({"id": g.get("id"), "name": g.get("name")}))
                .collect()
        })
        .unwrap_or_default();
    if groups.is_empty() {
        return Err("Create a group first.".into());
    }
    let mut skip: HashSet<String> = HashSet::new();
    if only_unsorted {
        if let Some(map) = doc.get("assign").and_then(Value::as_object) {
            skip.extend(map.keys().cloned());
        }
    }
    let cache = st.cache.clone();
    let items = tauri::async_runtime::spawn_blocking(move || cache.sort_items(include_snippets, &skip, include_contacts))
        .await
        .map_err(|e| e.to_string())??;
    if items.is_empty() {
        return Err("Everything is already sorted.".into());
    }
    st.relay
        .post("/api/claude/sort", &json!({"items": items, "groups": groups}))
        .await
        .map_err(|e| net_err(&app, &st, e))
}

const SETTINGS: &[&str] = &["read_receipts", "notifications", "media_cache_gb", "auto_images", "auto_update"];

#[tauri::command(async)]
pub fn settings_get(app: AppHandle, st: St<'_>) -> Result<Value, String> {
    let get = |key: &str| st.cache.meta_get(&format!("setting_{key}")).ok().flatten();
    Ok(json!({
        "read_receipts": get("read_receipts").as_deref() != Some("false"),
        "notifications": get("notifications").as_deref() != Some("false"),
        "auto_images": get("auto_images").as_deref() != Some("false"),
        "auto_update": get("auto_update").as_deref() != Some("false"),
        "media_cache_gb": get("media_cache_gb").and_then(|v| v.parse::<u64>().ok()).unwrap_or(5),
        "autostart": app.autolaunch().is_enabled().unwrap_or(false),
    }))
}

#[tauri::command(async)]
pub fn settings_set(app: AppHandle, st: St<'_>, key: String, value: Value) -> Result<(), String> {
    if key == "autostart" {
        let manager = app.autolaunch();
        return if value.as_bool().unwrap_or(false) {
            manager.enable().map_err(|e| e.to_string())
        } else {
            manager.disable().map_err(|e| e.to_string())
        };
    }
    if !SETTINGS.contains(&key.as_str()) {
        return Err("Unknown setting.".into());
    }
    if key == "auto_update" && value == Value::Bool(true) {
        let _ = st.updates.request_check();
    }
    let text = match value {
        Value::Bool(b) => b.to_string(),
        Value::Number(n) => n.to_string(),
        _ => return Err("Bad value.".into()),
    };
    st.cache.meta_set(&format!("setting_{key}"), &text)
}

#[tauri::command(async)]
pub fn cache_stats(st: St<'_>) -> Result<Value, String> {
    let (messages, chats, db_bytes) = st.cache.stats()?;
    Ok(json!({
        "messages": messages, "chats": chats, "db_bytes": db_bytes,
        "media_bytes": media::cache_bytes(&st.media_dir),
        "online": st.online.load(Ordering::SeqCst),
    }))
}

const PICTURE_MAX_BYTES: u64 = 10 << 20;

#[tauri::command(async)]
pub fn pictures_get(st: St<'_>) -> Value {
    st.cache
        .meta_get("pictures")
        .ok()
        .flatten()
        .and_then(|raw| serde_json::from_str(&raw).ok())
        .unwrap_or_else(|| json!({"pictures": {}, "own": []}))
}

#[tauri::command]
pub async fn pick_picture(app: AppHandle, st: St<'_, >) -> Result<Option<Value>, String> {
    let st = st.inner().clone();
    // A test can't drive Windows' file dialog.
    if let Some(path) = crate::test_override("WA_TEST_PICK_FILE") {
        return Ok(media::register_picked(&st, path.into()));
    }
    let (tx, rx) = tokio::sync::oneshot::channel();
    app.dialog()
        .file()
        .set_title("Choose a picture")
        .add_filter("Images", &["jpg", "jpeg", "png", "webp"])
        .pick_file(move |file| {
            let _ = tx.send(file);
        });
    let Some(file) = rx.await.map_err(|e| e.to_string())? else { return Ok(None) };
    let path = file.into_path().map_err(|e| e.to_string())?;
    Ok(media::register_picked(&st, path))
}

/// Replaces the picture this app shows for a chat, on every PC. The home PC checks and re-encodes the
/// image. Nothing changes on WhatsApp.
#[tauri::command]
pub async fn picture_set(app: AppHandle, st: St<'_, >, jid: String, token: String) -> Result<(), String> {
    let st = st.inner().clone();
    if !media::valid_jid(&jid) {
        return Err("Bad chat.".into());
    }
    let path = st.picked.lock().unwrap().get(&token).cloned().ok_or("That file is no longer available.")?;
    let size = tokio::fs::metadata(&path).await.map_err(|e| e.to_string())?.len();
    if size > PICTURE_MAX_BYTES {
        return Err("The image must be under 10 MB.".into());
    }
    let bytes = tokio::fs::read(&path).await.map_err(|e| e.to_string())?;
    let query: String = url::form_urlencoded::byte_serialize(jid.as_bytes()).collect();
    let req = st
        .relay
        .request(reqwest::Method::POST, &format!("/api/picture/own?jid={query}"))
        .map_err(|e| net_err(&app, &st, e))?
        .body(bytes);
    st.relay.json(req).await.map_err(|e| net_err(&app, &st, e))?;
    st.picked.lock().unwrap().remove(&token);
    Ok(())
}

/// Whether WhatsApp says the user may change this group's real photo.
#[tauri::command]
pub async fn picture_group_access(app: AppHandle, st: St<'_, >, jid: String) -> Result<Value, String> {
    let st = st.inner().clone();
    if !media::valid_jid(&jid) || !jid.ends_with("@g.us") {
        return Err("Bad chat.".into());
    }
    let query: String = url::form_urlencoded::byte_serialize(jid.as_bytes()).collect();
    st.relay.get(&format!("/api/picture/group-access?jid={query}")).await.map_err(|e| net_err(&app, &st, e))
}

/// Changes a group's real photo on WhatsApp, which everyone in it sees. The UI only calls this once the user has
/// confirmed. A refusal comes back as an error, and nothing falls back to a WA Relay-only picture.
#[tauri::command]
pub async fn picture_group_set(app: AppHandle, st: St<'_, >, jid: String, token: String) -> Result<Value, String> {
    let st = st.inner().clone();
    if !media::valid_jid(&jid) || !jid.ends_with("@g.us") {
        return Err("Bad chat.".into());
    }
    let path = st.picked.lock().unwrap().get(&token).cloned().ok_or("That file is no longer available.")?;
    let size = tokio::fs::metadata(&path).await.map_err(|e| e.to_string())?.len();
    if size > PICTURE_MAX_BYTES {
        return Err("The image must be under 10 MB.".into());
    }
    let bytes = tokio::fs::read(&path).await.map_err(|e| e.to_string())?;
    let query: String = url::form_urlencoded::byte_serialize(jid.as_bytes()).collect();
    let req = st
        .relay
        .request(reqwest::Method::POST, &format!("/api/picture/group?jid={query}"))
        .map_err(|e| net_err(&app, &st, e))?
        .body(bytes);
    let done = st.relay.json(req).await.map_err(|e| net_err(&app, &st, e))?;
    st.picked.lock().unwrap().remove(&token);
    Ok(done)
}

/// Creates a real WhatsApp group, which adds and notifies other people. The UI only calls this once the user has
/// seen the name and members and confirmed. It's sent once and never retried, since a try that seemed to fail
/// may have worked.
#[tauri::command]
pub async fn whatsapp_group_create(app: AppHandle, st: St<'_, >, name: String, members: Vec<String>) -> Result<Value, String> {
    let st = st.inner().clone();
    if members.is_empty() || members.iter().any(|m| !media::valid_jid(m) || m.ends_with("@g.us")) {
        return Err("Choose at least one of your contacts.".into());
    }
    let done = st
        .relay
        .post("/api/group/create", &json!({ "name": name, "members": members }))
        .await
        .map_err(|e| net_err(&app, &st, e))?;
    // The new chat has no message yet, so it's added here to open at once.
    if let Some(jid) = done.get("jid").and_then(Value::as_str) {
        let now = std::time::SystemTime::now().duration_since(std::time::UNIX_EPOCH).map(|d| d.as_millis() as i64).unwrap_or(0);
        let chat = crate::db::WireChat { jid: jid.to_string(), name: done.get("name").and_then(Value::as_str).unwrap_or("").to_string(), last_ms: now, last_read_ms: now };
        let _ = st.cache.apply_batch(&[], &[chat], None);
    }
    Ok(done)
}

/// A name of the user's own for someone, shown in WA Relay and the connector only.
#[tauri::command]
pub async fn contact_name_set(app: AppHandle, st: St<'_, >, jid: String, name: String) -> Result<Value, String> {
    let st = st.inner().clone();
    if !media::valid_jid(&jid) || jid.ends_with("@g.us") {
        return Err("That isn't a person.".into());
    }
    let done = st.relay.post("/api/name", &json!({ "jid": jid, "name": name })).await.map_err(|e| net_err(&app, &st, e))?;
    let _ = net::refresh_contacts(&app, &st).await;
    Ok(done)
}

#[tauri::command]
pub async fn contact_name_remove(app: AppHandle, st: St<'_, >, jid: String) -> Result<Value, String> {
    let st = st.inner().clone();
    if !media::valid_jid(&jid) || jid.ends_with("@g.us") {
        return Err("That isn't a person.".into());
    }
    let query: String = url::form_urlencoded::byte_serialize(jid.as_bytes()).collect();
    let req = st.relay.request(reqwest::Method::DELETE, &format!("/api/name?jid={query}")).map_err(|e| net_err(&app, &st, e))?;
    let done = st.relay.json(req).await.map_err(|e| net_err(&app, &st, e))?;
    let _ = net::refresh_contacts(&app, &st).await;
    Ok(done)
}

async fn group_action(app: &AppHandle, st: &Arc<AppState>, action: &str, jid: &str, body: Value) -> Result<Value, String> {
    if !media::valid_jid(jid) || !jid.ends_with("@g.us") {
        return Err("Bad chat.".into());
    }
    let query: String = url::form_urlencoded::byte_serialize(jid.as_bytes()).collect();
    st.relay.post(&format!("/api/group/{action}?jid={query}"), &body).await.map_err(|e| net_err(app, st, e))
}

/// Everyone in the group sees this. The app only calls it after the user confirms.
#[tauri::command]
pub async fn whatsapp_group_rename(app: AppHandle, st: St<'_, >, jid: String, name: String) -> Result<Value, String> {
    let st = st.inner().clone();
    let done = group_action(&app, &st, "rename", &jid, json!({ "name": name })).await?;
    // The change also arrives by sync. This just shows it at once.
    if let Some(new_name) = done.get("name").and_then(Value::as_str) {
        let chat = crate::db::WireChat { jid: jid.clone(), name: new_name.to_string(), last_ms: 0, last_read_ms: 0 };
        let _ = st.cache.apply_batch(&[], &[chat], None);
        let _ = app.emit("chats-changed", json!({ "chats": [jid] }));
    }
    Ok(done)
}

#[tauri::command]
pub async fn whatsapp_group_leave(app: AppHandle, st: St<'_, >, jid: String) -> Result<Value, String> {
    let st = st.inner().clone();
    group_action(&app, &st, "leave", &jid, json!({})).await
}

fn members_key(jid: &str) -> String {
    format!("members:{jid}")
}

fn member_rows(st: &AppState, stored: &str) -> Value {
    let doc: Value = serde_json::from_str(stored).unwrap_or(Value::Null);
    let wire: Vec<crate::db::WireMember> = doc.get("members").cloned().and_then(|m| serde_json::from_value(m).ok()).unwrap_or_default();
    let mut rows: Vec<_> = wire.iter().map(|m| st.cache.member_row(m)).collect();
    rows.sort_by_cached_key(|m| (!m.you, !m.admin, m.name.to_lowercase()));
    json!({ "members": rows, "fetched_ms": doc.get("fetched_ms").cloned().unwrap_or(Value::Null) })
}

/// The last member list this PC saw, for instant display and offline. Names are worked out now, so they
/// stay current.
#[tauri::command(async)]
pub fn group_members(st: St<'_>, jid: String) -> Value {
    let stored = st.cache.meta_get(&members_key(&jid)).ok().flatten().unwrap_or_default();
    member_rows(&st, &stored)
}

/// Everyone who has written in a chat, so a person's colour doesn't depend on how far back the user has scrolled.
#[tauri::command(async)]
pub fn chat_senders(st: St<'_>, jid: String) -> Vec<String> {
    st.cache.chat_senders(&jid).unwrap_or_default()
}

#[tauri::command]
pub async fn group_members_refresh(app: AppHandle, st: St<'_, >, jid: String) -> Result<Value, String> {
    let st = st.inner().clone();
    if !media::valid_jid(&jid) || !jid.ends_with("@g.us") {
        return Err("Bad chat.".into());
    }
    let query: String = url::form_urlencoded::byte_serialize(jid.as_bytes()).collect();
    let got = st.relay.get(&format!("/api/group/members?jid={query}")).await.map_err(|e| net_err(&app, &st, e))?;
    let now = std::time::SystemTime::now().duration_since(std::time::UNIX_EPOCH).map(|d| d.as_millis() as i64).unwrap_or(0);
    let stored = json!({ "members": got.get("members").cloned().unwrap_or(json!([])), "fetched_ms": now }).to_string();
    let _ = st.cache.meta_set(&members_key(&jid), &stored);
    Ok(member_rows(&st, &stored))
}

#[tauri::command]
pub async fn picture_remove(app: AppHandle, st: St<'_, >, jid: String) -> Result<(), String> {
    let st = st.inner().clone();
    if !media::valid_jid(&jid) {
        return Err("Bad chat.".into());
    }
    let query: String = url::form_urlencoded::byte_serialize(jid.as_bytes()).collect();
    let req = st
        .relay
        .request(reqwest::Method::DELETE, &format!("/api/picture/own?jid={query}"))
        .map_err(|e| net_err(&app, &st, e))?;
    st.relay.json(req).await.map_err(|e| net_err(&app, &st, e))?;
    Ok(())
}

#[tauri::command(async)]
pub fn update_state(app: AppHandle, st: St<'_>) -> Value {
    update::status(&app, &st)
}

#[tauri::command(async)]
pub fn update_check(st: St<'_>) -> Result<(), String> {
    st.updates.request_check()
}

#[tauri::command]
/// keep_unsent: the user saw the list of unsent things and chose to go ahead. The page only offers it when
/// everything listed is kept through the restart; a file still being sent always holds it back.
pub async fn update_install(app: AppHandle, st: St<'_, >, keep_unsent: Option<bool>) -> Result<(), String> {
    let st = st.inner().clone();
    update::install_now(&app, &st, keep_unsent.unwrap_or(false)).await
}

/// The UI says when it holds something a restart would lose, such as an unsent draft.
#[tauri::command(async)]
pub fn update_set_busy(st: St<'_>, busy: bool) {
    st.updates.set_ui_busy(busy);
}

pub fn handlers() -> impl Fn(tauri::ipc::Invoke) -> bool + Send + Sync + 'static {
    tauri::generate_handler![
        auth_state, conn_state, open_login, sign_out, chat_list, messages_page, messages_after, messages_around,
        search, set_active_chat, mark_read, send_text, react, load_older, pick_files, save_pasted, forget_token,
        send_file, media_fetch, media_save, media_open, open_link, contacts, groups_get, group_create, group_update,
        relay_get, relay_set, group_delete, assign, claude_api, sort_start, settings_get, settings_set, cache_stats, update_state,
        update_check, update_install, update_set_busy, take_open_target, silence_state, silence_set, silence_clear, blink_set, attention_test, pictures_get, pick_picture, picture_set, picture_remove, picture_group_access, picture_group_set, whatsapp_group_create,
        group_members, group_members_refresh, chat_senders, contact_name_set, contact_name_remove,
        whatsapp_group_rename, whatsapp_group_leave, edit_message, revoke_message, hide_message, forward_message,
        cards_get, card_save, card_delete, card_history, card_undo, card_suggest, card_import, pick_vcard
    ]
}

#[cfg(test)]
mod tests {
    use super::merge_read;
    use serde_json::json;

    #[test]
    fn waiting_reads_merge_per_chat() {
        let mut all = serde_json::Map::new();
        merge_read(&mut all, json!({"chat": "a@s.whatsapp.net", "up_to_ms": 10, "receipts": true, "messages": [{"id": "1"}, {"id": "2"}]}));
        merge_read(&mut all, json!({"chat": "a@s.whatsapp.net", "up_to_ms": 30, "receipts": true, "messages": [{"id": "3"}, {"id": "2"}]}));
        merge_read(&mut all, json!({"chat": "b@g.us", "up_to_ms": 5, "receipts": false, "messages": [{"id": "9", "sender_jid": "x"}]}));
        assert_eq!(all.len(), 2);
        let a = &all["a@s.whatsapp.net"];
        assert_eq!(a["up_to_ms"], 30);
        let ids: Vec<&str> = a["messages"].as_array().unwrap().iter().map(|m| m["id"].as_str().unwrap()).collect();
        assert_eq!(ids, ["3", "2", "1"]);
        // An older read arriving late doesn't move the point back.
        merge_read(&mut all, json!({"chat": "a@s.whatsapp.net", "up_to_ms": 20, "receipts": true, "messages": []}));
        assert_eq!(all["a@s.whatsapp.net"]["up_to_ms"], 30);
        assert_eq!(all["a@s.whatsapp.net"]["messages"].as_array().unwrap().len(), 3);
    }
}
