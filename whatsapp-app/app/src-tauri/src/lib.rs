//! WA Relay: a desktop client for the one WhatsApp link on the home PC.

pub mod attention;
mod commands;
pub mod db;
pub mod link;
pub mod media;
pub mod net;
pub mod setup;
pub mod toast;
pub mod update;

use db::Cache;
use net::Relay;
use serde_json::json;
use std::collections::HashMap;
use std::path::PathBuf;
use std::sync::atomic::{AtomicBool, AtomicI64, Ordering};
use std::sync::{Arc, Mutex};
use tauri::image::Image;
use tauri::tray::{MouseButton, MouseButtonState, TrayIcon, TrayIconBuilder, TrayIconEvent};
use tauri::webview::PageLoadEvent;
use tauri::{AppHandle, Emitter, Manager, WebviewUrl, WebviewWindow, WebviewWindowBuilder, WindowEvent};
use tauri_plugin_autostart::{MacosLauncher, ManagerExt};

pub const ACCESS_HOST_SUFFIX: &str = ".cloudflareaccess.com";

pub struct AppState {
    pub cache: Arc<Cache>,
    pub relay: Relay,
    pub media_dir: PathBuf,
    pub need_login: AtomicBool,
    pub online: AtomicBool,
    pub wa_connected: AtomicBool,
    pub wa_linked: AtomicBool,
    pub bridge_down: AtomicBool,
    pub caught_up: AtomicBool,
    pub last_current_ms: AtomicI64,
    pub link: Mutex<link::Track>,
    pub claude_up: AtomicBool,
    pub pulling: AtomicBool,
    pub pull_again: AtomicBool,
    pub auth_changed: tokio::sync::Notify,
    pub active_chat: Mutex<Option<String>>,
    /// What a clicked notification asked to open, until the page takes it.
    pub open_target: Mutex<Option<toast::Target>>,
    /// How many notifications a test build would have shown.
    pub test_toasts: std::sync::atomic::AtomicU64,
    pub picked: Mutex<HashMap<String, PathBuf>>,
    pub download_gates: Mutex<HashMap<i64, Arc<tokio::sync::Mutex<()>>>>,
    pub tray: Mutex<Option<TrayIcon>>,
    pub attention: attention::Attention,
    pub updates: Arc<update::Updates>,
    /// Chats where the home PC had no picture to give, so scrolling doesn't keep asking.
    pub avatar_misses: Mutex<HashMap<String, (i64, std::time::Instant)>>,
}

impl AppState {
    pub fn conn_json(&self) -> serde_json::Value {
        json!({
            "need_login": self.need_login.load(Ordering::SeqCst),
            "online": self.online.load(Ordering::SeqCst),
            "wa_connected": self.wa_connected.load(Ordering::SeqCst),
            "wa_linked": self.wa_linked.load(Ordering::SeqCst),
            "claude_up": self.claude_up.load(Ordering::SeqCst),
            "link": self.link_shown(),
            "last_updated_ms": self.last_current_ms.load(Ordering::SeqCst),
        })
    }
}

pub fn random_id() -> String {
    let mut buf = [0u8; 16];
    getrandom::getrandom(&mut buf).expect("system random");
    buf.iter().map(|b| format!("{b:02x}")).collect()
}

/// Debug builds, and release builds made with the `test-overrides` feature to test real updates.
/// The installer shipped to PCs is neither.
pub fn test_build() -> bool {
    cfg!(any(debug_assertions, feature = "test-overrides"))
}

/// Test builds can point at a fake relay and an isolated folder, so no test touches real data.
/// Release builds ignore these, and always talk to the real hostname.
pub fn test_override(name: &str) -> Option<String> {
    if test_build() {
        std::env::var(name).ok().filter(|v| !v.is_empty())
    } else {
        None
    }
}

/// An installer restarts the app without its environment, so a test build installed for a real update
/// test reads the same settings from a file beside itself.
fn load_test_overrides() {
    if !test_build() {
        return;
    }
    let Some(file) = std::env::current_exe().ok().and_then(|exe| exe.parent().map(|d| d.join("wa-test-overrides.txt"))) else {
        return;
    };
    let Ok(text) = std::fs::read_to_string(file) else { return };
    for (name, value) in text.lines().filter_map(|line| line.split_once('=')) {
        std::env::set_var(name.trim(), value.trim());
    }
}

