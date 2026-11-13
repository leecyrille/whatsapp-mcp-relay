//! Notifications you can click. Each one carries a link to this app, which the installer registers with
//! Windows. Windows opens that link itself, so a click works with the window hidden, with the app not
//! running, and after the notification has moved to Action Center, and needs no background server.
//!
//! The link holds only a chat's address, never message text. Any program or web page can open a link like
//! this, so it's treated as untrusted: it can only bring the app forward and open a chat that's already in
//! the list. Ignore is handled by Windows, and only closes the notification. Silencing changes something,
//! so its link also carries a key only this app's own notifications know.

use crate::{media, AppState};
use serde::Serialize;
use std::sync::Arc;
use std::time::Duration;
use tauri::menu::{Menu, MenuItem, PredefinedMenuItem, Submenu};
use tauri::{AppHandle, Emitter, Manager};
use tauri_plugin_notification::NotificationExt;

/// A build for testing real updates has its own link type, so it can't take over a real install's.
pub const SCHEME: &str = if cfg!(feature = "test-overrides") { "warelaytest" } else { "warelay" };

#[derive(Debug, Clone, PartialEq, Eq, Serialize)]
#[serde(tag = "kind", rename_all = "lowercase")]
pub enum Target {
    App,
    Chat { jid: String },
    Claude,
    Silence { key: String },
}

pub fn link(target: &Target) -> String {
    match target {
        Target::App => format!("{SCHEME}://open"),
        Target::Chat { jid } => format!("{SCHEME}://open?chat={}", jid.replace('@', "%40")),
        Target::Claude => format!("{SCHEME}://claude"),
        Target::Silence { key } => format!("{SCHEME}://silence?key={key}"),
    }
}

/// Reads one of this app's links. Anything else isn't one, and a bad address opens no chat.
pub fn parse(arg: &str) -> Option<Target> {
    let url = url::Url::parse(arg).ok()?;
    if url.scheme() != SCHEME {
        return None;
    }
    match url.host_str()? {
        "claude" => Some(Target::Claude),
        "silence" => Some(Target::Silence {
            key: url.query_pairs().find(|(key, _)| key == "key").map(|(_, value)| value.into_owned()).unwrap_or_default(),
        }),
        "open" => {
            let chat = url.query_pairs().find(|(key, _)| key == "chat").map(|(_, value)| value.into_owned());
            Some(match chat {
                Some(jid) if media::valid_jid(&jid) => Target::Chat { jid },
                _ => Target::App,
            })
        }
        _ => None,
    }
}

pub fn from_args(args: &[String]) -> Option<Target> {
    args.iter().find_map(|arg| parse(arg))
}

/// A chat that isn't in the list just brings the app forward.
fn known(st: &AppState, target: Target) -> Target {
    match target {
        Target::Chat { jid } => {
            let jid = st.cache.canon(&jid);
            if st.cache.has_chat(&jid) {
                Target::Chat { jid }
            } else {
                st.updates.log("opened from a link, with a chat not in the list");
                Target::App
            }
        }
        other => other,
    }
}

/// Keeps what to open until the page asks. A page that's still loading asks when it's ready. Silencing
/// opens nothing.
pub fn receive(app: &AppHandle, st: &AppState, target: Target) {
    if let Target::Silence { key } = &target {
        if key.len() >= 16 && *key == silence_key(st) {
            let _ = silence(app, st, SILENCE_BUTTON_MINUTES);
        } else {
            st.updates.log("a link to silence notifications had the wrong key");
        }
        return;
    }
    let target = known(st, target);
    if !matches!(target, Target::App) {
        st.updates.log("opened from a link");
    }
    *st.open_target.lock().unwrap() = Some(target);
}

/// A second copy of the app was started with a link, or with none, and handed it over and left.
pub fn activate(app: &AppHandle, args: &[String]) {
    let target = from_args(args);
    if !matches!(target, Some(Target::Silence { .. })) {
        crate::show_main(app);
    }
    let (Some(target), Some(st)) = (target, app.try_state::<Arc<AppState>>()) else { return };
    receive(app, &st, target);
    let _ = app.emit("open-target", ());
}

/// Only the app that was just clicked may take the foreground, and that's the short-lived second copy. This
/// lets it pass that on to the copy that's running, or Windows may only flash the taskbar.
pub fn allow_handover(args: &[String]) {
    #[cfg(windows)]
    if from_args(args).is_some() {
        use windows::Win32::UI::WindowsAndMessaging::{AllowSetForegroundWindow, ASFW_ANY};
        unsafe {
            let _ = AllowSetForegroundWindow(ASFW_ANY);
        }
    }
    #[cfg(not(windows))]
    let _ = args;
}

