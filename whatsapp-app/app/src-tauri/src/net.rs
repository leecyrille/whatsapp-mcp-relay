//! Talks to the relay on the home PC, and keeps the local cache in step with it.
//!
//! Cloudflare Access sits in front of everything. Signing in happens once, in a window inside the
//! app, and the login cookie stays in the WebView's own store. This code reads that cookie and sends
//! it on its own requests; it is never written anywhere else.

use crate::db::{WireChat, WireContact, WireMsg};
use crate::link::{Reach, SILENCE};
use crate::toast::Target;
use crate::AppState;
use futures_util::StreamExt;
use serde::Deserialize;
use serde_json::Value;
use std::sync::atomic::Ordering;
use std::sync::Arc;
use std::time::Duration;
use tauri::{AppHandle, Emitter, Manager};

pub const CLIENT_HEADER: &str = "X-WA-Client";
const NO_HEARTBEAT: &str = "stream: no heartbeat";
const STREAM_PREFIX: &str = "stream:";
/// A home PC that accepts the connection but never answers must not leave this waiting forever.
const STREAM_CONNECT_TIMEOUT: Duration = Duration::from_secs(25);

#[derive(Debug)]
pub enum NetError {
    /// The login is missing or expired.
    NeedLogin,
    /// The home PC or the network can't be reached.
    Offline(String),
    /// The server answered with an error.
    Server(u16, String),
}

impl NetError {
    pub fn message(&self) -> String {
        match self {
            NetError::NeedLogin => "You need to sign in again.".into(),
            NetError::Offline(_) => "Can't reach your home PC. Check the connection and try again.".into(),
            NetError::Server(_, msg) => msg.clone(),
        }
    }
}

pub struct Relay {
    pub base: String,
    http: reqwest::Client,
    stream_http: reqwest::Client,
    cookie: std::sync::RwLock<Option<String>>,
}

impl Relay {
    pub fn new(base: String, version: &str) -> Relay {
        // Redirects are never followed: a redirect from Access means the login has lapsed, and the
        // cookie must not travel to wherever it points.
        let build = |timeout: Option<Duration>| {
            let mut b = reqwest::Client::builder()
                .redirect(reqwest::redirect::Policy::none())
                .connect_timeout(Duration::from_secs(15))
                .user_agent(format!("wa-relay-app/{version}"))
                .gzip(true);
            if let Some(t) = timeout {
                b = b.timeout(t);
            }
            b.build().expect("http client")
        };
        Relay {
            base,
            http: build(Some(Duration::from_secs(90))),
            stream_http: build(None),
            cookie: std::sync::RwLock::new(None),
        }
    }

    pub fn set_cookie(&self, cookie: Option<String>) {
        *self.cookie.write().unwrap() = cookie;
    }

    pub fn cookie(&self) -> Option<String> {
        self.cookie.read().unwrap().clone()
    }

    pub fn has_cookie(&self) -> bool {
        self.cookie.read().unwrap().is_some()
    }

    fn with_auth(&self, req: reqwest::RequestBuilder) -> Result<reqwest::RequestBuilder, NetError> {
        let cookie = self.cookie.read().unwrap().clone().ok_or(NetError::NeedLogin)?;
        Ok(req.header(reqwest::header::COOKIE, cookie).header(CLIENT_HEADER, "1"))
    }

    pub fn request(&self, method: reqwest::Method, path: &str) -> Result<reqwest::RequestBuilder, NetError> {
        self.with_auth(self.http.request(method, format!("{}{}", self.base, path)))
    }

    /// For a request that may run long: a streamed response or a large upload chunk.
    pub fn long_request(&self, method: reqwest::Method, path: &str) -> Result<reqwest::RequestBuilder, NetError> {
        self.with_auth(self.stream_http.request(method, format!("{}{}", self.base, path)))
    }