pub fn update_tray(app: &AppHandle, st: &AppState) {
    let unread = st.cache.total_unread();
    let tooltip = match (st.link_shown(), unread) {
        ("ok" | "connecting" | "catching_up", 0) => "WA Relay".to_string(),
        ("ok" | "connecting" | "catching_up", _) => format!("WA Relay: {unread} unread"),
        _ => "WA Relay: not connected, may be out of date".to_string(),
    };
    let tooltip = match toast::silenced_until(st) {
        Some(until) => format!("{tooltip}\nNotifications silenced until {}", toast::clock(until)),
        None => tooltip,
    };
    if let Some(tray) = st.tray.lock().unwrap().as_ref() {
        let _ = tray.set_tooltip(Some(&tooltip));
    }
    attention::refresh(st);
    let _ = app.emit("unread", unread);
}

pub fn show_main(app: &AppHandle) {
    if let Some(window) = app.get_webview_window("main") {
        let _ = window.show();
        let _ = window.unminimize();
        // A test window sits off-screen, and must not take the keyboard from whoever's using this PC.
        if test_override("WA_TEST_OFFSCREEN").is_none() {
            let _ = window.set_focus();
        }
    }
}

fn main_window(app: &AppHandle, start_hidden: bool, data_dir: Option<PathBuf>) -> tauri::Result<WebviewWindow> {
    let mut builder = WebviewWindowBuilder::new(app, "main", WebviewUrl::App("index.html".into()))
        .title(test_override("WA_TEST_TITLE").unwrap_or_else(|| "WA Relay".to_string()))
        .inner_size(1180.0, 780.0)
        .min_inner_size(420.0, 480.0)
        .visible(false)
        // The app's own page is the only thing this window ever shows. Links go to the browser.
        .on_navigation(|url| matches!(url.host_str(), Some("tauri.localhost") | Some("localhost")));
    if let Some(dir) = data_dir {
        builder = builder.data_directory(dir);
    }
    if test_override("WA_TEST_OFFSCREEN").is_some() {
        builder = builder.position(-3000.0, -3000.0);
    }
    let window = builder.build()?;
    if !start_hidden {
        let _ = window.show();
    }
    Ok(window)
}

/// The sign-in window. It only navigates between the relay's own host and Cloudflare Access, and has
/// no capability, so nothing it loads can call the app's commands.
pub fn open_login_window(app: &AppHandle) -> tauri::Result<()> {
    if let Some(existing) = app.get_webview_window("login") {
        let _ = existing.set_focus();
        return Ok(());
    }
    let st = app.state::<Arc<AppState>>().inner().clone();
    let base: url::Url = st.relay.base.parse().expect("relay url");
    let relay_host = base.host_str().unwrap_or("").to_string();
    let login_url: url::Url = format!("{}/login-ok", st.relay.base).parse().expect("login url");
    let allowed_host = relay_host.clone();
    let handle = app.clone();
    let mut builder = WebviewWindowBuilder::new(app, "login", WebviewUrl::External(login_url))
        .title("Sign in to WA Relay")
        .inner_size(460.0, 640.0)
        .center()
        .on_navigation(move |url| {
            let host = url.host_str().unwrap_or("");
            host == allowed_host || host.ends_with(ACCESS_HOST_SUFFIX)
        })
        .on_page_load(move |window, payload| {
            if payload.event() != PageLoadEvent::Finished {
                return;
            }
            let url = payload.url();
            if url.host_str() != Some(relay_host.as_str()) || url.path() != "/login-ok" {
                return;
            }
            let app = handle.clone();
            let window = window.clone();
            // Reading cookies must not happen on the main thread.
            tauri::async_runtime::spawn(async move {
                let st = app.state::<Arc<AppState>>().inner().clone();
                let base = st.relay.base.clone();
                let reader = app.clone();
                let cookie = tauri::async_runtime::spawn_blocking(move || net::read_cookie(&reader, &base))
                    .await
                    .ok()
                    .flatten();
                if let Some(cookie) = cookie {
                    st.relay.set_cookie(Some(cookie));
                    st.need_login.store(false, Ordering::SeqCst);
                    st.auth_changed.notify_waiters();
                    let _ = app.emit("conn", st.conn_json());
                    let _ = window.close();
                    show_main(&app);
                }
            });
        });
    if let Some(dir) = test_override("WA_DATA_DIR") {
        builder = builder.data_directory(PathBuf::from(dir).join("webview"));
    }
    if test_override("WA_TEST_OFFSCREEN").is_some() {
        builder = builder.position(-3000.0, -3000.0);
    }
    builder.build()?;
    Ok(())
}

