//! Starting the app while a copy is already running hands over to that copy and leaves. If that copy has stopped
//! answering, this says so, rather than seeming to do nothing. It never ends the other copy, and never runs beside it:
//! both would be writing to the same saved chats.

/// Whether a running copy had this long to answer, and didn't. It answers on its main thread, so one that's only busy
/// for a moment isn't counted.
#[cfg(windows)]
const ANSWER_MS: u32 = 10_000;

const MESSAGE: &str = "WA Relay is already running, but it isn't answering.\n\nWait a minute and open it again. If it still \
doesn't open, end \u{201c}WA Relay\u{201d} in Task Manager and then open it. Your chats are safe either way.";

/// Finds the window the single-instance plugin gives a running copy, by the names it uses, and asks it to answer. With
/// none, or one that can't be asked, starting carries on as usual and the plugin decides.
#[cfg(windows)]
pub fn running_copy_is_stuck(identifier: &str) -> bool {
    use windows::core::HSTRING;
    use windows::Win32::Foundation::{GetLastError, SetLastError, ERROR_SUCCESS, ERROR_TIMEOUT, LPARAM, WPARAM};
    use windows::Win32::UI::WindowsAndMessaging::{FindWindowW, SendMessageTimeoutW, SMTO_ERRORONEXIT, SMTO_NORMAL, WM_NULL};
    // Two copies side by side, for a test: neither hands over to the other.
    if crate::test_override("WA_SECOND_INSTANCE").is_some() {
        return false;
    }
    let (class, name) = (HSTRING::from(format!("{identifier}-sic")), HSTRING::from(format!("{identifier}-siw")));
    unsafe {
        let Ok(window) = FindWindowW(&class, &name) else { return false };
        SetLastError(ERROR_SUCCESS);
        let answered = SendMessageTimeoutW(window, WM_NULL, WPARAM(0), LPARAM(0), SMTO_NORMAL | SMTO_ERRORONEXIT, ANSWER_MS, None);
        answered.0 == 0 && GetLastError() == ERROR_TIMEOUT
    }
}

#[cfg(not(windows))]
pub fn running_copy_is_stuck(_: &str) -> bool {
    false
}

/// A plain message box, which needs nothing from the rest of the app. Not at sign-in to Windows, when nobody asked.
pub fn say_so() {
    if std::env::args().any(|a| a == "--minimized") {
        return;
    }
    // A test must not put a message on the screen of whoever's using this PC.
    if crate::test_override("WA_TEST_OFFSCREEN").is_some() {
        println!("{MESSAGE}");
        return;
    }
    #[cfg(windows)]
    {
        use windows::core::HSTRING;
        use windows::Win32::UI::WindowsAndMessaging::{MessageBoxW, MB_ICONWARNING, MB_OK, MB_SETFOREGROUND};
        unsafe {
            MessageBoxW(None, &HSTRING::from(MESSAGE), &HSTRING::from("WA Relay"), MB_OK | MB_ICONWARNING | MB_SETFOREGROUND);
        }
    }
}

#[cfg(all(test, windows))]
mod tests {
    use super::*;

    #[test]
    fn with_no_copy_running_nothing_is_stuck() {
        let start = std::time::Instant::now();
        assert!(!running_copy_is_stuck("app.warelay.nobody-runs-this"));
        assert!(start.elapsed() < std::time::Duration::from_secs(2), "and it doesn't wait to find out");
    }
}