    pub async fn send(&self, req: reqwest::RequestBuilder) -> Result<reqwest::Response, NetError> {
        let resp = req.send().await.map_err(|e| NetError::Offline(e.to_string()))?;
        let status = resp.status();
        if status.is_redirection() || status == 401 || status == 403 {
            return Err(NetError::NeedLogin);
        }
        Ok(resp)
    }

    pub async fn json(&self, req: reqwest::RequestBuilder) -> Result<Value, NetError> {
        let resp = self.send(req).await?;
        let status = resp.status().as_u16();
        let body: Value = resp.json().await.map_err(|e| NetError::Offline(e.to_string()))?;
        if status >= 400 {
            let msg = body.get("error").and_then(Value::as_str).unwrap_or("Something went wrong.").to_string();
            return Err(NetError::Server(status, msg));
        }
        Ok(body)
    }

    pub async fn get(&self, path: &str) -> Result<Value, NetError> {
        self.json(self.request(reqwest::Method::GET, path)?).await
    }

    pub async fn post(&self, path: &str, body: &Value) -> Result<Value, NetError> {
        self.json(self.request(reqwest::Method::POST, path)?.json(body)).await
    }
}

/// Reads the login cookie from the WebView. Must not run on the main thread: on Windows that
/// deadlocks, so it is only called from background tasks.
pub fn read_cookie(app: &AppHandle, base: &str) -> Option<String> {
    let url: url::Url = base.parse().ok()?;
    let window = app.get_webview_window("main")?;
    let cookies = window.cookies_for_url(url).ok()?;
    let parts: Vec<String> = cookies
        .iter()
        .filter(|c| c.name().starts_with("CF_"))
        .map(|c| format!("{}={}", c.name(), c.value()))
        .collect();
    if parts.iter().any(|p| p.starts_with("CF_Authorization=")) {
        Some(parts.join("; "))
    } else {
        None
    }
}

#[derive(Deserialize)]
struct SyncPage {
    #[serde(default)]
    messages: Vec<WireMsg>,
    #[serde(default)]
    chats: Vec<WireChat>,
    next: i64,
    head: i64,
    more: bool,
}

#[derive(Deserialize)]
struct Delta {
    #[serde(default)]
    prev: i64,
    #[serde(default)]
    seq: i64,
    #[serde(default)]
    messages: Vec<WireMsg>,
    #[serde(default)]
    chats: Vec<WireChat>,
}

fn emit_conn(app: &AppHandle, st: &AppState) {
    let _ = app.emit("conn", st.conn_json());
}

pub fn need_login(app: &AppHandle, st: &AppState) {
    st.relay.set_cookie(None);
    st.need_login.store(true, Ordering::SeqCst);
    st.online.store(false, Ordering::SeqCst);
    emit_conn(app, st);
}

/// Holds the event stream open for as long as the app runs, catching up on every reconnect.
pub async fn run(app: AppHandle, st: Arc<AppState>) {
    let mut backoff = Duration::from_secs(1);
    loop {
        if !st.relay.has_cookie() {
            let base = st.relay.base.clone();
            let handle = app.clone();
            let found = tauri::async_runtime::spawn_blocking(move || read_cookie(&handle, &base))
                .await
                .ok()
                .flatten();
            match found {
                Some(cookie) => {
                    st.relay.set_cookie(Some(cookie));
                    st.need_login.store(false, Ordering::SeqCst);
                    emit_conn(&app, &st);
                }
                None => {
                    st.need_login.store(true, Ordering::SeqCst);
                    emit_conn(&app, &st);
                    // Woken when the sign-in window finishes.
                    tokio::select! {
                        _ = st.auth_changed.notified() => {}
                        _ = tokio::time::sleep(Duration::from_secs(30)) => {}
                    }
                    continue;
                }
            }
        }
        let started = std::time::Instant::now();
        let result = stream_once(&app, &st).await;
        st.online.store(false, Ordering::SeqCst);
        if let Err(NetError::NeedLogin) = result {
            need_login(&app, &st);
            continue;
        }
        if let Err(e) = &result {
            st.mark_lost(reach_of(e), matches!(e, NetError::Offline(m) if m == NO_HEARTBEAT));
        }
        emit_conn(&app, &st);
        if started.elapsed() > Duration::from_secs(30) {
            backoff = Duration::from_secs(1);
        }
        tokio::select! {
            _ = st.auth_changed.notified() => {}
            _ = tokio::time::sleep(backoff) => {}
        }
        // Kept short, so the warning clears soon after the home PC comes back.
        backoff = (backoff * 2).min(Duration::from_secs(10));
    }
}