/// Shows a notification. If Windows won't take this kind, one with no click is better than none.
pub fn show(app: &AppHandle, st: &AppState, title: &str, body: &str, target: &Target) {
    // Held back, not saved up: nothing here is shown later. Messages and unread counts carry on as usual.
    if silenced_until(st).is_some() {
        return;
    }
    // A test run must not put made-up messages on the screen of whoever's using this PC.
    if crate::test_override("WA_TEST_OFFSCREEN").is_some() {
        st.test_toasts.fetch_add(1, std::sync::atomic::Ordering::SeqCst);
        return;
    }
    #[cfg(windows)]
    {
        let xml = toast_xml(title, body, &link(target), &link(&Target::Silence { key: silence_key(st) }));
        match send(&app.config().identifier, &xml) {
            Ok(()) => return,
            Err(e) => st.updates.log(&format!("notification not accepted: {:#x}", e.code().0)),
        }
    }
    #[cfg(not(windows))]
    let _ = (st, target);
    let _ = app.notification().builder().title(title).body(body).show();
}

/// Text from messages goes through here, so it can't change the notification's layout or its link.
fn xml_escape(text: &str) -> String {
    let mut out = String::with_capacity(text.len());
    for c in text.chars() {
        match c {
            '&' => out.push_str("&amp;"),
            '<' => out.push_str("&lt;"),
            '>' => out.push_str("&gt;"),
            '"' => out.push_str("&quot;"),
            '\'' => out.push_str("&apos;"),
            // Characters XML can't hold at all, which would otherwise lose the whole notification.
            '\t' | '\n' | '\r' | '\u{20}'..='\u{D7FF}' | '\u{E000}'..='\u{FFFD}' | '\u{10000}'..='\u{10FFFF}' => out.push(c),
            _ => {}
        }
    }
    out
}

pub fn toast_xml(title: &str, body: &str, link: &str, silence: &str) -> String {
    let (title, body, link, silence) = (xml_escape(title), xml_escape(body), xml_escape(link), xml_escape(silence));
    format!(
        "<toast activationType=\"protocol\" launch=\"{link}\"><visual><binding template=\"ToastGeneric\">\
         <text>{title}</text><text>{body}</text></binding></visual><actions>\
         <action content=\"Open\" activationType=\"protocol\" arguments=\"{link}\"/>\
         <action content=\"Silence 1 hour\" activationType=\"protocol\" arguments=\"{silence}\"/>\
         <action content=\"Ignore\" activationType=\"system\" arguments=\"dismiss\"/>\
         </actions></toast>"
    )
}

#[cfg(windows)]
fn send(app_id: &str, xml: &str) -> windows::core::Result<()> {
    use windows::core::HSTRING;
    use windows::Data::Xml::Dom::XmlDocument;
    use windows::UI::Notifications::{ToastNotification, ToastNotificationManager};
    let doc = XmlDocument::new()?;
    doc.LoadXml(&HSTRING::from(xml))?;
    let toast = ToastNotification::CreateToastNotification(&doc)?;
    ToastNotificationManager::CreateToastNotifierWithId(&HSTRING::from(app_id))?.Show(&toast)
}

pub const SILENCE_MINUTES: [u32; 6] = [15, 30, 60, 120, 180, 240];
const SILENCE_BUTTON_MINUTES: u32 = 60;
const SILENCE_UNTIL: &str = "silence_until_ms";
const SILENCE_KEY: &str = "silence_key";
const MS_PER_MINUTE: i64 = 60_000;

fn now_ms() -> i64 {
    std::time::SystemTime::now().duration_since(std::time::UNIX_EPOCH).map(|d| d.as_millis() as i64).unwrap_or(0)
}

/// A stored end time, so it survives a restart and ends by itself. One further away than the longest choice
/// means the clock moved, and that counts as ended, so it can't stay on for good.
fn still_silenced(until: i64, now: i64, longest: i64) -> bool {
    now < until && until - now <= longest
}

fn longest_ms() -> i64 {
    let test = crate::test_override("WA_TEST_SILENCE_MS").and_then(|v| v.parse::<i64>().ok()).unwrap_or(0);
    test.max(*SILENCE_MINUTES.iter().max().unwrap() as i64 * MS_PER_MINUTE)
}

pub fn silenced_until(st: &AppState) -> Option<i64> {
    let until = st.cache.meta_get(SILENCE_UNTIL).ok().flatten()?.parse::<i64>().ok()?;
    still_silenced(until, now_ms(), longest_ms()).then_some(until)
}

