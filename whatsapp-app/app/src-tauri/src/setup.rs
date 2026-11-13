//! Which relay this copy talks to, and carrying data over from an earlier app identifier.
//!
//! Nothing here names anyone's relay. A build can have one built in by setting `WA_DEFAULT_RELAY` when it's
//! compiled; otherwise the first run asks for it. Either way it's saved, so a later build without one built
//! in keeps working.

use crate::db::Cache;
use crate::{test_build, test_override};
use std::path::{Path, PathBuf};

pub const BUILT_IN_RELAY: Option<&str> = option_env!("WA_DEFAULT_RELAY");
const SAVED: &str = "relay_url";

/// Checks and tidies a relay address: https, a host name, and nothing after it. A test build may also use
/// plain http to this PC. "relay.example.com" is taken as https.
pub fn clean_address(raw: &str) -> Result<String, String> {
    let raw = raw.trim();
    if raw.is_empty() {
        return Err("Enter your relay's address.".into());
    }
    let full = if raw.contains("://") { raw.to_string() } else { format!("https://{raw}") };
    let url = url::Url::parse(&full).map_err(|_| "That isn't a web address.".to_string())?;
    let host = url.host_str().filter(|h| !h.is_empty()).ok_or("The address needs a host name.")?;
    let local = matches!(host, "127.0.0.1" | "localhost");
    match url.scheme() {
        "https" => {}
        "http" if local && test_build() => {}
        _ => return Err("The address must start with https://.".into()),
    }
    if !url.username().is_empty() || url.password().is_some() {
        return Err("The address can't contain a user name or password.".into());
    }
    if url.path() != "/" || url.query().is_some() || url.fragment().is_some() {
        return Err("Enter just the address, with nothing after the host name.".into());
    }
    Ok(match url.port() {
        Some(port) => format!("{}://{host}:{port}", url.scheme()),
        None => format!("{}://{host}", url.scheme()),
    })
}

/// The relay for this copy: a test's own, then the saved one, then one built in, which is saved the first
/// time. None means the first run hasn't been told yet.
pub fn relay_address(cache: &Cache) -> Option<String> {
    if let Some(test) = test_override("WA_RELAY_URL") {
        return Some(test);
    }
    if let Some(saved) = cache.meta_get(SAVED).ok().flatten().filter(|s| !s.is_empty()) {
        return Some(saved);
    }
    let built_in = clean_address(BUILT_IN_RELAY?).ok()?;
    let _ = cache.meta_set(SAVED, &built_in);
    Some(built_in)
}

pub fn save_address(cache: &Cache, raw: &str) -> Result<String, String> {
    let clean = clean_address(raw)?;
    cache.meta_set(SAVED, &clean).map_err(|e| e.to_string())?;
    Ok(clean)
}

/// Earlier releases used another identifier, so their data sits in a sibling folder ending ".warelay" (or
/// ".warelay.updtest" for the update test's builds). The first start under a new identifier moves it
/// across, logins and settings included, as long as there's nothing here yet. Returns the folder it came
/// from. Test builds only do this when a test asks, so they can't touch a real copy's data.
pub fn adopt_earlier_data(data_root: &Path, identifier: &str) -> Option<PathBuf> {
    if test_build() && test_override("WA_ADOPT_EARLIER_DATA").is_none() {
        return None;
    }
    if data_root.join("cache.db").exists() {
        return None;
    }
    let family = if identifier.ends_with(".updtest") { ".warelay.updtest" } else { ".warelay" };
    let parent = data_root.parent()?;
    let from = std::fs::read_dir(parent)
        .ok()?
        .flatten()
        .filter(|e| e.file_type().map(|t| t.is_dir()).unwrap_or(false))
        .filter(|e| {
            let name = e.file_name().to_string_lossy().to_lowercase();
            name.ends_with(family) && name != identifier.to_lowercase()
        })
        .filter_map(|e| {
            let modified = std::fs::metadata(e.path().join("cache.db")).and_then(|m| m.modified()).ok()?;
            Some((modified, e.path()))
        })
        .max()
        .map(|(_, path)| path)?;
    std::fs::create_dir_all(data_root).ok()?;
    let mut moved = 0;
    for entry in std::fs::read_dir(&from).ok()?.flatten() {
        let target = data_root.join(entry.file_name());
        if !target.exists() && std::fs::rename(entry.path(), &target).is_ok() {
            moved += 1;
        }
    }
    // Whatever couldn't move stays where it was. An empty folder goes.
    let _ = std::fs::remove_dir(&from);
    (moved > 0).then_some(from)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn addresses_are_tidied_or_refused() {
        assert_eq!(clean_address("relay.example.com").unwrap(), "https://relay.example.com");
        assert_eq!(clean_address(" https://relay.example.com/ ").unwrap(), "https://relay.example.com");
        assert_eq!(clean_address("https://relay.example.com:8443").unwrap(), "https://relay.example.com:8443");
        for bad in [
            "",
            "   ",
            "http://relay.example.com",
            "ftp://relay.example.com",
            "https://relay.example.com/path",
            "https://relay.example.com/?q=1",
            "https://relay.example.com/#x",
            "https://user:pw@relay.example.com",
            "https://",
            "not a url at all",
        ] {
            assert!(clean_address(bad).is_err(), "{bad:?}");
        }
        // Plain http only for a test build talking to this PC.
        assert_eq!(clean_address("http://127.0.0.1:18791").is_ok(), test_build());
    }

    #[test]
    fn earlier_data_is_adopted_once_and_only_from_its_own_family() {
        std::env::set_var("WA_ADOPT_EARLIER_DATA", "1");
        let root = tempfile::tempdir().unwrap();
        let old = root.path().join("example.old.warelay");
        let other = root.path().join("example.old.warelay.updtest");
        let unrelated = root.path().join("someone.else");
        for dir in [&old, &other, &unrelated] {
            std::fs::create_dir_all(dir.join("EBWebView")).unwrap();
            std::fs::write(dir.join("cache.db"), dir.to_string_lossy().as_bytes()).unwrap();
        }
        let new = root.path().join("app.warelay.desktop");
        assert_eq!(adopt_earlier_data(&new, "app.warelay.desktop").as_deref(), Some(old.as_path()));
        assert_eq!(std::fs::read_to_string(new.join("cache.db")).unwrap(), old.to_string_lossy());
        assert!(new.join("EBWebView").is_dir() && !old.exists());
        assert!(other.join("cache.db").exists() && unrelated.join("cache.db").exists(), "other families untouched");
        // Once there's data here, nothing more is taken.
        std::fs::create_dir_all(old.join("x")).unwrap();
        std::fs::write(old.join("cache.db"), "newer").unwrap();
        assert_eq!(adopt_earlier_data(&new, "app.warelay.desktop"), None);
        // A test build's identifier only looks at test builds' folders.
        let test_new = root.path().join("app.warelay.updtest");
        assert_eq!(adopt_earlier_data(&test_new, "app.warelay.updtest").as_deref(), Some(other.as_path()));
    }
}