/// What a failed connection says about where the break is.
fn reach_of(e: &NetError) -> Reach {
    match e {
        // The stream was up and then went away, so the connection itself worked.
        NetError::Offline(message) if message.starts_with(STREAM_PREFIX) => Reach::HomeDown,
        NetError::Offline(_) => Reach::NoInternet,
        // Cloudflare answered, and cloudflared couldn't get an answer from the relay.
        NetError::Server(502 | 503 | 504, _) => Reach::RelayDown,
        _ => Reach::HomeDown,
    }
}

async fn stream_once(app: &AppHandle, st: &Arc<AppState>) -> Result<(), NetError> {
    let req = st.relay.long_request(reqwest::Method::GET, "/api/events")?;
    let resp = match tokio::time::timeout(STREAM_CONNECT_TIMEOUT, st.relay.send(req)).await {
        Ok(result) => result?,
        Err(_) => return Err(NetError::Offline(NO_HEARTBEAT.into())),
    };
    if resp.status() != 200 {
        return Err(NetError::Server(resp.status().as_u16(), "event stream refused".into()));
    }
    st.online.store(true, Ordering::SeqCst);
    st.mark_connected();
    st.need_login.store(false, Ordering::SeqCst);
    emit_conn(app, st);
    // Catch up in the background, so the stream stays drained while a first sync runs.
    {
        let (app, st) = (app.clone(), st.clone());
        tauri::async_runtime::spawn(async move {
            let _ = refresh_contacts(&app, &st).await;
            let _ = refresh_groups(&app, &st).await;
            let _ = refresh_pictures(&app, &st).await;
            let _ = pull(&app, &st).await;
        });
    }
    let mut stream = resp.bytes_stream();
    let mut buffer: Vec<u8> = Vec::new();
    loop {
        // The relay sends a heartbeat every 10 seconds. Silence for longer means the connection is
        // dead, as it is after the laptop sleeps, even if nothing has reported an error.
        let chunk = match tokio::time::timeout(SILENCE, stream.next()).await {
            Err(_) => return Err(NetError::Offline(NO_HEARTBEAT.into())),
            Ok(None) => return Err(NetError::Offline("stream: ended".into())),
            Ok(Some(Err(e))) => return Err(NetError::Offline(format!("{STREAM_PREFIX} {e}"))),
            Ok(Some(Ok(bytes))) => bytes,
        };
        st.mark_heard();
        buffer.extend_from_slice(&chunk);
        while let Some(end) = find_event_end(&buffer) {
            let event: Vec<u8> = buffer.drain(..end + 2).collect();
            for line in event.split(|b| *b == b'\n') {
                if let Some(data) = line.strip_prefix(b"data: ") {
                    if let Ok(value) = serde_json::from_slice::<Value>(data) {
                        handle_event(app, st, value).await;
                    }
                }
            }
        }
    }
}

fn find_event_end(buf: &[u8]) -> Option<usize> {
    buf.windows(2).position(|w| w == b"\n\n")
}