fn silence_key(st: &AppState) -> String {
    if let Some(key) = st.cache.meta_get(SILENCE_KEY).ok().flatten() {
        return key;
    }
    let key = crate::random_id();
    let _ = st.cache.meta_set(SILENCE_KEY, &key);
    key
}

pub fn silence_test_link(st: &AppState) -> String {
    link(&Target::Silence { key: silence_key(st) })
}

pub fn silence(app: &AppHandle, st: &AppState, minutes: u32) -> Result<(), String> {
    if !SILENCE_MINUTES.contains(&minutes) {
        return Err("That isn't one of the choices.".into());
    }
    let length = crate::test_override("WA_TEST_SILENCE_MS").and_then(|v| v.parse::<i64>().ok()).unwrap_or(minutes as i64 * MS_PER_MINUTE);
    st.cache.meta_set(SILENCE_UNTIL, &(now_ms() + length).to_string()).map_err(|e| e.to_string())?;
    silence_changed(app, st);
    Ok(())
}

pub fn silence_off(app: &AppHandle, st: &AppState) -> Result<(), String> {
    st.cache.meta_set(SILENCE_UNTIL, "0").map_err(|e| e.to_string())?;
    silence_changed(app, st);
    Ok(())
}

/// Local time for the tray.
pub fn clock(ms: i64) -> String {
    chrono::DateTime::from_timestamp_millis(ms)
        .map(|t| t.with_timezone(&chrono::Local).format("%-I:%M %p").to_string())
        .unwrap_or_default()
}

fn silence_changed(app: &AppHandle, st: &AppState) {
    let until = silenced_until(st);
    if let (Ok(menu), Some(tray)) = (tray_menu(app, until), st.tray.lock().unwrap().as_ref()) {
        let _ = tray.set_menu(Some(menu));
    }
    crate::update_tray(app, st);
    let _ = app.emit("silence", until);
}

pub fn tray_menu(app: &AppHandle, until: Option<i64>) -> tauri::Result<Menu<tauri::Wry>> {
    let menu = Menu::new(app)?;
    menu.append(&MenuItem::with_id(app, "open", "Open WA Relay", true, None::<&str>)?)?;
    match until {
        Some(until) => {
            menu.append(&MenuItem::with_id(app, "silence-state", format!("Notifications silenced until {}", clock(until)), false, None::<&str>)?)?;
            menu.append(&MenuItem::with_id(app, "silence-off", "Turn notifications back on", true, None::<&str>)?)?;
        }
        None => {
            let choices = Submenu::new(app, "Silence notifications", true)?;
            for minutes in SILENCE_MINUTES {
                choices.append(&MenuItem::with_id(app, format!("silence:{minutes}"), silence_label(minutes), true, None::<&str>)?)?;
            }
            menu.append(&choices)?;
        }
    }
    menu.append(&PredefinedMenuItem::separator(app)?)?;
    menu.append(&MenuItem::with_id(app, "quit", "Quit", true, None::<&str>)?)?;
    Ok(menu)
}

pub fn silence_label(minutes: u32) -> String {
    match minutes {
        60 => "1 hour".to_string(),
        m if m > 60 => format!("{} hours", m / 60),
        m => format!("{m} minutes"),
    }
}

pub fn on_tray_menu(app: &AppHandle, st: &AppState, id: &str) {
    if id == "silence-off" {
        let _ = silence_off(app, st);
    } else if let Some(minutes) = id.strip_prefix("silence:").and_then(|m| m.parse().ok()) {
        let _ = silence(app, st, minutes);
    }
}