pub fn run() {
    load_test_overrides();
    toast::allow_handover(&std::env::args().collect::<Vec<_>>());
    let base_builder = tauri::Builder::default();
    // A test of two running clients needs two copies, each with its own data folder.
    let base_builder = if test_override("WA_SECOND_INSTANCE").is_some() {
        base_builder
    } else {
        base_builder.plugin(tauri_plugin_single_instance::init(|app, args, _cwd| toast::activate(app, &args)))
    };
    let app_builder = base_builder
        .plugin(tauri_plugin_autostart::init(MacosLauncher::LaunchAgent, Some(vec!["--minimized"])))
        .plugin(tauri_plugin_notification::init())
        .plugin(tauri_plugin_dialog::init())
        .plugin(tauri_plugin_opener::init())
        .plugin(tauri_plugin_updater::Builder::new().build())
        .register_asynchronous_uri_scheme_protocol("wamedia", |ctx, request, responder| {
            let app = ctx.app_handle().clone();
            tauri::async_runtime::spawn(async move {
                responder.respond(media::handle(app, request).await);
            });
        })
        .invoke_handler(commands::handlers())
        .on_window_event(|window, event| match event {
            // Closing the main window hides it. The app keeps syncing in the tray until Quit.
            WindowEvent::CloseRequested { api, .. } if window.label() == "main" => {
                api.prevent_close();
                if let Some(main) = window.app_handle().get_webview_window("main") {
                    attention::hide_window(&main);
                }
                attention::refresh(window.app_handle().state::<Arc<AppState>>().inner());
            }
            // Coming to the front stops the taskbar's flash, and leaving it starts again if anything's still unread.
            WindowEvent::Focused(_) if window.label() == "main" => {
                attention::refresh(window.app_handle().state::<Arc<AppState>>().inner());
            }
            // Opening a parked window from its taskbar button.
            WindowEvent::Resized(_) if window.label() == "main" => {
                let st = window.app_handle().state::<Arc<AppState>>().inner().clone();
                if st.attention.parked() {
                    attention::refresh(&st);
                }
            }
            // Dropped files are registered here, so the UI gets tokens and never handles paths.
            WindowEvent::DragDrop(tauri::DragDropEvent::Drop { paths, .. }) if window.label() == "main" => {
                let st = window.app_handle().state::<Arc<AppState>>().inner().clone();
                let files: Vec<_> = paths.iter().filter_map(|p| media::register_picked(&st, p.clone())).collect();
                let _ = window.emit("files-dropped", files);
            }
            _ => {}
        });
    app_builder
        .setup(setup)
        .build(tauri::generate_context!())
        .expect("failed to start")
        .run(|_app, _event| {});
}