async fn handle_event(app: &AppHandle, st: &Arc<AppState>, value: Value) {
    let kind = value.get("type").and_then(Value::as_str).unwrap_or("").to_string();
    match kind.as_str() {
        "status" => {
            let connected = value.get("connected").and_then(Value::as_bool).unwrap_or(false);
            let logged_in = value.get("logged_in").and_then(Value::as_bool).unwrap_or(false);
            let bridge_down = value.get("bridge_down").and_then(Value::as_bool).unwrap_or(false);
            st.mark_status(connected, logged_in, bridge_down);
            let own: Vec<String> = ["me_pn", "me_lid"]
                .iter()
                .filter_map(|k| value.get(*k).and_then(Value::as_str))
                .filter(|s| !s.is_empty())
                .map(str::to_string)
                .collect();
            if !own.is_empty() {
                let _ = st.cache.set_own(own);
            }
            emit_conn(app, st);
            let head = value.get("head").and_then(Value::as_i64).unwrap_or(0);
            if head > st.cache.cursor() {
                spawn_pull(app, st);
            } else if connected && logged_in {
                st.caught_up.store(true, Ordering::SeqCst);
            }
        }
        "delta" => {
            let Ok(delta) = serde_json::from_value::<Delta>(value) else { return };
            let cursor = st.cache.cursor();
            if delta.seq <= cursor {
                return;
            }
            if delta.prev > cursor {
                spawn_pull(app, st); // Something was missed. Sequence numbers make that safe to fix.
                return;
            }
            apply(app, st, delta.messages, delta.chats, Some(delta.seq), true).await;
        }
        "changed" => {
            let seq = value.get("seq").and_then(Value::as_i64).unwrap_or(0);
            if seq > st.cache.cursor() {
                spawn_pull(app, st);
            }
        }
        "contacts" => {
            let (app, st) = (app.clone(), st.clone());
            tauri::async_runtime::spawn(async move {
                let _ = refresh_contacts(&app, &st).await;
            });
        }
        "groups" => {
            let _ = st.cache.meta_set("groups", &value.to_string());
            let _ = app.emit("groups", value);
        }
        "group_members" => {
            let _ = app.emit("group-members-changed", value.get("jid").and_then(Value::as_str).unwrap_or(""));
        }
        "picture" => {
            let jid = value.get("jid").and_then(Value::as_str).unwrap_or("");
            st.avatar_misses.lock().unwrap().remove(jid);
            let mut doc: Value = st.cache.meta_get("pictures").ok().flatten().and_then(|r| serde_json::from_str(&r).ok()).unwrap_or_else(|| serde_json::json!({}));
            apply_picture_event(&mut doc, &value);
            let _ = st.cache.meta_set("pictures", &doc.to_string());
            let _ = app.emit("pictures", doc);
        }
        "send_result" => {
            if let Some(message) = value.get("message").cloned() {
                if let Ok(m) = serde_json::from_value::<WireMsg>(message) {
                    apply(app, st, vec![m], vec![], None, false).await;
                }
            }
            let _ = app.emit("send-result", value);
        }
        "forward_result" => {
            let _ = app.emit("forward-result", value);
        }
        "cards" => {
            let _ = app.emit("cards-changed", ());
        }
        "claude" | "claude_sort" => {
            let pending = value.get("kind").and_then(Value::as_str) == Some("item")
                && value.pointer("/item/kind").and_then(Value::as_str) == Some("approval")
                && value.pointer("/item/state").and_then(Value::as_str) == Some("pending");
            if pending {
                crate::toast::show(app, st, "Claude needs your approval", "Open WA Relay to approve or deny it.", &Target::Claude);
            }
            let _ = app.emit("claude-event", value);
        }
        "claude_status" => {
            let up = value.get("up").and_then(Value::as_bool).unwrap_or(false);
            st.claude_up.store(up, Ordering::SeqCst);
            emit_conn(app, st);
        }
        _ => {}
    }
}

fn spawn_pull(app: &AppHandle, st: &Arc<AppState>) {
    let (app, st) = (app.clone(), st.clone());
    tauri::async_runtime::spawn(async move {
        let _ = pull(&app, &st).await;
    });
}

