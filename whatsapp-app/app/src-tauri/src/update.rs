//! Automatic updates. Each check asks the public release feed first (GitHub releases, with no login and
//! no cookie), and only if that can't be reached, the home PC's relay with the app's own login, as before.
//!
//! An update is downloaded and checked in the background, then kept in memory until it can be installed
//! without getting in the way: when the user presses the button, or when the window has been closed for a
//! while and nothing is in progress. The signature check against the key built into this app is what
//! makes an update trustworthy, since the installer isn't code-signed. It also covers the version, so an
//! old genuine installer can't be passed off as a new one.
//!
//! A lapsed login or a dead connection is never shown as a problem here. The app already has a banner
//! for those, and the next check picks up where this one stopped.

use crate::net::{NetError, CLIENT_HEADER};
use crate::{test_build, test_override, AppState};
use serde::Deserialize;
use serde_json::{json, Value};
use std::io::Write;
use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicBool, AtomicUsize, Ordering};
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant, SystemTime, UNIX_EPOCH};
use tauri::{AppHandle, Emitter, Manager};
use tauri_plugin_updater::{UpdaterBuilder, UpdaterExt};

pub const SETTING: &str = "setting_auto_update";
const MARKER: &str = "after-update.txt";
const MARKER_MAX_AGE_SECS: u64 = 300;
const LOG: &str = "update.log";
const LOG_MAX_BYTES: u64 = 256 << 10;
const DOWNLOAD_TIMEOUT: Duration = Duration::from_secs(600);

/// Where releases for everyone are published. A build can name its own feed with `WA_UPDATE_FEED` set when
/// it's compiled, for a fork that publishes elsewhere.
pub const PUBLIC_FEED: &str = match option_env!("WA_UPDATE_FEED") {
    Some(feed) => feed,
    None => "https://github.com/leecyrille/whatsapp-mcp-relay/releases/latest/download/latest.json",
};

/// The public feed for this copy. Test builds only use one a test points them at, so ordinary test runs
/// never reach the real one.
fn public_feed() -> Option<url::Url> {
    let raw = if test_build() { test_override("WA_UPDATE_FEED")? } else { PUBLIC_FEED.to_string() };
    let url: url::Url = raw.parse().ok()?;
    let local_test = test_build() && matches!(url.host_str(), Some("127.0.0.1" | "localhost"));
    (url.scheme() == "https" || (url.scheme() == "http" && local_test)).then_some(url)
}

/// Where an installer from the public feed may come from, including GitHub's redirect to its file store.
/// Nothing from the feed is ever sent the login cookie.
fn public_host_ok(host: &str, feed: &url::Url) -> bool {
    Some(host) == feed.host_str() || host == "github.com" || host.ends_with(".githubusercontent.com")
}

fn public_url_ok(url: &url::Url, feed: &url::Url) -> bool {
    let scheme_ok = url.scheme() == "https" || (url.scheme() == feed.scheme() && url.host_str() == feed.host_str());
    scheme_ok && url.host_str().is_some_and(|h| public_host_ok(h, feed)) && url.username().is_empty() && url.password().is_none()
}

struct Timing {
    first: Duration,
    every: Duration,
    retry: Duration,
    idle: Duration,
    tick: Duration,
}

impl Timing {
    fn normal() -> Timing {
        Timing {
            first: Duration::from_secs(60),
            every: Duration::from_secs(6 * 3600),
            retry: Duration::from_secs(30 * 60),
            idle: Duration::from_secs(5 * 60),
            tick: Duration::from_secs(30),
        }
    }

    /// Test builds set `WA_UPDATE_TEST` to "first,every,retry,idle" in seconds. Without it, they don't
    /// update at all, so ordinary test runs never go looking for one.
    fn for_test() -> Option<Timing> {
        let raw = test_override("WA_UPDATE_TEST")?;
        let parts: Vec<u64> = raw.split(',').filter_map(|p| p.trim().parse().ok()).collect();
        let [first, every, retry, idle] = parts[..] else { return None };
        Some(Timing {
            first: Duration::from_secs(first),
            every: Duration::from_secs(every),
            retry: Duration::from_secs(retry),
            idle: Duration::from_secs(idle),
            tick: Duration::from_millis(250),
        })
    }
}