fn setup(app: &mut tauri::App) -> Result<(), Box<dyn std::error::Error>> {
    let handle = app.handle().clone();
    let (data_root, adopted) = match test_override("WA_DATA_DIR") {
        Some(dir) => (PathBuf::from(dir), None),
        None => {
            let dir = app.path().app_local_data_dir()?;
            let adopted = setup::adopt_earlier_data(&dir, &app.config().identifier);
            (dir, adopted)
        }
    };
    std::fs::create_dir_all(&data_root)?;
    // A clicked notification always wants the window, even if something else says to start hidden.
    let opened_with = toast::from_args(&std::env::args().collect::<Vec<_>>());
    let start_hidden = update::take_restart_marker(&data_root)
        .unwrap_or_else(|| std::env::args().any(|a| a == "--minimized") || test_override("WA_START_HIDDEN").is_some())
        || matches!(opened_with, Some(toast::Target::Silence { .. }));
    let start_hidden = start_hidden && !matches!(opened_with, Some(toast::Target::App | toast::Target::Chat { .. } | toast::Target::Claude));
    let version = app.package_info().version.to_string();
    let updates = Arc::new(update::Updates::new(data_root.clone()));
    updates.log(&format!("started {version}, hidden={start_hidden}"));
    if adopted.is_some() {
        updates.log("moved this app's data over from its earlier identifier");
    }
    let media_dir = data_root.join("media");
    std::fs::create_dir_all(&media_dir)?;
    let cache = Arc::new(Cache::open(&data_root.join("cache.db")).map_err(|e| format!("cache: {e}"))?);
    // No relay yet means the first run: the window asks for one, and nothing talks to a relay until then.
    let base = setup::relay_address(&cache);

    // Boot with Windows unless the user has turned it off. Decided once, on the first run.
    if cache.meta_get("first_run_done").ok().flatten().is_none() {
        if !test_build() {
            let _ = handle.autolaunch().enable();
        }
        let _ = cache.meta_set("first_run_done", "1");
    }

    let attention = attention::Attention::new(attention::saved_motion(&cache));
    let state = Arc::new(AppState {
        cache,
        relay: Relay::new(base.clone().unwrap_or_default(), &version),
        media_dir: media_dir.clone(),
        need_login: AtomicBool::new(false),
        online: AtomicBool::new(false),
        wa_connected: AtomicBool::new(false),
        wa_linked: AtomicBool::new(false),
        bridge_down: AtomicBool::new(false),
        caught_up: AtomicBool::new(false),
        last_current_ms: AtomicI64::new(0),
        link: Mutex::new(link::Track::new()),
        claude_up: AtomicBool::new(false),
        pulling: AtomicBool::new(false),
        pull_again: AtomicBool::new(false),
        auth_changed: tokio::sync::Notify::new(),
        active_chat: Mutex::new(None),
        open_target: Mutex::new(None),
        test_toasts: std::sync::atomic::AtomicU64::new(0),
        picked: Mutex::new(HashMap::new()),
        download_gates: Mutex::new(HashMap::new()),
        tray: Mutex::new(None),
        attention,
        updates,
        avatar_misses: Mutex::new(HashMap::new()),
    });
    link::load_last_current(&state);
    if let Some(target) = opened_with {
        toast::receive(&handle, &state, target);
    }
    app.manage(state.clone());

    let webview_dir = test_override("WA_DATA_DIR").map(|d| PathBuf::from(d).join("webview"));
    main_window(&handle, start_hidden, webview_dir)?;

    let menu = toast::tray_menu(&handle, toast::silenced_until(&state))?;
    let tray = TrayIconBuilder::with_id("main")
        .icon(Image::from_bytes(include_bytes!("../icons/tray.png"))?)
        .tooltip("WA Relay")
        .menu(&menu)
        .show_menu_on_left_click(false)
        .on_menu_event(|app, event| match event.id.as_ref() {
            "open" => show_main(app),
            "quit" => app.exit(0),
            id => toast::on_tray_menu(app, app.state::<Arc<AppState>>().inner(), id),
        })
        .on_tray_icon_event(|tray, event| {
            if let TrayIconEvent::Click { button: MouseButton::Left, button_state: MouseButtonState::Up, .. } = event {
                show_main(tray.app_handle());
            }
        })
        .build(app)?;
    *state.tray.lock().unwrap() = Some(tray);
    update_tray(&handle, &state);

    let (dir, limit) = (media_dir, 5u64 << 30);
    tauri::async_runtime::spawn_blocking(move || media::prune(&dir, limit));
    if base.is_some() {
        tauri::async_runtime::spawn(link::watch(handle.clone(), state.clone()));
    }
    tauri::async_runtime::spawn(toast::watch_silence(handle.clone(), state.clone()));
    tauri::async_runtime::spawn(attention::run(handle.clone(), state.clone()));
    tauri::async_runtime::spawn(update::run(handle.clone(), state.clone()));
    if base.is_some() {
        tauri::async_runtime::spawn(net::run(handle, state));
    }
    Ok(())
}