/// Fetches everything after the cursor. Only one pull runs at a time; a request that arrives during
/// one sets a flag, so the running pull goes round again.
pub async fn pull(app: &AppHandle, st: &Arc<AppState>) -> Result<(), NetError> {
    if st.pulling.swap(true, Ordering::SeqCst) {
        st.pull_again.store(true, Ordering::SeqCst);
        return Ok(());
    }
    let result = pull_loop(app, st).await;
    st.pulling.store(false, Ordering::SeqCst);
    if result.is_ok() {
        st.caught_up.store(true, Ordering::SeqCst);
        tauri::async_runtime::spawn(crate::commands::flush_pending_reads(app.clone(), st.clone()));
    }
    if result.is_err() {
        if let Err(NetError::NeedLogin) = result {
            need_login(app, st);
        }
    }
    let _ = app.emit("sync-progress", serde_json::json!({"syncing": false, "cursor": st.cache.cursor()}));
    result
}

async fn pull_loop(app: &AppHandle, st: &Arc<AppState>) -> Result<(), NetError> {
    loop {
        st.pull_again.store(false, Ordering::SeqCst);
        loop {
            let cursor = st.cache.cursor();
            let req = st.relay.request(reqwest::Method::GET, &format!("/api/sync?after={cursor}&limit=2000"))?;
            let resp = st.relay.send(req).await?;
            let status = resp.status().as_u16();
            if status != 200 {
                return Err(NetError::Server(status, "sync refused".into()));
            }
            let page: SyncPage = resp.json().await.map_err(|e| NetError::Offline(e.to_string()))?;
            let (next, head, more) = (page.next, page.head, page.more);
            apply(app, st, page.messages, page.chats, Some(next), false).await;
            let _ = app.emit("sync-progress", serde_json::json!({"syncing": more, "cursor": next, "head": head}));
            if !more {
                break;
            }
        }
        if !st.pull_again.load(Ordering::SeqCst) {
            return Ok(());
        }
    }
}

/// Writes rows off the async threads, then tells the UI what changed. A live delta can also notify.
async fn apply(app: &AppHandle, st: &Arc<AppState>, messages: Vec<WireMsg>, chats: Vec<WireChat>, cursor: Option<i64>, live: bool) {
    let cache = st.cache.clone();
    let notify_candidates: Vec<WireMsg> = if live {
        messages.iter().filter(|m| !m.from_me && !m.revoked).cloned().collect()
    } else {
        vec![]
    };
    let touched = tauri::async_runtime::spawn_blocking(move || cache.apply_batch(&messages, &chats, cursor)).await;
    let Ok(Ok(touched)) = touched else { return };
    if touched.is_empty() {
        return;
    }
    let _ = app.emit("chats-changed", serde_json::json!({ "chats": touched }));
    crate::update_tray(app, st);
    for m in notify_candidates {
        notify(app, st, &m);
    }
}

fn notify(app: &AppHandle, st: &AppState, m: &WireMsg) {
    if st.cache.meta_get("setting_notifications").ok().flatten().as_deref() == Some("false") {
        return;
    }
    // Delayed offline delivery shouldn't produce a burst of old notifications.
    let now = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|d| d.as_millis() as i64)
        .unwrap_or(0);
    if now - m.ts > 10 * 60 * 1000 {
        return;
    }
    let canon = st.cache.canon(&m.chat);
    let focused = app
        .get_webview_window("main")
        .and_then(|w| w.is_focused().ok())
        .unwrap_or(false);
    let active = st.active_chat.lock().unwrap().clone();
    if focused && active.as_deref() == Some(canon.as_str()) {
        return;
    }
    let body = match m.media.as_str() {
        "image" if m.text.is_empty() => "📷 Photo".to_string(),
        "video" if m.text.is_empty() => "🎥 Video".to_string(),
        "audio" => "🎤 Audio".to_string(),
        "sticker" => "Sticker".to_string(),
        "document" if m.text.is_empty() => format!("📄 {}", m.filename),
        _ => m.text.clone(),
    };
    let body: String = st.cache.with_mention_names(&body).chars().take(200).collect();
    let title = st.cache.chat_name(&canon);
    crate::toast::show(app, st, &title, &body, &Target::Chat { jid: canon });
}