/// A checked update, waiting to be installed.
struct Staged {
    version: String,
    manifest: Value,
    bytes: Vec<u8>,
}

pub struct Updates {
    data_dir: PathBuf,
    status: Mutex<(&'static str, String)>,
    staged: Mutex<Option<Staged>>,
    check_now: tokio::sync::Notify,
    ui_busy: AtomicBool,
    sending: AtomicUsize,
    active: AtomicBool,
    installing: AtomicBool,
}

impl Updates {
    pub fn new(data_dir: PathBuf) -> Updates {
        Updates {
            data_dir,
            status: Mutex::new(("idle", String::new())),
            staged: Mutex::new(None),
            check_now: tokio::sync::Notify::new(),
            ui_busy: AtomicBool::new(false),
            sending: AtomicUsize::new(0),
            active: AtomicBool::new(false),
            installing: AtomicBool::new(false),
        }
    }

    pub fn request_check(&self) -> Result<(), String> {
        if !self.active.load(Ordering::SeqCst) {
            return Err("Updates are off in this test build.".into());
        }
        self.check_now.notify_one();
        Ok(())
    }

    pub fn set_ui_busy(&self, busy: bool) {
        self.ui_busy.store(busy, Ordering::SeqCst);
    }

    /// Held for as long as a file is being sent, since a restart would drop it.
    pub fn sending(self: &Arc<Self>) -> SendGuard {
        self.sending.fetch_add(1, Ordering::SeqCst);
        SendGuard(self.clone())
    }

    fn status_json(&self, current: &str) -> Value {
        let (state, version) = self.status.lock().unwrap().clone();
        // held: whether something in progress is holding back an automatic install right now.
        json!({ "state": state, "version": version, "current": current, "held": self.busy() })
    }

    fn has_staged(&self) -> bool {
        self.staged.lock().unwrap().is_some()
    }

    fn busy(&self) -> bool {
        self.ui_busy.load(Ordering::SeqCst) || self.uploading()
    }

    fn uploading(&self) -> bool {
        self.sending.load(Ordering::SeqCst) > 0
    }

