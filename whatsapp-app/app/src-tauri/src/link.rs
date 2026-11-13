//! Whether what's on screen is current. This is the single answer the UI shows, so stale chats never
//! look current, and each kind of failure says what's actually wrong.
//!
//! A heartbeat arrives every 10 seconds. A stream that closes is only reported if it isn't back within
//! 8 seconds, so one dropped connection doesn't raise an alarm. A stream that goes silent is reported 25
//! seconds after the last thing heard from it. So the warning appears within about 8 to 25 seconds.

use crate::AppState;
use std::sync::atomic::Ordering;
use std::sync::Arc;
use std::time::{Duration, Instant};
use tauri::{AppHandle, Emitter};

pub const GRACE: Duration = Duration::from_secs(8);
pub const SILENCE: Duration = Duration::from_secs(25);
const SAVE_EVERY: Duration = Duration::from_secs(30);
const LAST_CURRENT: &str = "last_current_ms";

/// Why the stream isn't up, as best this PC can tell.
#[derive(Clone, Copy, Debug, PartialEq)]
pub enum Reach {
    /// No connection to Cloudflare at all, which points at this PC's internet.
    NoInternet,
    /// Cloudflare answers, but the tunnel doesn't, or the stream went silent.
    HomeDown,
    /// The tunnel is up, but nothing answers behind it.
    RelayDown,
}

pub struct Track {
    /// When the stream was lost. None while it's up.
    pub lost_at: Option<Instant>,
    pub last_heard: Instant,
    pub reach: Reach,
    /// When the home PC last started reporting a problem. None while it reports none.
    pub problem_since: Option<Instant>,
    pub shown: &'static str,
}

impl Track {
    pub fn new() -> Track {
        let now = Instant::now();
        Track { lost_at: Some(now), last_heard: now, reach: Reach::HomeDown, problem_since: None, shown: "connecting" }
    }
}

#[derive(Debug, Clone, Copy)]
pub struct Signals {
    pub need_login: bool,
    pub lost_for: Option<Duration>,
    pub reach: Reach,
    pub problem_for: Option<Duration>,
    pub bridge_down: bool,
    pub linked: bool,
    pub connected: bool,
    pub caught_up: bool,
}

/// Works out what to show. "ok" only ever means connected all the way to WhatsApp and caught up.
/// "connecting" is the short grace period, with no warning yet.
pub fn link_state(s: Signals) -> &'static str {
    if s.need_login {
        return "login";
    }
    if let Some(lost) = s.lost_for {
        if lost < GRACE {
            return "connecting";
        }
        return match s.reach {
            Reach::NoInternet => "no_internet",
            Reach::HomeDown => "home_down",
            Reach::RelayDown => "relay_down",
        };
    }
    if let Some(problem) = s.problem_for {
        if problem < GRACE {
            return "connecting";
        }
        if s.bridge_down {
            return "bridge_down";
        }
        if !s.linked {
            return "unlinked";
        }
        return "wa_down";
    }
    if !s.caught_up {
        return "catching_up";
    }
    "ok"
}

impl AppState {
    fn signals(&self, now: Instant) -> Signals {
        let track = self.link.lock().unwrap();
        Signals {
            need_login: self.need_login.load(Ordering::SeqCst),
            lost_for: track.lost_at.map(|at| now.duration_since(at)),
            reach: track.reach,
            problem_for: track.problem_since.map(|at| now.duration_since(at)),
            bridge_down: self.bridge_down.load(Ordering::SeqCst),
            linked: self.wa_linked.load(Ordering::SeqCst),
            connected: self.wa_connected.load(Ordering::SeqCst),
            caught_up: self.caught_up.load(Ordering::SeqCst),
        }
    }

    pub fn link_shown(&self) -> &'static str {
        self.link.lock().unwrap().shown
    }

    pub fn mark_connected(&self) {
        let now = Instant::now();
        let mut track = self.link.lock().unwrap();
        track.lost_at = None;
        track.last_heard = now;
        // Until the home PC reports its status and a sync finishes, nothing says this is current.
        track.problem_since = Some(now);
        self.caught_up.store(false, Ordering::SeqCst);
    }

    pub fn mark_heard(&self) {
        self.link.lock().unwrap().last_heard = Instant::now();
    }

    pub fn mark_lost(&self, reach: Reach, silent: bool) {
        let mut track = self.link.lock().unwrap();
        track.reach = reach;
        if track.lost_at.is_none() {
            // A silent stream has already been gone since it was last heard.
            track.lost_at = Some(if silent { track.last_heard } else { Instant::now() });
        }
        self.caught_up.store(false, Ordering::SeqCst);
    }

    /// The home PC's latest report.
    pub fn mark_status(&self, connected: bool, linked: bool, bridge_down: bool) {
        self.wa_connected.store(connected, Ordering::SeqCst);
        self.wa_linked.store(linked, Ordering::SeqCst);
        self.bridge_down.store(bridge_down, Ordering::SeqCst);
        let mut track = self.link.lock().unwrap();
        if connected && linked && !bridge_down {
            track.problem_since = None;
        } else {
            track.problem_since.get_or_insert_with(Instant::now);
            // WhatsApp may have queued messages for the bridge. Nothing here is known to be current.
            self.caught_up.store(false, Ordering::SeqCst);
        }
    }
}