/// The tray, Settings and tooltip catch up by themselves when it ends, including after sleep.
pub async fn watch_silence(app: AppHandle, st: Arc<AppState>) {
    let mut was = silenced_until(&st);
    loop {
        tokio::time::sleep(Duration::from_secs(10)).await;
        let now = silenced_until(&st);
        if now != was {
            was = now;
            if now.is_none() {
                silence_changed(&app, &st);
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn url(rest: &str) -> String {
        format!("{SCHEME}://{rest}")
    }

    #[test]
    fn a_link_comes_back_as_what_it_was_made_from() {
        for target in [
            Target::App,
            Target::Claude,
            Target::Chat { jid: "15550000001@s.whatsapp.net".into() },
            Target::Chat { jid: "120363000000000001@g.us".into() },
            Target::Chat { jid: "15550000001-1624124972@g.us".into() },
            Target::Chat { jid: "99999@lid".into() },
            Target::Silence { key: "0123456789abcdef".into() },
        ] {
            assert_eq!(parse(&link(&target)), Some(target));
        }
    }

    #[test]
    fn other_links_and_arguments_are_not_ours() {
        for arg in ["--minimized", "", "C:\\Windows\\notepad.exe", "https://open/?chat=1@g.us", "warelay", "warelayother://open"] {
            assert_eq!(parse(arg), None, "{arg}");
        }
        assert_eq!(parse(&url("somewhere-else")), None);
        assert_eq!(parse(&url("")), None);
    }

    #[test]
    fn a_bad_address_opens_no_chat() {
        for chat in ["..%2F..%2Fx", "a%20b", "%22%3Cx%3E", "", &"1".repeat(81), "x%00y", "a%2Fb@g.us", "x%26chat%3D1"] {
            assert_eq!(parse(&url(&format!("open?chat={chat}"))), Some(Target::App), "{chat}");
        }
    }

    #[test]
    fn silence_ends_at_its_time_and_a_moved_clock_cant_keep_it_on() {
        let longest = 240 * MS_PER_MINUTE;
        let start = 1_800_000_000_000;
        assert!(still_silenced(start + longest, start, longest), "the longest choice");
        assert!(still_silenced(start + 1, start, longest));
        assert!(!still_silenced(start, start, longest), "ends exactly on time");
        assert!(!still_silenced(start, start + 1, longest), "after sleeping through the end");
        assert!(!still_silenced(start + longest, start - 365 * 24 * 60 * MS_PER_MINUTE, longest), "clock set back a year");
    }

    #[test]
    fn silence_choices_are_labelled_as_asked() {
        let labels: Vec<_> = SILENCE_MINUTES.iter().map(|m| silence_label(*m)).collect();
        assert_eq!(labels, ["15 minutes", "30 minutes", "1 hour", "2 hours", "3 hours", "4 hours"]);
    }

    #[test]
    fn message_text_stays_out_of_the_link_and_cant_change_the_layout() {
        let nasty = "</text><action content=\"Pwn\" activationType=\"protocol\" arguments=\"https://example.com\"/>&\u{1}'\"<text>";
        let xml = toast_xml(nasty, nasty, &link(&Target::Chat { jid: "1@g.us".into() }), &link(&Target::App));
        assert!(!xml.contains("Pwn\""));
        assert!(!xml.contains("example.com\""));
        assert!(!xml.contains('\u{1}'));
        assert_eq!(xml.matches("<action ").count(), 3);
        assert_eq!(xml.matches("<text>").count(), 2);
    }

    /// Windows' own XML reader and toast class, without showing anything.
    #[cfg(windows)]
    #[test]
    fn windows_accepts_it_and_reads_it_as_intended() {
        use windows::core::HSTRING;
        use windows::Data::Xml::Dom::XmlDocument;
        use windows::UI::Notifications::ToastNotification;
        let nasty = "a </text><action content=\"Pwn\"/> & <b> \u{1} 📷 \"quoted\" 'x'";
        let target = link(&Target::Chat { jid: "1@g.us".into() });
        let doc = XmlDocument::new().unwrap();
        let silence = link(&Target::Silence { key: "0123456789abcdef".into() });
        doc.LoadXml(&HSTRING::from(toast_xml(nasty, nasty, &target, &silence))).unwrap();
        ToastNotification::CreateToastNotification(&doc).unwrap();
        let count = |name: &str| doc.GetElementsByTagName(&HSTRING::from(name)).unwrap().Length().unwrap();
        assert_eq!(count("text"), 2);
        assert_eq!(count("action"), 3);
        let attr = |name: &str, index: u32, key: &str| {
            let node = doc.GetElementsByTagName(&HSTRING::from(name)).unwrap().Item(index).unwrap();
            let attrs = node.Attributes().unwrap();
            attrs.GetNamedItem(&HSTRING::from(key)).unwrap().InnerText().unwrap().to_string()
        };
        assert_eq!(attr("toast", 0, "launch"), target);
        assert_eq!(attr("toast", 0, "activationType"), "protocol");
        assert_eq!(attr("action", 0, "arguments"), target);
        assert_eq!(attr("action", 1, "content"), "Silence 1 hour");
        assert_eq!(attr("action", 1, "arguments"), silence);
        assert_eq!(attr("action", 2, "content"), "Ignore");
        assert_eq!(attr("action", 2, "activationType"), "system");
        assert_eq!(attr("action", 2, "arguments"), "dismiss");
        let text = doc.GetElementsByTagName(&HSTRING::from("text")).unwrap().Item(1).unwrap().InnerText().unwrap().to_string();
        assert!(text.contains("<action") && text.contains('📷') && !text.contains('\u{1}'));
    }
}