    pub fn log(&self, line: &str) {
        let path = self.data_dir.join(LOG);
        if std::fs::metadata(&path).map(|m| m.len() > LOG_MAX_BYTES).unwrap_or(false) {
            let _ = std::fs::remove_file(&path);
        }
        if let Ok(mut file) = std::fs::OpenOptions::new().create(true).append(true).open(&path) {
            let _ = writeln!(file, "{} {line}", now_secs());
        }
    }
}

pub struct SendGuard(Arc<Updates>);

impl Drop for SendGuard {
    fn drop(&mut self) {
        self.0.sending.fetch_sub(1, Ordering::SeqCst);
    }
}

fn now_secs() -> u64 {
    SystemTime::now().duration_since(UNIX_EPOCH).map(|d| d.as_secs()).unwrap_or(0)
}

/// What the installer's restart should do. A quiet update must not pop the window up, and one the user asked
/// for must show it, whatever flags the running copy was started with.
fn write_marker(dir: &Path, show: bool) {
    let word = if show { "show" } else { "hide" };
    let _ = std::fs::write(dir.join(MARKER), format!("{word} {}", now_secs()));
}

fn parse_marker(text: &str, now: u64) -> Option<bool> {
    let (word, at) = text.trim().split_once(' ')?;
    let at: u64 = at.parse().ok()?;
    if now.saturating_sub(at) > MARKER_MAX_AGE_SECS {
        return None; // The install didn't finish then. This is an ordinary start.
    }
    match word {
        "show" => Some(false),
        "hide" => Some(true),
        _ => None,
    }
}

/// Whether the installer's restart asked to start hidden. None means this isn't one.
pub fn take_restart_marker(dir: &Path) -> Option<bool> {
    let path = dir.join(MARKER);
    let text = std::fs::read_to_string(&path).ok()?;
    let _ = std::fs::remove_file(&path);
    parse_marker(&text, now_secs())
}

/// The whole rule for installing without being asked. An open window always wins: the user may be reading
/// or typing, so it waits however long that takes.
pub fn may_install_quietly(enabled: bool, window_open: bool, hidden_for: Option<Duration>, need: Duration, busy: bool) -> bool {
    enabled && !window_open && !busy && hidden_for.is_some_and(|h| h >= need)
}

fn enabled(st: &AppState) -> bool {
    st.cache.meta_get(SETTING).ok().flatten().as_deref() != Some("false")
}

fn current_version(app: &AppHandle) -> String {
    app.package_info().version.to_string()
}

fn set_status(app: &AppHandle, st: &AppState, state: &'static str, version: &str) {
    *st.updates.status.lock().unwrap() = (state, version.to_string());
    let _ = app.emit("update-state", st.updates.status_json(&current_version(app)));
}

pub fn status(app: &AppHandle, st: &AppState) -> Value {
    st.updates.status_json(&current_version(app))
}

fn builder(app: &AppHandle, base: &str) -> Result<UpdaterBuilder, tauri_plugin_updater::Error> {
    // The updater wants an endpoint, though every check here is made by hand and handed to it.
    let endpoint: url::Url = if base.is_empty() {
        public_feed().map(|u| u.to_string()).unwrap_or_else(|| PUBLIC_FEED.to_string()).parse()?
    } else {
        format!("{base}/api/update/latest").parse()?
    };
    app.updater_builder().endpoints(vec![endpoint])
}

/// latest.json as Tauri writes it, of which only the Windows entry matters here.
#[derive(Deserialize)]
struct PublicLatest {
    version: String,
    platforms: std::collections::HashMap<String, PublicPlatform>,
}

#[derive(Deserialize)]
struct PublicPlatform {
    signature: String,
    url: String,
}

/// What a feed offered. The cookie is only ever the relay's own, for the relay's own address.
struct Offer {
    version: String,
    manifest: Value,
    cookie: Option<String>,
    public: Option<url::Url>,
}

fn redirects_within(feed: url::Url) -> reqwest::redirect::Policy {
    reqwest::redirect::Policy::custom(move |attempt| {
        let ok = attempt.previous().len() < 5 && public_url_ok(attempt.url(), &feed);
        if ok {
            attempt.follow()
        } else {
            attempt.stop()
        }
    })
}

/// Asks the public feed. Ok(None) means it answered with nothing for Windows.
async fn public_offer(st: &AppState, version: &str) -> Result<Option<Offer>, String> {
    let feed = public_feed().ok_or("no public feed")?;
    let client = reqwest::Client::builder()
        .redirect(redirects_within(feed.clone()))
        .connect_timeout(Duration::from_secs(15))
        .timeout(Duration::from_secs(30))
        .user_agent(format!("wa-relay-app/{version}"))
        .build()
        .map_err(|e| e.to_string())?;
    let resp = client.get(feed.clone()).send().await.map_err(|e| format!("unreachable: {e}"))?;
    if !resp.status().is_success() {
        return Err(format!("status {}", resp.status().as_u16()));
    }
    let latest: PublicLatest = resp.json().await.map_err(|e| format!("unreadable: {e}"))?;
    let Some(entry) = latest.platforms.get("windows-x86_64") else { return Ok(None) };
    let url: url::Url = entry.url.parse().map_err(|_| "bad installer address".to_string())?;
    if !public_url_ok(&url, &feed) {
        st.updates.log(&format!("{} refused: installer at an unexpected address", latest.version));
        return Ok(None);
    }
    Ok(Some(Offer {
        manifest: json!({ "version": latest.version, "url": url.as_str(), "signature": entry.signature }),
        version: latest.version,
        cookie: None,
        public: Some(feed),
    }))
}

#[derive(Deserialize)]
struct Latest {
    version: String,
    signature: String,
    file: String,
}

/// The relay names a file, and the address is built here. So its answer can't send the login cookie
/// anywhere but the relay this app already talks to.
fn manifest_for(base: &str, latest: &Latest) -> Result<Value, String> {
    let mut url: url::Url = base.parse().map_err(|e: url::ParseError| e.to_string())?;
    url.path_segments_mut()
        .map_err(|_| "bad relay address".to_string())?
        .pop_if_empty()
        .extend(["api", "update", "installer", &latest.file]);
    Ok(json!({ "version": latest.version, "url": url.as_str(), "signature": latest.signature }))
}

/// Checks and downloads once, and says how long to wait before the next try.
async fn check(app: &AppHandle, st: &Arc<AppState>, timing: &Timing) -> Duration {
    set_status(app, st, "checking", "");
    let (state, version, wait) = match fetch(app, st).await {
        Ok(Some(version)) => ("ready", version, timing.every),
        Ok(None) => ("up_to_date", String::new(), timing.every),
        Err(Failure::NeedLogin) => ("need_login", String::new(), timing.retry),
        Err(Failure::Offline) => ("offline", String::new(), timing.retry),
        Err(Failure::Refused(version)) => ("refused", version, timing.every),
    };
    set_status(app, st, state, &version);
    wait
}

enum Failure {
    NeedLogin,
    Offline,
    Refused(String),
}

impl From<NetError> for Failure {
    fn from(e: NetError) -> Failure {
        match e {
            NetError::NeedLogin => Failure::NeedLogin,
            _ => Failure::Offline,
        }
    }
}

async fn fetch(app: &AppHandle, st: &Arc<AppState>) -> Result<Option<String>, Failure> {
    let offer = match public_offer(st, &current_version(app)).await {
        Ok(offer) => offer,
        Err(why) => {
            if public_feed().is_some() {
                st.updates.log(&format!("public feed: {why}"));
            }
            if st.relay.base.is_empty() {
                return Err(Failure::Offline);
            }
            relay_offer(st).await?
        }
    };
    let Some(offer) = offer else { return Ok(None) };
    download(app, st, offer).await
}

/// Asks the home PC's relay, with the app's login, as every version before the public feed did.
async fn relay_offer(st: &Arc<AppState>) -> Result<Option<Offer>, Failure> {
    let req = st.relay.request(reqwest::Method::GET, "/api/update/latest")?;
    let resp = st.relay.send(req).await?;
    let status = resp.status().as_u16();
    if status == 204 {
        return Ok(None);
    }
    if status != 200 {
        st.updates.log(&format!("check refused: {status}"));
        return Err(Failure::Offline);
    }
    let latest: Latest = resp.json().await.map_err(|_| Failure::Offline)?;
    let manifest = manifest_for(&st.relay.base, &latest).map_err(|_| Failure::Offline)?;
    let cookie = st.relay.cookie().ok_or(Failure::NeedLogin)?;
    Ok(Some(Offer { version: latest.version, manifest, cookie: Some(cookie), public: None }))
}

async fn download(app: &AppHandle, st: &Arc<AppState>, offer: Offer) -> Result<Option<String>, Failure> {
    let Offer { version: offered, manifest, cookie, public } = offer;
    let refused = |e: tauri_plugin_updater::Error| {
        st.updates.log(&format!("{offered} refused: {e}"));
        Failure::Refused(offered.clone())
    };
    let mut b = builder(app, &st.relay.base).map_err(refused)?;
    if let Some(cookie) = &cookie {
        b = b.header("cookie", cookie.as_str()).and_then(|b| b.header(CLIENT_HEADER, "1")).map_err(refused)?;
    }
    let update = b
        .timeout(DOWNLOAD_TIMEOUT)
        .configure_client(move |c| {
            // From the relay, a redirect means the login lapsed, and the cookie stays here. From the public
            // feed, only GitHub's own redirect to its file store is followed.
            let policy = match &public {
                Some(feed) => {
                    let feed = feed.clone();
                    reqwest013::redirect::Policy::custom(move |attempt| {
                        let ok = attempt.previous().len() < 5 && public_url_ok(attempt.url(), &feed);
                        if ok {
                            attempt.follow()
                        } else {
                            attempt.stop()
                        }
                    })
                }
                None => reqwest013::redirect::Policy::none(),
            };
            c.redirect(policy).user_agent(concat!("wa-relay-app/updater/", env!("CARGO_PKG_VERSION")))
        })
        .build()
        .map_err(refused)?
        .restore_update(manifest.clone())
        .map_err(refused)?;
    let from_relay = cookie.is_some();
    let Some(update) = update else { return Ok(None) };
    set_status(app, st, "downloading", &offered);
    let bytes = match update.download(|_, _| {}, || {}).await {
        Ok(bytes) => bytes,
        Err(tauri_plugin_updater::Error::Reqwest(e)) => {
            st.updates.log(&format!("{} download failed: {e}", offered));
            return Err(Failure::Offline);
        }
        Err(tauri_plugin_updater::Error::Network(e)) => {
            st.updates.log(&format!("{} download failed: {e}", offered));
            let login = from_relay && ["302", "401", "403"].iter().any(|code| e.contains(code));
            return Err(if login { Failure::NeedLogin } else { Failure::Offline });
        }
        Err(e) => return Err(refused(e)),
    };
    st.updates.log(&format!("{} downloaded and verified, {} bytes", offered, bytes.len()));
    let version = offered.clone();
    *st.updates.staged.lock().unwrap() = Some(Staged { version: version.clone(), manifest, bytes });
    Ok(Some(version))
}

/// Claude's jobs run on the home PC and carry on through a restart here, but one may be waiting on
/// the user, so an update waits too. If this can't tell, the job can't be reached from here either way.
async fn claude_running(st: &AppState) -> bool {
    let Ok(req) = st.relay.request(reqwest::Method::GET, "/api/claude/threads") else { return false };
    match st.relay.json(req).await {
        Ok(value) => value
            .get("threads")
            .and_then(Value::as_array)
            .is_some_and(|threads| threads.iter().any(|t| t.get("running").and_then(Value::as_bool) == Some(true))),
        Err(_) => false,
    }
}

/// Starts the installer and ends this process. Returning at all means it failed.
fn install(app: &AppHandle, st: &AppState, show: bool) -> String {
    // Set first: the update has left the queue, and the background loop mustn't fetch it all over again.
    st.updates.installing.store(true, Ordering::SeqCst);
    let Some(staged) = st.updates.staged.lock().unwrap().take() else {
        st.updates.installing.store(false, Ordering::SeqCst);
        return "There's no update ready.".into();
    };
    write_marker(&st.updates.data_dir, show);
    st.updates.log(&format!("installing {}, show={show}", staged.version));
    let result = builder(app, &st.relay.base)
        .and_then(|b| b.build())
        .and_then(|updater| updater.restore_update(staged.manifest))
        .and_then(|update| match update {
            Some(update) => update.install(&staged.bytes),
            None => Err(tauri_plugin_updater::Error::ReleaseNotFound),
        });
    let _ = std::fs::remove_file(st.updates.data_dir.join(MARKER));
    let why = result.err().map(|e| e.to_string()).unwrap_or_default();
    st.updates.log(&format!("install failed: {why}"));
    st.updates.installing.store(false, Ordering::SeqCst);
    "The update couldn't be installed. It will try again later.".into()
}

/// The user pressed the button. Still refuses to lose what's in progress, but says why.
pub async fn install_now(app: &AppHandle, st: &Arc<AppState>, keep_unsent: bool) -> Result<(), String> {
    if !st.updates.has_staged() {
        return Err("There's no update ready.".into());
    }
    if st.updates.uploading() {
        return Err("A file is still being sent. Try again when it's done.".into());
    }
    if st.updates.busy() && !keep_unsent {
        return Err("Something you wrote hasn't been sent. Settings lists it: send it, or discard it.".into());
    }
    if claude_running(st).await {
        return Err("Claude is still working. Try again when it finishes.".into());
    }
    let (handle, state) = (app.clone(), st.clone());
    let failed = tauri::async_runtime::spawn_blocking(move || install(&handle, &state, true))
        .await
        .map_err(|e| e.to_string())?;
    set_status(app, st, "failed", "");
    Err(failed)
}

pub async fn run(app: AppHandle, st: Arc<AppState>) {
    let timing = if test_build() {
        match Timing::for_test() {
            Some(timing) => timing,
            None => return,
        }
    } else {
        Timing::normal()
    };
    st.updates.active.store(true, Ordering::SeqCst);
    let mut next_check = Instant::now() + timing.first;
    let mut hidden_since: Option<Instant> = None;
    loop {
        let mut manual = false;
        tokio::select! {
            _ = st.updates.check_now.notified() => manual = true,
            _ = st.auth_changed.notified() => {
                // Signing in again picks up a check that was waiting on the login.
                if st.updates.status.lock().unwrap().0 == "need_login" {
                    next_check = Instant::now();
                }
            }
            _ = tokio::time::sleep(timing.tick) => {}
        }
        if st.updates.installing.load(Ordering::SeqCst) {
            continue;
        }
        let now = Instant::now();
        let is_enabled = enabled(&st);
        // Turned off only stops installing by itself: it still checks, so Settings can offer the update.
        if !st.updates.has_staged() && (manual || now >= next_check) {
            next_check = Instant::now() + check(&app, &st, &timing).await;
        }
        let window_open = app.get_webview_window("login").is_some()
            || (app.get_webview_window("main").and_then(|w| w.is_visible().ok()).unwrap_or(true) && !st.attention.parked());
        hidden_since = if window_open { None } else { hidden_since.or(Some(now)) };
        if !st.updates.has_staged() {
            continue;
        }
        let hidden_for = hidden_since.map(|since| now.duration_since(since));
        if may_install_quietly(is_enabled, window_open, hidden_for, timing.idle, st.updates.busy()) && !claude_running(&st).await {
            let (handle, state) = (app.clone(), st.clone());
            // Only comes back if the install failed. The update is gone then, and the next check starts over.
            let _ = tauri::async_runtime::spawn_blocking(move || install(&handle, &state, false)).await;
            set_status(&app, &st, "failed", "");
            next_check = Instant::now() + timing.retry;
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    const NEED: Duration = Duration::from_secs(300);

    #[test]
    fn an_open_window_always_blocks_a_quiet_install() {
        let long = Some(Duration::from_secs(99_999));
        assert!(may_install_quietly(true, false, long, NEED, false));
        assert!(!may_install_quietly(true, true, long, NEED, false), "window open");
        assert!(!may_install_quietly(true, false, long, NEED, true), "something in progress");
        assert!(!may_install_quietly(false, false, long, NEED, false), "turned off");
        assert!(!may_install_quietly(true, false, Some(Duration::from_secs(10)), NEED, false), "only just hidden");
        assert!(!may_install_quietly(true, false, None, NEED, false), "never hidden");
    }

    #[test]
    fn the_marker_only_counts_right_after_an_install() {
        assert_eq!(parse_marker("hide 1000", 1100), Some(true));
        assert_eq!(parse_marker("show 1000", 1100), Some(false));
        assert_eq!(parse_marker("hide 1000", 1000 + MARKER_MAX_AGE_SECS + 1), None, "stale");
        assert_eq!(parse_marker("garbage", 1000), None);
        assert_eq!(parse_marker("other 1000", 1000), None);
    }

    #[test]
    fn taking_the_marker_removes_it() {
        let dir = tempfile::tempdir().unwrap();
        write_marker(dir.path(), false);
        assert_eq!(take_restart_marker(dir.path()), Some(true));
        assert_eq!(take_restart_marker(dir.path()), None);
    }

    #[test]
    fn the_download_address_always_stays_on_the_relay() {
        let latest = |file: &str| Latest { version: "1.2.3".into(), signature: "s".into(), file: file.into() };
        let url = |file: &str| manifest_for("https://relay.example.com", &latest(file)).unwrap()["url"].as_str().unwrap().to_string();
        assert_eq!(url("WA Relay_1.2.3_x64-setup.exe"), "https://relay.example.com/api/update/installer/WA%20Relay_1.2.3_x64-setup.exe");
        for hostile in ["https://evil.example/x.exe", "//evil.example/x.exe", "../../../evil", "x?y=1#z"] {
            let got: url::Url = url(hostile).parse().unwrap();
            assert_eq!(got.host_str(), Some("relay.example.com"), "{hostile}");
            assert!(got.path().starts_with("/api/update/installer/"), "{hostile}: {got}");
            assert_eq!(got.path_segments().unwrap().count(), 4, "{hostile}: {got}");
        }
    }
}
