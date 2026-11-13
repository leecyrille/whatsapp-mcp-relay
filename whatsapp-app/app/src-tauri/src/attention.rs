//! Blinks the tray icon and the taskbar button while anything is unread, by one rule.
//!
//! Measured on Windows 11 build 26200: a timed flash keeps pulsing about every 1.65 s, while a counted flash ends stuck
//! lit. The flash is stopped here when the window comes to the front, rather than by `FLASHW_TIMERNOFG`, which ends at
//! once on a minimised window that's still the active one. A hidden window has no taskbar button, so when closed to the
//! tray the window comes back minimised, without taking focus, for as long as something is unread.

use crate::AppState;
use serde::Serialize;
use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};
use std::sync::{Arc, Mutex};
use std::time::Duration;
use tauri::image::Image;
use tauri::{AppHandle, Manager};

const MOTION_KEY: &str = "blink_motion";
/// Half of the 1.6 s beat the unread chat rows pulse on.
const TRAY_STEP_MS: u64 = 800;

#[derive(Clone, Copy, PartialEq, Eq, Debug, Serialize)]
#[serde(rename_all = "lowercase")]
pub enum Tray {
    Plain,
    Steady,
    Blink,
}

#[derive(Clone, Copy, PartialEq, Eq, Debug, Serialize)]
#[serde(rename_all = "lowercase")]
pub enum Taskbar {
    Off,
    Steady,
    Blink,
}

/// What's true right now.
#[derive(Clone, Copy, Debug)]
pub struct Seen {
    pub unread: bool,
    pub silenced: bool,
    /// The Settings switch is on, and Windows isn't set to show fewer animations.
    pub motion: bool,
    pub visible: bool,
    pub minimized: bool,
    pub foreground: bool,
    /// The window is only showing so that it has a taskbar button.
    pub parked: bool,
}

/// What to do about it.
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub struct Plan {
    pub tray: Tray,
    pub taskbar: Taskbar,
    pub show_minimized: bool,
    pub hide: bool,
    pub parked: bool,
    /// The user has opened a window that was parked.
    pub opened: bool,
}

/// Silence means quiet: nothing blinks and no button appears, and the tray still shows a steady unread icon.
pub fn plan(s: Seen) -> Plan {
    let wanted = s.unread && !s.silenced;
    let tray = match (s.unread, s.silenced || !s.motion) {
        (false, _) => Tray::Plain,
        (true, true) => Tray::Steady,
        (true, false) => Tray::Blink,
    };
    let opened = s.visible && s.parked && !s.minimized;
    let (show_minimized, hide, parked) = if !s.visible {
        (wanted, false, wanted)
    } else if opened {
        (false, false, false)
    } else if s.parked && !wanted {
        (false, true, false)
    } else {
        (false, false, s.parked)
    };
    let has_button = (s.visible && !hide) || show_minimized;
    let taskbar = if !wanted || !has_button || s.foreground {
        Taskbar::Off
    } else if s.motion {
        Taskbar::Blink
    } else {
        Taskbar::Steady
    };
    Plan { tray, taskbar, show_minimized, hide, parked, opened }
}

pub struct Attention {
    parked: AtomicBool,
    motion: AtomicBool,
    tray: Mutex<Tray>,
    taskbar: Mutex<Taskbar>,
    lit: AtomicBool,
    wake: tokio::sync::Notify,
    swaps: AtomicU64,
}

impl Attention {
    pub fn new(motion: bool) -> Self {
        Self {
            parked: AtomicBool::new(false),
            motion: AtomicBool::new(motion),
            tray: Mutex::new(Tray::Plain),
            taskbar: Mutex::new(Taskbar::Off),
            lit: AtomicBool::new(false),
            wake: tokio::sync::Notify::new(),
            swaps: AtomicU64::new(0),
        }
    }

    pub fn parked(&self) -> bool {
        self.parked.load(Ordering::SeqCst)
    }
}