pub async fn refresh_contacts(app: &AppHandle, st: &Arc<AppState>) -> Result<(), NetError> {
    let value = st.relay.get("/api/contacts").await?;
    let contacts: Vec<WireContact> = value
        .get("contacts")
        .cloned()
        .and_then(|v| serde_json::from_value(v).ok())
        .unwrap_or_default();
    let aliases: Vec<(String, String)> = value
        .get("aliases")
        .cloned()
        .and_then(|v| serde_json::from_value(v).ok())
        .unwrap_or_default();
    let cache = st.cache.clone();
    let done = tauri::async_runtime::spawn_blocking(move || cache.set_contacts(&contacts, &aliases)).await;
    if let Ok(Ok(())) = done {
        let _ = app.emit("contacts-changed", ());
        let _ = app.emit("chats-changed", serde_json::json!({ "chats": [] }));
    }
    Ok(())
}

/// Keeps the list of known pictures current, so a PC that was offline shows changes it missed.
pub fn apply_picture_event(doc: &mut Value, event: &Value) {
    let Some(jid) = event.get("jid").and_then(Value::as_str) else { return };
    let version = event.get("v").and_then(Value::as_i64).unwrap_or(0);
    if !doc.is_object() {
        *doc = serde_json::json!({});
    }
    if !doc.get("pictures").is_some_and(Value::is_object) {
        doc["pictures"] = serde_json::json!({});
    }
    doc["pictures"][jid] = serde_json::json!(version);
    let is_own = event.get("own").and_then(Value::as_bool).unwrap_or(false);
    let mut own: Vec<String> = doc
        .get("own")
        .and_then(Value::as_array)
        .map(|list| list.iter().filter_map(Value::as_str).filter(|j| *j != jid).map(str::to_string).collect())
        .unwrap_or_default();
    if is_own {
        own.push(jid.to_string());
    }
    doc["own"] = serde_json::json!(own);
}

pub async fn refresh_pictures(app: &AppHandle, st: &Arc<AppState>) -> Result<(), NetError> {
    let value = st.relay.get("/api/pictures").await?;
    let _ = st.cache.meta_set("pictures", &value.to_string());
    st.avatar_misses.lock().unwrap().clear();
    let _ = app.emit("pictures", value);
    Ok(())
}

pub async fn refresh_groups(app: &AppHandle, st: &Arc<AppState>) -> Result<(), NetError> {
    let value = st.relay.get("/api/groups").await?;
    let _ = st.cache.meta_set("groups", &value.to_string());
    let _ = app.emit("groups", value);
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn a_picture_event_updates_the_list() {
        let mut doc = serde_json::json!({});
        apply_picture_event(&mut doc, &serde_json::json!({"jid": "1@g.us", "v": 5, "own": true}));
        assert_eq!(doc["pictures"]["1@g.us"], 5);
        assert_eq!(doc["own"], serde_json::json!(["1@g.us"]));
        apply_picture_event(&mut doc, &serde_json::json!({"jid": "1@g.us", "v": 6, "own": false}));
        assert_eq!(doc["pictures"]["1@g.us"], 6);
        assert_eq!(doc["own"], serde_json::json!([]));
        apply_picture_event(&mut doc, &serde_json::json!({"nothing": true}));
        assert_eq!(doc["pictures"]["1@g.us"], 6);
    }

    #[test]
    fn failures_say_where_the_break_is() {
        assert_eq!(reach_of(&NetError::Offline("dns error".into())), Reach::NoInternet);
        assert_eq!(reach_of(&NetError::Offline(NO_HEARTBEAT.into())), Reach::HomeDown);
        assert_eq!(reach_of(&NetError::Server(530, String::new())), Reach::HomeDown);
        assert_eq!(reach_of(&NetError::Server(502, String::new())), Reach::RelayDown);
    }

    #[test]
    fn finds_event_boundaries() {
        assert_eq!(find_event_end(b"data: x\n\ndata: y"), Some(7));
        assert_eq!(find_event_end(b"data: partial\n"), None);
    }
}