fn now_ms() -> i64 {
    std::time::SystemTime::now().duration_since(std::time::UNIX_EPOCH).map(|d| d.as_millis() as i64).unwrap_or(0)
}

pub fn load_last_current(st: &AppState) {
    if let Some(saved) = st.cache.meta_get(LAST_CURRENT).ok().flatten().and_then(|v| v.parse().ok()) {
        st.last_current_ms.store(saved, Ordering::SeqCst);
    }
}

/// Re-works the state once a second, and tells the UI when it changes.
pub async fn watch(app: AppHandle, st: Arc<AppState>) {
    let mut last_saved = Instant::now() - SAVE_EVERY;
    loop {
        tokio::time::sleep(Duration::from_secs(1)).await;
        let now = Instant::now();
        let silent = {
            let track = st.link.lock().unwrap();
            track.lost_at.is_none() && now.duration_since(track.last_heard) >= SILENCE
        };
        if silent {
            // The stream's own timeout also ends it. This keeps the warning on time if that's delayed.
            st.mark_lost(Reach::HomeDown, true);
        }
        let state = link_state(st.signals(now));
        let previous = std::mem::replace(&mut st.link.lock().unwrap().shown, state);
        if state == "ok" {
            st.last_current_ms.store(now_ms(), Ordering::SeqCst);
        }
        let leaving_ok = previous == "ok" && state != "ok";
        if (state == "ok" && now.duration_since(last_saved) >= SAVE_EVERY) || leaving_ok {
            let _ = st.cache.meta_set(LAST_CURRENT, &st.last_current_ms.load(Ordering::SeqCst).to_string());
            last_saved = now;
        }
        if previous != state {
            let _ = app.emit("conn", st.conn_json());
            crate::update_tray(&app, &st);
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    const SHORT: Duration = Duration::from_secs(2);
    const LONG: Duration = Duration::from_secs(30);

    fn up() -> Signals {
        Signals {
            need_login: false,
            lost_for: None,
            reach: Reach::HomeDown,
            problem_for: None,
            bridge_down: false,
            linked: true,
            connected: true,
            caught_up: true,
        }
    }

    #[test]
    fn current_only_when_everything_is_up_and_caught_up() {
        assert_eq!(link_state(up()), "ok");
        assert_eq!(link_state(Signals { caught_up: false, ..up() }), "catching_up");
    }

    #[test]
    fn each_failure_is_told_apart() {
        let lost = |reach| Signals { lost_for: Some(LONG), reach, caught_up: false, ..up() };
        assert_eq!(link_state(lost(Reach::HomeDown)), "home_down");
        assert_eq!(link_state(lost(Reach::RelayDown)), "relay_down");
        assert_eq!(link_state(lost(Reach::NoInternet)), "no_internet");
        let problem = Signals { problem_for: Some(LONG), ..up() };
        assert_eq!(link_state(Signals { bridge_down: true, linked: false, connected: false, ..problem }), "bridge_down");
        assert_eq!(link_state(Signals { linked: false, connected: false, ..problem }), "unlinked");
        assert_eq!(link_state(Signals { connected: false, ..problem }), "wa_down");
        assert_eq!(link_state(Signals { need_login: true, lost_for: Some(LONG), ..up() }), "login");
    }

    #[test]
    fn one_dropped_connection_raises_no_alarm() {
        assert_eq!(link_state(Signals { lost_for: Some(SHORT), ..up() }), "connecting");
        assert_eq!(link_state(Signals { problem_for: Some(SHORT), connected: false, ..up() }), "connecting");
    }

    #[test]
    fn an_old_stale_status_cannot_look_current_while_the_stream_is_down() {
        // WhatsApp connected and caught up, as last reported, but the stream has been gone a while.
        assert_eq!(link_state(Signals { lost_for: Some(LONG), ..up() }), "home_down");
    }
}