pub fn saved_motion(cache: &crate::db::Cache) -> bool {
    cache.meta_get(MOTION_KEY).ok().flatten().as_deref() != Some("0")
}

pub fn set_motion(st: &AppState, on: bool) {
    let _ = st.cache.meta_set(MOTION_KEY, if on { "1" } else { "0" });
    st.attention.motion.store(on, Ordering::SeqCst);
    st.attention.wake.notify_one();
}

/// Asks for the rule to be worked out again. Safe from any thread, and bursts collapse into one.
pub fn refresh(st: &AppState) {
    st.attention.wake.notify_one();
}

/// A test window stays off-screen and leaves the real taskbar alone, unless a test is measuring that taskbar.
fn real_taskbar() -> bool {
    crate::test_override("WA_TEST_OFFSCREEN").is_none() || crate::test_override("WA_TEST_REAL_TASKBAR").is_some()
}

fn tray_icon(unread: bool) -> tauri::Result<Image<'static>> {
    if unread {
        Image::from_bytes(include_bytes!("../icons/tray-unread.png"))
    } else {
        Image::from_bytes(include_bytes!("../icons/tray.png"))
    }
}

fn set_tray_icon(st: &AppState, unread: bool) {
    if let (Some(tray), Ok(icon)) = (st.tray.lock().unwrap().as_ref(), tray_icon(unread)) {
        let _ = tray.set_icon(Some(icon));
    }
}

/// The window library skips a hide if it never knew about the window being shown, so Windows is told as well.
pub fn hide_window(window: &tauri::WebviewWindow) {
    let _ = window.hide();
    if real_taskbar() {
        native::hide(window);
    }
}

/// Runs on the main thread, so window changes and tray changes happen in order.
fn apply(app: &AppHandle, st: &AppState, unread: bool, silenced: bool) {
    let Some(window) = app.get_webview_window("main") else { return };
    let real = real_taskbar();
    let minimized = window.is_minimized().unwrap_or(false);
    let focused = window.is_focused().unwrap_or(false);
    let seen = Seen {
        unread,
        silenced,
        motion: st.attention.motion.load(Ordering::SeqCst),
        visible: !real || window.is_visible().unwrap_or(true),
        minimized,
        foreground: focused && !minimized,
        parked: st.attention.parked(),
    };
    let plan = plan(seen);
    st.attention.parked.store(plan.parked, Ordering::SeqCst);

    if real {
        if plan.show_minimized {
            native::show_minimized_without_focus(&window);
        }
        if plan.hide {
            hide_window(&window);
        }
        // A window Windows opened from its button is news to the window library, which still thinks it's hidden.
        if plan.opened && focused {
            let _ = window.show();
        }
    }
    {
        let mut taskbar = st.attention.taskbar.lock().unwrap();
        // Hiding a window can end its flash, so one that's just come back always starts again.
        if *taskbar != plan.taskbar || plan.show_minimized {
            if real {
                native::flash(&window, plan.taskbar);
            }
            *taskbar = plan.taskbar;
        }
    }
    let mut tray = st.attention.tray.lock().unwrap();
    if *tray != plan.tray {
        *tray = plan.tray;
        drop(tray);
        st.attention.lit.store(plan.tray != Tray::Plain, Ordering::SeqCst);
        set_tray_icon(st, plan.tray != Tray::Plain);
        st.attention.wake.notify_one(); // So the loop below starts or stops its timer.
    }
}

/// Reads off the main thread, since a long search can hold the cache for a moment, then acts on it.
pub async fn run(app: AppHandle, st: Arc<AppState>) {
    let step = crate::test_override("WA_TEST_BLINK_MS").and_then(|v| v.parse().ok()).unwrap_or(TRAY_STEP_MS);
    loop {
        let blinking = *st.attention.tray.lock().unwrap() == Tray::Blink;
        tokio::select! {
            _ = st.attention.wake.notified() => {
                let unread = st.cache.total_unread() > 0;
                let silenced = crate::toast::silenced_until(&st).is_some();
                let (handle, state) = (app.clone(), st.clone());
                let _ = app.run_on_main_thread(move || apply(&handle, &state, unread, silenced));
            }
            _ = tokio::time::sleep(Duration::from_millis(step)), if blinking => {
                let state = st.clone();
                let _ = app.run_on_main_thread(move || {
                    // Checked here, or a late step could bring the unread icon back after everything's read.
                    if *state.attention.tray.lock().unwrap() == Tray::Blink {
                        let lit = !state.attention.lit.fetch_xor(true, Ordering::SeqCst);
                        set_tray_icon(&state, lit);
                        state.attention.swaps.fetch_add(1, Ordering::SeqCst);
                    }
                });
            }
        }
    }
}

/// What opening the window does, for a test on a PC someone's using: it must not take their keyboard.
pub fn test_open(window: &tauri::WebviewWindow) {
    native::restore_without_focus(window);
}

/// What the user's own minimise button does, which goes through Windows and not the window library.
pub fn test_minimize(window: &tauri::WebviewWindow) {
    native::minimize(window);
}

pub fn test_state(st: &AppState) -> serde_json::Value {
    serde_json::json!({
        "tray": *st.attention.tray.lock().unwrap(),
        "taskbar": *st.attention.taskbar.lock().unwrap(),
        "parked": st.attention.parked(),
        "motion": st.attention.motion.load(Ordering::SeqCst),
        "swaps": st.attention.swaps.load(Ordering::SeqCst),
    })
}

#[cfg(windows)]
mod native {
    use super::Taskbar;
    use tauri::WebviewWindow;
    use windows::Win32::Foundation::HWND;
    use windows::Win32::UI::WindowsAndMessaging::{
        FlashWindowEx, ShowWindow, FLASHWINFO, FLASHW_STOP, FLASHW_TIMER, FLASHW_TRAY, SW_HIDE, SW_SHOWMINNOACTIVE, SW_SHOWNOACTIVATE,
    };

    fn hwnd(window: &WebviewWindow) -> Option<HWND> {
        window.hwnd().ok().map(|h| HWND(h.0))
    }

    pub fn show_minimized_without_focus(window: &WebviewWindow) {
        if let Some(hwnd) = hwnd(window) {
            // Its result says whether the window was visible before, not whether this worked.
            let _ = unsafe { ShowWindow(hwnd, SW_SHOWMINNOACTIVE) };
        }
    }

    pub fn hide(window: &WebviewWindow) {
        if let Some(hwnd) = hwnd(window) {
            let _ = unsafe { ShowWindow(hwnd, SW_HIDE) };
        }
    }

    pub fn restore_without_focus(window: &WebviewWindow) {
        if let Some(hwnd) = hwnd(window) {
            let _ = unsafe { ShowWindow(hwnd, SW_SHOWNOACTIVATE) };
        }
    }

    pub fn minimize(window: &WebviewWindow) {
        if let Some(hwnd) = hwnd(window) {
            let _ = unsafe { ShowWindow(hwnd, SW_SHOWMINNOACTIVE) };
        }
    }

    pub fn flash(window: &WebviewWindow, how: Taskbar) {
        let Some(hwnd) = hwnd(window) else { return };
        let call = |flags, count| {
            let info = FLASHWINFO { cbSize: size_of::<FLASHWINFO>() as u32, hwnd, dwFlags: flags, uCount: count, dwTimeout: 0 };
            let _ = unsafe { FlashWindowEx(&info) };
        };
        call(FLASHW_STOP, 0);
        match how {
            Taskbar::Off => {}
            // A single counted flash ends lit, and stays lit until stopped.
            Taskbar::Steady => call(FLASHW_TRAY, 1),
            Taskbar::Blink => call(FLASHW_TRAY | FLASHW_TIMER, 0),
        }
    }
}

#[cfg(not(windows))]
mod native {
    use super::Taskbar;
    use tauri::WebviewWindow;
    pub fn show_minimized_without_focus(_: &WebviewWindow) {}
    pub fn hide(_: &WebviewWindow) {}
    pub fn restore_without_focus(_: &WebviewWindow) {}
    pub fn minimize(_: &WebviewWindow) {}
    pub fn flash(_: &WebviewWindow, _: Taskbar) {}
}

#[cfg(test)]
mod tests {
    use super::*;

    fn seen() -> Seen {
        Seen { unread: true, silenced: false, motion: true, visible: true, minimized: false, foreground: false, parked: false }
    }

    #[test]
    fn both_blink_while_unread_and_stop_once_read() {
        let on = plan(seen());
        assert_eq!((on.tray, on.taskbar), (Tray::Blink, Taskbar::Blink));
        let off = plan(Seen { unread: false, ..seen() });
        assert_eq!((off.tray, off.taskbar), (Tray::Plain, Taskbar::Off));
    }

    #[test]
    fn silence_stops_both_blinking_and_the_tray_still_shows_unread() {
        let p = plan(Seen { silenced: true, ..seen() });
        assert_eq!((p.tray, p.taskbar), (Tray::Steady, Taskbar::Off));
    }

    #[test]
    fn the_blink_switch_and_fewer_animations_leave_both_steady() {
        let p = plan(Seen { motion: false, ..seen() });
        assert_eq!((p.tray, p.taskbar), (Tray::Steady, Taskbar::Steady));
    }

    #[test]
    fn nothing_flashes_on_the_window_in_front_but_the_tray_carries_on() {
        let p = plan(Seen { foreground: true, ..seen() });
        assert_eq!((p.tray, p.taskbar), (Tray::Blink, Taskbar::Off));
    }

    #[test]
    fn closed_to_the_tray_the_button_comes_back_while_unread() {
        let hidden = Seen { visible: false, ..seen() };
        let p = plan(hidden);
        assert!(p.show_minimized && p.parked && !p.hide);
        assert_eq!(p.taskbar, Taskbar::Blink);
        let read = plan(Seen { unread: false, ..hidden });
        assert!(!read.show_minimized && !read.parked);
        assert_eq!(read.taskbar, Taskbar::Off);
    }

    #[test]
    fn a_parked_window_goes_away_once_read_or_silenced() {
        let parked = Seen { minimized: true, parked: true, ..seen() };
        assert_eq!(plan(parked).taskbar, Taskbar::Blink);
        assert!(!plan(parked).hide);
        for s in [Seen { unread: false, ..parked }, Seen { silenced: true, ..parked }] {
            let p = plan(s);
            assert!(p.hide && !p.parked, "{s:?}");
            assert_eq!(p.taskbar, Taskbar::Off);
        }
    }

    #[test]
    fn silenced_and_hidden_no_button_appears() {
        let p = plan(Seen { visible: false, silenced: true, ..seen() });
        assert!(!p.show_minimized && !p.parked);
        assert_eq!((p.tray, p.taskbar), (Tray::Steady, Taskbar::Off));
    }

    #[test]
    fn a_window_lee_minimised_is_never_hidden_or_taken_over() {
        let p = plan(Seen { minimized: true, ..seen() });
        assert!(!p.hide && !p.parked && !p.show_minimized);
        assert_eq!(p.taskbar, Taskbar::Blink);
        let read = plan(Seen { minimized: true, unread: false, ..seen() });
        assert!(!read.hide);
        assert_eq!(read.taskbar, Taskbar::Off);
    }

    #[test]
    fn opening_a_parked_window_makes_it_his_again() {
        let p = plan(Seen { parked: true, foreground: true, ..seen() });
        assert!(p.opened && !p.parked && !p.hide);
        assert_eq!(p.taskbar, Taskbar::Off);
        // Still behind something, it keeps asking, and reading then doesn't hide it.
        let behind = plan(Seen { parked: true, ..seen() });
        assert!(behind.opened && !behind.parked);
        assert_eq!(behind.taskbar, Taskbar::Blink);
        assert!(!plan(Seen { unread: false, parked: true, ..seen() }).hide);
    }
}
