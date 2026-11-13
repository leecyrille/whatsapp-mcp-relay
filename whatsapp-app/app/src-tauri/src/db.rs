//! The local cache: every chat and message, in SQLite with a full-text index, so the chat list,
//! message paging and search never wait on the network. Rows arrive by sequence number from the home
//! PC, which holds the only WhatsApp link.
//!
//! The same person can be addressed by phone number or by LID. Messages keep the address they
//! arrived under (raw_chat, needed to fetch media and send receipts) and are grouped under one
//! canonical chat for display. Canonicals are recomputed when the alias list grows.

use base64::Engine;
use rusqlite::{params, Connection, OptionalExtension};
use serde::{Deserialize, Serialize};
use std::collections::{HashMap, HashSet};
use std::path::Path;
use std::sync::{Mutex, RwLock};

const SCHEMA: &str = r#"
CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS messages (
    rowid INTEGER PRIMARY KEY,
    raw_chat TEXT NOT NULL,
    id TEXT NOT NULL,
    chat TEXT NOT NULL,
    sender TEXT NOT NULL DEFAULT '',
    sender_jid TEXT NOT NULL DEFAULT '',
    push_name TEXT NOT NULL DEFAULT '',
    text TEXT NOT NULL DEFAULT '',
    ts INTEGER NOT NULL,
    from_me INTEGER NOT NULL DEFAULT 0,
    media TEXT NOT NULL DEFAULT '',
    filename TEXT NOT NULL DEFAULT '',
    mime TEXT NOT NULL DEFAULT '',
    size INTEGER NOT NULL DEFAULT 0,
    thumb BLOB,
    width INTEGER NOT NULL DEFAULT 0,
    height INTEGER NOT NULL DEFAULT 0,
    seconds INTEGER NOT NULL DEFAULT 0,
    quoted_id TEXT NOT NULL DEFAULT '',
    quoted_sender TEXT NOT NULL DEFAULT '',
    quoted_text TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL DEFAULT '',
    edited_ms INTEGER NOT NULL DEFAULT 0,
    revoked INTEGER NOT NULL DEFAULT 0,
    reactions TEXT NOT NULL DEFAULT '',
    UNIQUE (raw_chat, id)
);
CREATE INDEX IF NOT EXISTS idx_msg_chat_ts ON messages (chat, ts DESC, rowid DESC);
CREATE VIRTUAL TABLE IF NOT EXISTS messages_fts USING fts5(
    text, filename, content='messages', content_rowid='rowid', tokenize='unicode61 remove_diacritics 2'
);
CREATE TRIGGER IF NOT EXISTS messages_ai AFTER INSERT ON messages BEGIN
    INSERT INTO messages_fts (rowid, text, filename) VALUES (new.rowid, new.text, new.filename);
END;
CREATE TRIGGER IF NOT EXISTS messages_ad AFTER DELETE ON messages BEGIN
    INSERT INTO messages_fts (messages_fts, rowid, text, filename) VALUES ('delete', old.rowid, old.text, old.filename);
END;
CREATE TRIGGER IF NOT EXISTS messages_au AFTER UPDATE OF text, filename ON messages BEGIN
    INSERT INTO messages_fts (messages_fts, rowid, text, filename) VALUES ('delete', old.rowid, old.text, old.filename);
    INSERT INTO messages_fts (rowid, text, filename) VALUES (new.rowid, new.text, new.filename);
END;
CREATE TABLE IF NOT EXISTS raw_chats (
    jid TEXT PRIMARY KEY,
    chat TEXT NOT NULL,
    name TEXT NOT NULL DEFAULT '',
    last_ms INTEGER NOT NULL DEFAULT 0,
    last_read_ms INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_raw_chat ON raw_chats (chat);
CREATE TABLE IF NOT EXISTS chats (
    jid TEXT PRIMARY KEY,
    name TEXT NOT NULL DEFAULT '',
    is_group INTEGER NOT NULL DEFAULT 0,
    last_ms INTEGER NOT NULL DEFAULT 0,
    preview TEXT NOT NULL DEFAULT '',
    preview_from_me INTEGER NOT NULL DEFAULT 0,
    preview_status TEXT NOT NULL DEFAULT '',
    unread INTEGER NOT NULL DEFAULT 0,
    last_read_ms INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_chats_last ON chats (last_ms DESC);
CREATE TABLE IF NOT EXISTS contacts (
    jid TEXT PRIMARY KEY,
    full_name TEXT NOT NULL DEFAULT '',
    first_name TEXT NOT NULL DEFAULT '',
    push_name TEXT NOT NULL DEFAULT '',
    business_name TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS aliases (lid TEXT PRIMARY KEY, pn TEXT NOT NULL);
"#;

mod b64 {
    use base64::Engine;
    use serde::{Deserialize, Deserializer};
    pub fn de<'de, D: Deserializer<'de>>(d: D) -> Result<Option<Vec<u8>>, D::Error> {
        let raw: Option<String> = Option::deserialize(d)?;
        Ok(raw.and_then(|s| base64::engine::general_purpose::STANDARD.decode(s).ok()))
    }
}

/// One message as the home PC sends it.
#[derive(Debug, Clone, Deserialize, Default)]
pub struct WireMsg {
    #[serde(default)]
    pub seq: i64,
    pub id: String,
    pub chat: String,
    #[serde(default)]
    pub sender: String,
    #[serde(default)]
    pub sender_jid: String,
    #[serde(default)]
    pub push_name: String,
    #[serde(default)]
    pub text: String,
    #[serde(default)]
    pub ts: i64,
    #[serde(default)]
    pub from_me: bool,
    #[serde(default)]
    pub media: String,
    #[serde(default)]
    pub filename: String,
    #[serde(default)]
    pub mime: String,
    #[serde(default)]
    pub size: i64,
    #[serde(default, deserialize_with = "b64::de")]
    pub thumb: Option<Vec<u8>>,
    #[serde(default)]
    pub width: i64,
    #[serde(default)]
    pub height: i64,
    #[serde(default)]
    pub seconds: i64,
    #[serde(default)]
    pub quoted_id: String,
    #[serde(default)]
    pub quoted_sender: String,
    #[serde(default)]
    pub quoted_text: String,
    #[serde(default)]
    pub status: String,
    #[serde(default)]
    pub edited_ms: i64,
    #[serde(default)]
    pub revoked: bool,
    #[serde(default)]
    pub reactions: Option<serde_json::Value>,
    /// Deleted for me, here or on the phone: removed from this PC's copy.
    #[serde(default)]
    pub hidden: bool,
}

/// One chat as the home PC sends it.
#[derive(Debug, Clone, Deserialize, Default)]
pub struct WireChat {
    pub jid: String,
    #[serde(default)]
    pub name: String,
    #[serde(default)]
    pub last_ms: i64,
    #[serde(default)]
    pub last_read_ms: i64,
}

#[derive(Debug, Clone, Deserialize, Default)]
pub struct WireContact {
    pub jid: String,
    #[serde(default)]
    pub full_name: String,
    #[serde(default)]
    pub first_name: String,
    #[serde(default)]
    pub push_name: String,
    #[serde(default)]
    pub business_name: String,
}

#[derive(Debug, Clone, Serialize)]
pub struct ChatRow {
    pub jid: String,
    pub name: String,
    pub is_group: bool,
    pub last_ms: i64,
    pub preview: String,
    pub preview_from_me: bool,
    pub preview_status: String,
    pub unread: i64,
}

#[derive(Debug, Clone, Serialize)]
pub struct MsgRow {
    pub rowid: i64,
    pub raw_chat: String,
    pub id: String,
    pub chat: String,
    pub sender_name: String,
    pub sender_jid: String,
    /// The one address this person goes by here, which is also what their picture is filed under. Empty
    /// for the user's own messages, and for old group rows that never recorded who sent them.
    pub person: String,
    pub from_me: bool,
    pub ts: i64,
    pub text: String,
    pub media: String,
    pub filename: String,
    pub mime: String,
    pub size: i64,
    pub has_thumb: bool,
    pub width: i64,
    pub height: i64,
    pub seconds: i64,
    pub quoted_id: String,
    pub quoted_name: String,
    pub quoted_text: String,
    pub status: String,
    pub edited: bool,
    pub revoked: bool,
    pub reactions: serde_json::Value,
    /// Names for the @mentions in the text, by the digits in it.
    pub mentions: HashMap<String, String>,
}

#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct WireMember {
    pub jid: String,
    #[serde(default)]
    pub phone: String,
    #[serde(default)]
    pub lid: String,
    #[serde(default)]
    pub admin: bool,
}

#[derive(Debug, Clone, Serialize)]
pub struct MemberRow {
    pub jid: String,
    /// The same address a message from this person carries, in `MsgRow::person`.
    pub person: String,
    pub lid: String,
    pub name: String,
    pub you: bool,
    pub admin: bool,
}

#[derive(Debug, Clone, Serialize)]
pub struct SearchHit {
    pub rowid: i64,
    pub chat: String,
    pub chat_name: String,
    pub ts: i64,
    pub from_me: bool,
    pub sender_name: String,
    pub snippet: String,
    pub media: String,
}

#[derive(Debug, Clone, Serialize)]
pub struct ContactRow {
    pub jid: String,
    pub name: String,
    pub number: String,
    pub has_chat: bool,
}

#[derive(Debug, Clone, Serialize)]
pub struct MediaInfo {
    pub raw_chat: String,
    pub id: String,
    pub media: String,
    pub filename: String,
    pub mime: String,
    pub size: i64,
    pub thumb: Option<Vec<u8>>,
}

/// What to tell the home PC when a chat is read.
#[derive(Debug, Clone, Serialize)]
pub struct ReadBatch {
    pub chat: String,
    pub up_to_ms: i64,
    pub messages: Vec<ReadItem>,
}

#[derive(Debug, Clone, Serialize)]
pub struct ReadItem {
    pub id: String,
    pub sender_jid: String,
}

#[derive(Debug, Clone, Serialize)]
pub struct SortItem {
    pub jid: String,
    pub name: String,
    pub is_group: bool,
    pub snippets: Vec<String>,
}

pub type DbResult<T> = Result<T, String>;

fn err(e: rusqlite::Error) -> String {
    e.to_string()
}

/// One writer and one reader, so a big sync never holds up scrolling or search.
pub struct Cache {
    writer: Mutex<Connection>,
    reader: Mutex<Connection>,
    names: RwLock<HashMap<String, String>>,
    aliases: RwLock<HashMap<String, String>>,
    own: RwLock<HashSet<String>>,
}

fn is_group(jid: &str) -> bool {
    jid.ends_with("@g.us")
}

fn number_of(jid: &str) -> String {
    let user = jid.split('@').next().unwrap_or("");
    if jid.ends_with("@s.whatsapp.net") && !user.is_empty() {
        format!("+{user}")
    } else {
        String::new()
    }
}

fn pick_name(c: &WireContact) -> String {
    for candidate in [&c.full_name, &c.first_name, &c.business_name, &c.push_name] {
        if !candidate.trim().is_empty() {
            return candidate.trim().to_string();
        }
    }
    String::new()
}

/// How much a contact's name can be trusted to be the one the user knows them by. A saved name beats the name
/// someone gave themselves.
fn name_rank(c: &WireContact) -> u8 {
    if !c.full_name.trim().is_empty() || !c.first_name.trim().is_empty() {
        0
    } else if !c.business_name.trim().is_empty() {
        1
    } else {
        2
    }
}

/// The IDs in a message's @mentions, by phone number or hidden ID. An email address isn't one.
fn mention_ids(text: &str) -> Vec<&str> {
    let mut found = vec![];
    for (at, _) in text.match_indices('@') {
        if text[..at].chars().next_back().is_some_and(char::is_alphanumeric) {
            continue;
        }
        let rest = &text[at + 1..];
        let digits = rest.find(|c: char| !c.is_ascii_digit()).unwrap_or(rest.len());
        let after = rest[digits..].chars().next();
        if (5..=20).contains(&digits) && !after.is_some_and(char::is_alphanumeric) {
            found.push(&rest[..digits]);
        }
    }
    found
}

fn looks_numeric(s: &str) -> bool {
    !s.is_empty() && s.chars().all(|c| c.is_ascii_digit())
}

fn preview_of(text: &str, media: &str, filename: &str, revoked: bool) -> String {
    if revoked {
        return "This message was deleted".into();
    }
    let flat: String = text.split_whitespace().collect::<Vec<_>>().join(" ");
    let label = match media {
        "image" => "📷 Photo",
        "video" => "🎥 Video",
        "audio" => "🎤 Audio",
        "sticker" => "Sticker",
        "document" => {
            if flat.is_empty() {
                return format!("📄 {filename}");
            }
            "📄"
        }
        _ => return flat,
    };
    if flat.is_empty() {
        label.into()
    } else {
        format!("{label} {flat}")
    }
}

impl Cache {
    pub fn open(path: &Path) -> DbResult<Cache> {
        let writer = Connection::open(path).map_err(err)?;
        writer
            .execute_batch(
                "PRAGMA journal_mode=WAL; PRAGMA synchronous=NORMAL; PRAGMA foreign_keys=OFF;
                 PRAGMA temp_store=MEMORY; PRAGMA mmap_size=268435456; PRAGMA cache_size=-32000;
                 PRAGMA busy_timeout=10000;",
            )
            .map_err(err)?;
        writer.execute_batch(SCHEMA).map_err(err)?;
        let reader = Connection::open(path).map_err(err)?;
        reader
            .execute_batch(
                "PRAGMA query_only=ON; PRAGMA mmap_size=268435456; PRAGMA cache_size=-32000; PRAGMA busy_timeout=10000;",
            )
            .map_err(err)?;
        let cache = Cache {
            writer: Mutex::new(writer),
            reader: Mutex::new(reader),
            names: RwLock::new(HashMap::new()),
            aliases: RwLock::new(HashMap::new()),
            own: RwLock::new(HashSet::new()),
        };
        cache.load_maps()?;
        if let Some(raw) = cache.meta_get("own_jids")? {
            *cache.own.write().unwrap() = raw.split(',').map(str::to_string).collect();
        }
        Ok(cache)
    }

    fn load_maps(&self) -> DbResult<()> {
        let conn = self.reader.lock().unwrap();
        let mut names = HashMap::new();
        let mut ranks: HashMap<String, u8> = HashMap::new();
        {
            let mut stmt = conn
                .prepare("SELECT jid, full_name, first_name, push_name, business_name FROM contacts")
                .map_err(err)?;
            let rows = stmt
                .query_map([], |r| {
                    Ok(WireContact {
                        jid: r.get(0)?,
                        full_name: r.get(1)?,
                        first_name: r.get(2)?,
                        push_name: r.get(3)?,
                        business_name: r.get(4)?,
                    })
                })
                .map_err(err)?;
            for c in rows.flatten() {
                let name = pick_name(&c);
                if !name.is_empty() {
                    ranks.insert(c.jid.clone(), name_rank(&c));
                    names.insert(c.jid, name);
                }
            }
        }
        let mut aliases = HashMap::new();
        {
            let mut stmt = conn.prepare("SELECT lid, pn FROM aliases").map_err(err)?;
            let rows = stmt
                .query_map([], |r| Ok((r.get::<_, String>(0)?, r.get::<_, String>(1)?)))
                .map_err(err)?;
            for (lid, pn) in rows.flatten() {
                aliases.insert(lid, pn);
            }
        }
        // A contact stored only under one address still names the person at the other.
        let mut merged = names.clone();
        for (lid, pn) in &aliases {
            match (names.get(lid), names.get(pn)) {
                (Some(n), None) => {
                    merged.insert(pn.clone(), n.clone());
                }
                (None, Some(n)) => {
                    merged.insert(lid.clone(), n.clone());
                }
                // The saved name often sits at the number, with only a profile name at the hidden ID.
                (Some(at_lid), Some(at_pn)) => match ranks.get(lid).cmp(&ranks.get(pn)) {
                    std::cmp::Ordering::Less => {
                        merged.insert(pn.clone(), at_lid.clone());
                    }
                    std::cmp::Ordering::Greater => {
                        merged.insert(lid.clone(), at_pn.clone());
                    }
                    std::cmp::Ordering::Equal => {}
                },
                _ => {}
            }
        }
        *self.names.write().unwrap() = merged;
        *self.aliases.write().unwrap() = aliases;
        Ok(())
    }

    /// The user's own addresses, so a quote of their own message says "You" and not their number.
    pub fn set_own(&self, jids: Vec<String>) -> DbResult<()> {
        let set: HashSet<String> = jids.iter().cloned().collect();
        if *self.own.read().unwrap() == set {
            return Ok(());
        }
        self.meta_set("own_jids", &jids.join(","))?;
        *self.own.write().unwrap() = set;
        Ok(())
    }

    pub fn canon(&self, jid: &str) -> String {
        self.aliases
            .read()
            .unwrap()
            .get(jid)
            .cloned()
            .unwrap_or_else(|| jid.to_string())
    }

    /// The name for an @mention: saved name, then profile name, then the number behind a hidden ID. Nothing
    /// known means the raw text stays.
    pub fn mention_name(&self, id: &str) -> Option<String> {
        let (lid, pn) = (format!("{id}@lid"), format!("{id}@s.whatsapp.net"));
        {
            let own = self.own.read().unwrap();
            if own.contains(&lid) || own.contains(&pn) {
                return Some("You".to_string());
            }
        }
        if let Some(name) = {
            let names = self.names.read().unwrap();
            names.get(&lid).or_else(|| names.get(&pn)).cloned()
        } {
            return Some(name);
        }
        self.aliases.read().unwrap().get(&lid).map(|number| number_of(number)).filter(|n| !n.is_empty())
    }

    fn mentions_in(&self, text: &str) -> HashMap<String, String> {
        mention_ids(text).into_iter().filter_map(|id| Some((id.to_string(), self.mention_name(id)?))).collect()
    }

    /// For places that only show plain text: previews, notifications and search snippets. Stored messages
    /// keep the raw text, so names stay current.
    pub fn with_mention_names(&self, text: &str) -> String {
        let mut out = String::with_capacity(text.len());
        let mut rest = text;
        for id in mention_ids(text) {
            let Some(name) = self.mention_name(id) else { continue };
            let raw = format!("@{id}");
            let Some(at) = rest.find(&raw) else { continue };
            out.push_str(&rest[..at]);
            out.push('@');
            out.push_str(&name);
            rest = &rest[at + raw.len()..];
        }
        out.push_str(rest);
        out
    }

    /// A group member's row in the chat header. Addresses come from WhatsApp's group info, and the names
    /// from the same place message senders get theirs.
    pub fn member_row(&self, m: &WireMember) -> MemberRow {
        let addresses = [m.phone.as_str(), m.lid.as_str(), m.jid.as_str()];
        let you = {
            let own = self.own.read().unwrap();
            addresses.iter().any(|a| !a.is_empty() && own.contains(*a))
        };
        let phone = if !m.phone.is_empty() {
            m.phone.clone()
        } else {
            let aliases = self.aliases.read().unwrap();
            aliases.get(&m.lid).or_else(|| aliases.get(&m.jid)).cloned().unwrap_or_else(|| m.jid.clone())
        };
        let name = {
            let names = self.names.read().unwrap();
            addresses.iter().chain([&phone.as_str()]).find_map(|a| names.get(*a).cloned())
        };
        let name = name.filter(|n| !n.trim().is_empty()).unwrap_or_else(|| {
            let number = number_of(&phone);
            if number.is_empty() {
                "Unknown member".to_string()
            } else {
                number
            }
        });
        MemberRow { jid: m.jid.clone(), person: phone, lid: m.lid.clone(), name, you, admin: m.admin }
    }

    fn display_name(&self, jid: &str, stored: &str) -> String {
        if let Some(name) = self.names.read().unwrap().get(jid) {
            return name.clone();
        }
        if !stored.trim().is_empty() && !looks_numeric(stored.trim()) {
            return stored.trim().to_string();
        }
        let number = number_of(jid);
        if !number.is_empty() {
            number
        } else {
            "Unknown".into()
        }
    }

    pub fn cursor(&self) -> i64 {
        self.meta_get("cursor")
            .ok()
            .flatten()
            .and_then(|v| v.parse().ok())
            .unwrap_or(0)
    }

    pub fn meta_get(&self, key: &str) -> DbResult<Option<String>> {
        let conn = self.reader.lock().unwrap();
        conn.query_row("SELECT value FROM meta WHERE key = ?", [key], |r| r.get(0))
            .optional()
            .map_err(err)
    }

    pub fn meta_set(&self, key: &str, value: &str) -> DbResult<()> {
        let conn = self.writer.lock().unwrap();
        conn.execute(
            "INSERT INTO meta (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value",
            params![key, value],
        )
        .map_err(err)?;
        Ok(())
    }

    /// Applies one batch and advances the cursor in the same transaction, so a crash can't lose rows
    /// or skip them. Returns the chats that changed.
    pub fn apply_batch(&self, messages: &[WireMsg], chats: &[WireChat], cursor: Option<i64>) -> DbResult<Vec<String>> {
        let mut conn = self.writer.lock().unwrap();
        let tx = conn.transaction().map_err(err)?;
        let mut touched: HashSet<String> = HashSet::new();
        {
            let mut chat_stmt = tx
                .prepare_cached(
                    "INSERT INTO raw_chats (jid, chat, name, last_ms, last_read_ms) VALUES (?1, ?2, ?3, ?4, ?5)
                     ON CONFLICT(jid) DO UPDATE SET chat = excluded.chat,
                       name = CASE WHEN excluded.name != '' THEN excluded.name ELSE raw_chats.name END,
                       last_ms = MAX(raw_chats.last_ms, excluded.last_ms),
                       last_read_ms = MAX(raw_chats.last_read_ms, excluded.last_read_ms)",
                )
                .map_err(err)?;
            for c in chats {
                let canon = self.canon(&c.jid);
                chat_stmt
                    .execute(params![c.jid, canon, c.name, c.last_ms, c.last_read_ms])
                    .map_err(err)?;
                touched.insert(canon);
            }
            let mut msg_stmt = tx
                .prepare_cached(
                    "INSERT INTO messages (raw_chat, id, chat, sender, sender_jid, push_name, text, ts, from_me, media,
                        filename, mime, size, thumb, width, height, seconds, quoted_id, quoted_sender, quoted_text,
                        status, edited_ms, revoked, reactions)
                     VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8, ?9, ?10, ?11, ?12, ?13, ?14, ?15, ?16, ?17, ?18, ?19, ?20, ?21, ?22, ?23, ?24)
                     ON CONFLICT(raw_chat, id) DO UPDATE SET chat = excluded.chat, sender = excluded.sender,
                        sender_jid = excluded.sender_jid, push_name = excluded.push_name, text = excluded.text,
                        ts = excluded.ts, from_me = excluded.from_me, media = excluded.media, filename = excluded.filename,
                        mime = excluded.mime, size = excluded.size, thumb = COALESCE(excluded.thumb, messages.thumb),
                        width = excluded.width, height = excluded.height, seconds = excluded.seconds,
                        quoted_id = excluded.quoted_id, quoted_sender = excluded.quoted_sender,
                        quoted_text = excluded.quoted_text, status = excluded.status, edited_ms = excluded.edited_ms,
                        revoked = excluded.revoked, reactions = excluded.reactions",
                )
                .map_err(err)?;
            let mut ensure_chat = tx
                .prepare_cached("INSERT OR IGNORE INTO raw_chats (jid, chat, last_ms) VALUES (?1, ?2, ?3)")
                .map_err(err)?;
            for m in messages {
                let canon = self.canon(&m.chat);
                if m.hidden {
                    tx.execute("DELETE FROM messages WHERE raw_chat = ?1 AND id = ?2", params![m.chat, m.id]).map_err(err)?;
                    touched.insert(canon);
                    continue;
                }
                let reactions = m.reactions.as_ref().map(|v| v.to_string()).unwrap_or_default();
                msg_stmt
                    .execute(params![
                        m.chat, m.id, canon, m.sender, m.sender_jid, m.push_name, m.text, m.ts, m.from_me as i64,
                        m.media, m.filename, m.mime, m.size, m.thumb, m.width, m.height, m.seconds, m.quoted_id,
                        m.quoted_sender, m.quoted_text, m.status, m.edited_ms, m.revoked as i64, reactions
                    ])
                    .map_err(err)?;
                ensure_chat.execute(params![m.chat, canon, m.ts]).map_err(err)?;
                touched.insert(canon);
            }
            if let Some(cursor) = cursor {
                tx.execute(
                    "INSERT INTO meta (key, value) VALUES ('cursor', ?1) ON CONFLICT(key) DO UPDATE SET value = excluded.value",
                    [cursor.to_string()],
                )
                .map_err(err)?;
            }
            for chat in &touched {
                self.refresh_chat(&tx, chat).map_err(err)?;
            }
        }
        tx.commit().map_err(err)?;
        Ok(touched.into_iter().collect())
    }

    /// Rebuilds one chat's list row: name, last message and unread count.
    fn refresh_chat(&self, tx: &Connection, chat: &str) -> rusqlite::Result<()> {
        let (stored_name, last_read, raw_last): (String, i64, i64) = tx.query_row(
            "SELECT COALESCE((SELECT name FROM raw_chats WHERE chat = ?1 AND name != ''
                  ORDER BY (jid = ?1) DESC, last_ms DESC LIMIT 1), ''),
                COALESCE(MAX(last_read_ms), 0), COALESCE(MAX(last_ms), 0)
             FROM raw_chats WHERE chat = ?1",
            [chat],
            |r| Ok((r.get(0)?, r.get(1)?, r.get(2)?)),
        )?;
        let last: Option<(String, String, String, bool, String, bool, i64)> = tx
            .query_row(
                "SELECT text, media, filename, from_me, status, revoked, ts FROM messages
                 WHERE chat = ?1 ORDER BY ts DESC, rowid DESC LIMIT 1",
                [chat],
                |r| {
                    Ok((
                        r.get(0)?,
                        r.get(1)?,
                        r.get(2)?,
                        r.get::<_, i64>(3)? != 0,
                        r.get(4)?,
                        r.get::<_, i64>(5)? != 0,
                        r.get(6)?,
                    ))
                },
            )
            .optional()?;
        let unread: i64 = tx.query_row(
            "SELECT COUNT(*) FROM messages WHERE chat = ?1 AND ts > ?2 AND from_me = 0 AND revoked = 0",
            params![chat, last_read],
            |r| r.get(0),
        )?;
        let name = self.display_name(chat, &stored_name);
        let (preview, from_me, status, last_ms) = match last {
            Some((text, media, filename, from_me, status, revoked, ts)) => {
                (preview_of(&text, &media, &filename, revoked), from_me, status, ts)
            }
            None => (String::new(), false, String::new(), raw_last),
        };
        tx.execute(
            "INSERT INTO chats (jid, name, is_group, last_ms, preview, preview_from_me, preview_status, unread, last_read_ms)
             VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8, ?9)
             ON CONFLICT(jid) DO UPDATE SET name = excluded.name, is_group = excluded.is_group,
               last_ms = excluded.last_ms, preview = excluded.preview, preview_from_me = excluded.preview_from_me,
               preview_status = excluded.preview_status, unread = excluded.unread, last_read_ms = excluded.last_read_ms",
            params![chat, name, is_group(chat) as i64, last_ms, preview, from_me as i64, status, unread, last_read],
        )?;
        Ok(())
    }

    /// Replaces the contact list. If new aliases appeared, messages move under their canonical chat
    /// and the now-empty chat row disappears, which is what merges a duplicate person.
    pub fn set_contacts(&self, contacts: &[WireContact], aliases: &[(String, String)]) -> DbResult<()> {
        let mut conn = self.writer.lock().unwrap();
        let tx = conn.transaction().map_err(err)?;
        tx.execute("DELETE FROM contacts", []).map_err(err)?;
        tx.execute("DELETE FROM aliases", []).map_err(err)?;
        {
            let mut stmt = tx
                .prepare_cached("INSERT OR REPLACE INTO contacts (jid, full_name, first_name, push_name, business_name) VALUES (?, ?, ?, ?, ?)")
                .map_err(err)?;
            for c in contacts {
                stmt.execute(params![c.jid, c.full_name, c.first_name, c.push_name, c.business_name])
                    .map_err(err)?;
            }
            let mut stmt = tx.prepare_cached("INSERT OR REPLACE INTO aliases (lid, pn) VALUES (?, ?)").map_err(err)?;
            for (lid, pn) in aliases {
                stmt.execute(params![lid, pn]).map_err(err)?;
            }
        }
        tx.commit().map_err(err)?;
        drop(conn);
        self.load_maps()?;
        self.recanonicalize()
    }

    fn recanonicalize(&self) -> DbResult<()> {
        let mut conn = self.writer.lock().unwrap();
        let tx = conn.transaction().map_err(err)?;
        let raws: Vec<(String, String)> = {
            let mut stmt = tx.prepare("SELECT jid, chat FROM raw_chats").map_err(err)?;
            let rows = stmt
                .query_map([], |r| Ok((r.get(0)?, r.get(1)?)))
                .map_err(err)?;
            rows.flatten().collect()
        };
        let mut all: HashSet<String> = HashSet::new();
        for (jid, chat) in &raws {
            let canon = self.canon(jid);
            if &canon != chat {
                tx.execute("UPDATE raw_chats SET chat = ?1 WHERE jid = ?2", params![canon, jid])
                    .map_err(err)?;
                tx.execute("UPDATE messages SET chat = ?1 WHERE raw_chat = ?2", params![canon, jid])
                    .map_err(err)?;
                tx.execute("DELETE FROM chats WHERE jid = ?1", [chat]).map_err(err)?;
            }
            all.insert(canon);
        }
        for chat in &all {
            self.refresh_chat(&tx, chat).map_err(err)?;
        }
        tx.commit().map_err(err)
    }

    pub fn chat_list(&self) -> DbResult<Vec<ChatRow>> {
        let conn = self.reader.lock().unwrap();
        let mut stmt = conn
            .prepare_cached(
                "SELECT jid, name, is_group, last_ms, preview, preview_from_me, preview_status, unread
                 FROM chats WHERE last_ms > 0 OR preview != '' ORDER BY last_ms DESC",
            )
            .map_err(err)?;
        let rows = stmt
            .query_map([], |r| {
                Ok(ChatRow {
                    jid: r.get(0)?,
                    name: r.get(1)?,
                    is_group: r.get::<_, i64>(2)? != 0,
                    last_ms: r.get(3)?,
                    preview: self.with_mention_names(&r.get::<_, String>(4)?),
                    preview_from_me: r.get::<_, i64>(5)? != 0,
                    preview_status: r.get(6)?,
                    unread: r.get(7)?,
                })
            })
            .map_err(err)?;
        Ok(rows.flatten().collect())
    }

    pub fn total_unread(&self) -> i64 {
        let conn = self.reader.lock().unwrap();
        conn.query_row("SELECT COALESCE(SUM(unread), 0) FROM chats", [], |r| r.get(0))
            .unwrap_or(0)
    }

    /// Whether this chat is one the list would show.
    pub fn has_chat(&self, chat: &str) -> bool {
        let conn = self.reader.lock().unwrap();
        conn.query_row("SELECT 1 FROM chats WHERE jid = ? AND (last_ms > 0 OR preview != '')", [chat], |_| Ok(()))
            .is_ok()
    }

    pub fn chat_name(&self, chat: &str) -> String {
        let conn = self.reader.lock().unwrap();
        conn.query_row("SELECT name FROM chats WHERE jid = ?", [chat], |r| r.get(0))
            .unwrap_or_else(|_| self.display_name(chat, ""))
    }

    fn map_msg(&self, r: &rusqlite::Row<'_>) -> rusqlite::Result<MsgRow> {
        let from_me = r.get::<_, i64>(6)? != 0;
        let sender: String = r.get(3)?;
        let sender_jid: String = r.get(4)?;
        let push: String = r.get(5)?;
        let quoted_sender: String = r.get(19)?;
        let chat: String = r.get(2)?;
        let sender_name = if from_me {
            "You".to_string()
        } else {
            self.person_name(&sender_jid, &sender, &push, &chat)
        };
        let person = self.person_of(&chat, from_me, &sender, &sender_jid);
        let quoted_name = if quoted_sender.is_empty() {
            String::new()
        } else if self.own.read().unwrap().contains(&quoted_sender) {
            "You".to_string()
        } else {
            self.person_name(&quoted_sender, "", "", &chat)
        };
        let text: String = r.get(9)?;
        let reactions_raw: String = r.get(25)?;
        let reactions = if reactions_raw.is_empty() {
            serde_json::Value::Null
        } else {
            serde_json::from_str(&reactions_raw).unwrap_or(serde_json::Value::Null)
        };
        Ok(MsgRow {
            rowid: r.get(0)?,
            raw_chat: r.get(1)?,
            chat,
            id: r.get(7)?,
            sender_name,
            sender_jid,
            person,
            from_me,
            ts: r.get(8)?,
            mentions: self.mentions_in(&text),
            text,
            media: r.get(10)?,
            filename: r.get(11)?,
            mime: r.get(12)?,
            size: r.get(13)?,
            has_thumb: r.get::<_, i64>(14)? != 0,
            width: r.get(15)?,
            height: r.get(16)?,
            seconds: r.get(17)?,
            quoted_id: r.get(18)?,
            quoted_name,
            quoted_text: self.with_mention_names(&r.get::<_, String>(20)?),
            status: r.get(21)?,
            edited: r.get::<_, i64>(22)? != 0,
            revoked: r.get::<_, i64>(23)? != 0,
            reactions,
        })
    }

    /// Who a message is from, as one address however it arrived. One-to-one history often has no sender on
    /// it, so the chat says who. In a group, an old row may only have the group's own ID, which says nothing.
    fn person_of(&self, chat: &str, from_me: bool, sender: &str, sender_jid: &str) -> String {
        if from_me {
            return String::new();
        }
        if chat.ends_with("@s.whatsapp.net") || chat.ends_with("@lid") {
            return chat.to_string();
        }
        if !sender_jid.is_empty() {
            return self.canon(sender_jid);
        }
        if sender.is_empty() || sender == chat.split('@').next().unwrap_or("") {
            return String::new();
        }
        let lid = format!("{sender}@lid");
        let pn = format!("{sender}@s.whatsapp.net");
        if let Some(number) = self.aliases.read().unwrap().get(&lid) {
            return number.clone();
        }
        let names = self.names.read().unwrap();
        if names.contains_key(&lid) && !names.contains_key(&pn) {
            return lid;
        }
        pn
    }

    /// Everyone but the user who has written in a chat that this copy knows of.
    pub fn chat_senders(&self, chat: &str) -> DbResult<Vec<String>> {
        let pairs: Vec<(String, String)> = {
            let conn = self.reader.lock().unwrap();
            let mut stmt = conn
                .prepare_cached("SELECT DISTINCT sender, sender_jid FROM messages WHERE chat = ?1 AND from_me = 0")
                .map_err(err)?;
            let rows = stmt.query_map([chat], |r| Ok((r.get(0)?, r.get(1)?))).map_err(err)?;
            rows.flatten().collect()
        };
        let mut people: Vec<String> = pairs.iter().map(|(sender, jid)| self.person_of(chat, false, sender, jid)).collect();
        let own = self.own.read().unwrap();
        people.retain(|p| !p.is_empty() && !own.contains(p));
        people.sort();
        people.dedup();
        Ok(people)
    }

    /// A person's display name in a chat. History from before the bridge fix stored the group's own
    /// ID as the sender, so that comes back empty, not as a wrong name.
    fn person_name(&self, jid: &str, sender: &str, push: &str, chat: &str) -> String {
        let chat_user = chat.split('@').next().unwrap_or("");
        if !jid.is_empty() {
            let names = self.names.read().unwrap();
            if let Some(name) = names.get(jid) {
                return name.clone();
            }
        }
        if !sender.is_empty() && sender != chat_user {
            let names = self.names.read().unwrap();
            for server in ["@s.whatsapp.net", "@lid"] {
                if let Some(name) = names.get(&format!("{sender}{server}")) {
                    return name.clone();
                }
            }
        }
        if !push.trim().is_empty() {
            return push.trim().to_string();
        }
        if !jid.is_empty() {
            let number = number_of(jid);
            if !number.is_empty() {
                return number;
            }
        }
        if !sender.is_empty() && sender != chat_user && !jid.ends_with("@lid") {
            return format!("+{sender}");
        }
        String::new()
    }

    const MSG_COLUMNS: &'static str = "rowid, raw_chat, chat, sender, sender_jid, push_name, from_me, id, ts, text, media, filename, mime, size,
        (thumb IS NOT NULL), width, height, seconds, quoted_id, quoted_sender, quoted_text, status, (edited_ms != 0), revoked, 0, reactions";

    /// Newest-first page, ending before a given position. Indexed, so it doesn't slow with chat size.
    pub fn messages_page(&self, chat: &str, before_ts: Option<i64>, before_rowid: Option<i64>, limit: i64) -> DbResult<Vec<MsgRow>> {
        let conn = self.reader.lock().unwrap();
        let sql = format!(
            "SELECT {} FROM messages WHERE chat = ?1 AND (?2 IS NULL OR ts < ?2 OR (ts = ?2 AND rowid < ?3))
             ORDER BY ts DESC, rowid DESC LIMIT ?4",
            Self::MSG_COLUMNS
        );
        let mut stmt = conn.prepare_cached(&sql).map_err(err)?;
        let rows = stmt
            .query_map(params![chat, before_ts, before_rowid.unwrap_or(i64::MAX), limit], |r| self.map_msg(r))
            .map_err(err)?;
        Ok(rows.flatten().collect())
    }

    /// Oldest-first page after a given position, for new messages and for scrolling back down.
    pub fn messages_after(&self, chat: &str, after_ts: i64, after_rowid: i64, limit: i64) -> DbResult<Vec<MsgRow>> {
        let conn = self.reader.lock().unwrap();
        let sql = format!(
            "SELECT {} FROM messages WHERE chat = ?1 AND (ts > ?2 OR (ts = ?2 AND rowid > ?3))
             ORDER BY ts ASC, rowid ASC LIMIT ?4",
            Self::MSG_COLUMNS
        );
        let mut stmt = conn.prepare_cached(&sql).map_err(err)?;
        let rows = stmt
            .query_map(params![chat, after_ts, after_rowid, limit], |r| self.map_msg(r))
            .map_err(err)?;
        Ok(rows.flatten().collect())
    }

    /// Messages around one message, for jumping to a search result. Returned oldest first.
    pub fn messages_around(&self, rowid: i64, limit: i64) -> DbResult<Vec<MsgRow>> {
        let (chat, ts): (String, i64) = {
            let conn = self.reader.lock().unwrap();
            conn.query_row("SELECT chat, ts FROM messages WHERE rowid = ?", [rowid], |r| Ok((r.get(0)?, r.get(1)?)))
                .map_err(err)?
        };
        let mut older = self.messages_page(&chat, Some(ts), Some(rowid + 1), limit / 2)?;
        older.reverse();
        let newer = self.messages_after(&chat, ts, rowid, limit / 2)?;
        older.extend(newer);
        Ok(older)
    }

    /// One message, by its position, for media commands.
    pub fn message_by_rowid(&self, rowid: i64) -> DbResult<Option<MsgRow>> {
        let conn = self.reader.lock().unwrap();
        let sql = format!("SELECT {} FROM messages WHERE rowid = ?1", Self::MSG_COLUMNS);
        let mut stmt = conn.prepare_cached(&sql).map_err(err)?;
        stmt.query_row([rowid], |r| self.map_msg(r)).optional().map_err(err)
    }

    /// Turns what the user typed into a safe FTS5 query: each word is quoted, so punctuation can't act as
    /// query syntax, and is matched as a prefix.
    /// Stored text only holds the IDs in @mentions, so searching for someone's name also looks for those.
    fn with_mentioned_people(&self, input: &str, query: String) -> String {
        let wanted = input.trim().to_lowercase();
        if wanted.chars().count() < 3 {
            return query;
        }
        let ids: HashSet<String> = self
            .names
            .read()
            .unwrap()
            .iter()
            .filter(|(_, name)| name.to_lowercase().contains(&wanted))
            .filter_map(|(jid, _)| jid.split('@').next().map(str::to_string))
            .filter(|id| !id.is_empty() && looks_numeric(id))
            .take(30)
            .collect();
        if ids.is_empty() {
            return query;
        }
        let mut sorted: Vec<_> = ids.into_iter().collect();
        sorted.sort();
        let terms: Vec<String> = sorted.iter().map(|id| format!("\"{id}\"")).collect();
        format!("({query}) OR {}", terms.join(" OR "))
    }

    pub fn fts_query(input: &str) -> Option<String> {
        let terms: Vec<String> = input
            .split_whitespace()
            .filter_map(|t| {
                let cleaned: String = t.chars().filter(|c| *c != '"').collect();
                if cleaned.is_empty() {
                    None
                } else {
                    Some(format!("\"{cleaned}\"*"))
                }
            })
            .collect();
        if terms.is_empty() {
            None
        } else {
            Some(terms.join(" "))
        }
    }

    pub fn search(&self, input: &str, limit: i64) -> DbResult<Vec<SearchHit>> {
        let Some(query) = Self::fts_query(input) else {
            return Ok(vec![]);
        };
        let query = self.with_mentioned_people(input, query);
        let conn = self.reader.lock().unwrap();
        let mut stmt = conn
            .prepare_cached(
                "SELECT m.rowid, m.chat, COALESCE(c.name, ''), m.ts, m.from_me, m.sender, m.sender_jid, m.push_name,
                    snippet(messages_fts, -1, char(1), char(2), '…', 14), m.media
                 FROM messages_fts
                 JOIN messages m ON m.rowid = messages_fts.rowid
                 LEFT JOIN chats c ON c.jid = m.chat
                 WHERE messages_fts MATCH ?1 AND m.revoked = 0
                 ORDER BY m.ts DESC LIMIT ?2",
            )
            .map_err(err)?;
        let rows = stmt
            .query_map(params![query, limit], |r| {
                let from_me = r.get::<_, i64>(4)? != 0;
                let chat: String = r.get(1)?;
                let sender: String = r.get(5)?;
                let sender_jid: String = r.get(6)?;
                let push: String = r.get(7)?;
                Ok(SearchHit {
                    rowid: r.get(0)?,
                    sender_name: if from_me {
                        "You".into()
                    } else {
                        self.person_name(&sender_jid, &sender, &push, &chat)
                    },
                    chat,
                    chat_name: r.get(2)?,
                    ts: r.get(3)?,
                    from_me,
                    // A highlighted mention has its marker just after the @, so it moves in front of it first.
                    snippet: self.with_mention_names(&r.get::<_, String>(8)?.replace("@\u{1}", "\u{1}@")),
                    media: r.get(9)?,
                })
            })
            .map_err(err)?;
        Ok(rows.flatten().collect())
    }

    /// Unread incoming messages to report, per address, capped to the newest of each.
    pub fn read_batches(&self, chat: &str) -> DbResult<Vec<ReadBatch>> {
        let conn = self.reader.lock().unwrap();
        let mut raws = conn
            .prepare_cached("SELECT jid, last_read_ms FROM raw_chats WHERE chat = ?")
            .map_err(err)?;
        let raw_rows: Vec<(String, i64)> = raws
            .query_map([chat], |r| Ok((r.get(0)?, r.get(1)?)))
            .map_err(err)?
            .flatten()
            .collect();
        let mut out = vec![];
        let shared_read = raw_rows.iter().map(|(_, ms)| *ms).max().unwrap_or(0);
        let mut stmt = conn
            .prepare_cached(
                "SELECT id, sender_jid, ts FROM messages WHERE raw_chat = ?1 AND ts > ?2 AND from_me = 0 AND revoked = 0
                 ORDER BY ts DESC LIMIT 200",
            )
            .map_err(err)?;
        for (raw, _) in raw_rows {
            let rows: Vec<(String, String, i64)> = stmt
                .query_map(params![raw, shared_read], |r| Ok((r.get(0)?, r.get(1)?, r.get(2)?)))
                .map_err(err)?
                .flatten()
                .collect();
            if rows.is_empty() {
                continue;
            }
            let up_to_ms = rows.iter().map(|r| r.2).max().unwrap_or(0);
            out.push(ReadBatch {
                chat: raw,
                up_to_ms,
                messages: rows
                    .into_iter()
                    .map(|(id, sender_jid, _)| ReadItem { id, sender_jid })
                    .collect(),
            });
        }
        Ok(out)
    }

    /// Clears the unread badge at once, without waiting for the home PC to confirm.
    pub fn mark_read_local(&self, chat: &str, up_to_ms: i64) -> DbResult<()> {
        let mut conn = self.writer.lock().unwrap();
        let tx = conn.transaction().map_err(err)?;
        tx.execute(
            "UPDATE raw_chats SET last_read_ms = MAX(last_read_ms, ?2) WHERE chat = ?1",
            params![chat, up_to_ms],
        )
        .map_err(err)?;
        self.refresh_chat(&tx, chat).map_err(err)?;
        tx.commit().map_err(err)
    }

    pub fn media_info(&self, rowid: i64) -> DbResult<Option<MediaInfo>> {
        let conn = self.reader.lock().unwrap();
        conn.query_row(
            "SELECT raw_chat, id, media, filename, mime, size, thumb FROM messages WHERE rowid = ?",
            [rowid],
            |r| {
                Ok(MediaInfo {
                    raw_chat: r.get(0)?,
                    id: r.get(1)?,
                    media: r.get(2)?,
                    filename: r.get(3)?,
                    mime: r.get(4)?,
                    size: r.get(5)?,
                    thumb: r.get(6)?,
                })
            },
        )
        .optional()
        .map_err(err)
    }

    pub fn contact_list(&self) -> DbResult<Vec<ContactRow>> {
        let conn = self.reader.lock().unwrap();
        let mut stmt = conn.prepare_cached("SELECT jid FROM contacts").map_err(err)?;
        let jids: Vec<String> = stmt
            .query_map([], |r| r.get(0))
            .map_err(err)?
            .flatten()
            .collect();
        let chats: HashSet<String> = {
            let mut s = conn.prepare_cached("SELECT jid FROM chats").map_err(err)?;
            let rows: Vec<String> = s.query_map([], |r| r.get(0)).map_err(err)?.flatten().collect();
            rows.into_iter().collect()
        };
        let names = self.names.read().unwrap();
        let mut seen = HashSet::new();
        let mut out = vec![];
        for jid in jids {
            if jid.ends_with("@broadcast") || is_group(&jid) {
                continue;
            }
            let canon = self.canon(&jid);
            if !seen.insert(canon.clone()) {
                continue;
            }
            let Some(name) = names.get(&canon).or_else(|| names.get(&jid)).cloned() else {
                continue;
            };
            out.push(ContactRow {
                has_chat: chats.contains(&canon),
                number: number_of(&canon),
                jid: canon,
                name,
            });
        }
        out.sort_by_key(|c| c.name.to_lowercase());
        Ok(out)
    }

    /// The material for Claude's sorting. The snippets, if included, are the few newest messages from
    /// other people, since the user's own replies say less about who someone is.
    pub fn sort_items(&self, snippets: bool, skip: &HashSet<String>, include_contacts: bool) -> DbResult<Vec<SortItem>> {
        let chats = self.chat_list()?;
        let conn = self.reader.lock().unwrap();
        let mut out = vec![];
        let mut seen = HashSet::new();
        let mut stmt = conn
            .prepare_cached(
                "SELECT text FROM messages WHERE chat = ?1 AND text != '' AND revoked = 0
                 ORDER BY ts DESC LIMIT 5",
            )
            .map_err(err)?;
        for chat in chats {
            seen.insert(chat.jid.clone());
            if skip.contains(&chat.jid) {
                continue;
            }
            let lines: Vec<String> = if snippets {
                stmt.query_map([&chat.jid], |r| r.get(0)).map_err(err)?.flatten().collect()
            } else {
                vec![]
            };
            out.push(SortItem {
                jid: chat.jid,
                name: chat.name,
                is_group: chat.is_group,
                snippets: lines,
            });
        }
        drop(stmt);
        drop(conn);
        if include_contacts {
            for contact in self.contact_list()? {
                if seen.contains(&contact.jid) || skip.contains(&contact.jid) {
                    continue;
                }
                out.push(SortItem {
                    jid: contact.jid,
                    name: contact.name,
                    is_group: false,
                    snippets: vec![],
                });
            }
        }
        Ok(out)
    }

    pub fn stats(&self) -> DbResult<(i64, i64, i64)> {
        let conn = self.reader.lock().unwrap();
        let messages = conn.query_row("SELECT COUNT(*) FROM messages", [], |r| r.get(0)).map_err(err)?;
        let chats = conn.query_row("SELECT COUNT(*) FROM chats", [], |r| r.get(0)).map_err(err)?;
        let size = conn
            .query_row(
                "SELECT page_count * page_size FROM pragma_page_count(), pragma_page_size()",
                [],
                |r| r.get(0),
            )
            .map_err(err)?;
        Ok((messages, chats, size))
    }

    /// Erases everything cached here. Nothing on the home PC or in WhatsApp changes.
    pub fn wipe(&self) -> DbResult<()> {
        {
            let conn = self.writer.lock().unwrap();
            conn.execute_batch(
                "DELETE FROM messages; DELETE FROM chats; DELETE FROM raw_chats; DELETE FROM contacts; DELETE FROM aliases;
                 DELETE FROM meta WHERE key != 'first_run_done' AND key NOT LIKE 'setting_%';
                 INSERT INTO messages_fts (messages_fts) VALUES ('rebuild');
                 VACUUM;
                 PRAGMA wal_checkpoint(TRUNCATE);",
            )
            .map_err(err)?;
        }
        self.load_maps()
    }
}

#[allow(dead_code)]
pub fn encode_b64(bytes: &[u8]) -> String {
    base64::engine::general_purpose::STANDARD.encode(bytes)
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::time::Instant;

    const PN: &str = "15550000001@s.whatsapp.net";
    const LID: &str = "99999@lid";
    const GROUP: &str = "120363000000000001@g.us";

    fn cache() -> (Cache, tempfile::TempDir) {
        let dir = tempfile::tempdir().unwrap();
        (Cache::open(&dir.path().join("c.db")).unwrap(), dir)
    }

    fn msg(id: &str, chat: &str, text: &str, ts: i64) -> WireMsg {
        WireMsg {
            id: id.into(),
            chat: chat.into(),
            text: text.into(),
            ts,
            sender: chat.split('@').next().unwrap().into(),
            ..Default::default()
        }
    }

    #[test]
    fn stores_and_pages_newest_first() {
        let (c, _d) = cache();
        let msgs: Vec<_> = (0..120).map(|i| msg(&format!("m{i}"), PN, &format!("text {i}"), 1000 + i)).collect();
        c.apply_batch(&msgs, &[], Some(120)).unwrap();
        assert_eq!(c.cursor(), 120);
        let first = c.messages_page(PN, None, None, 50).unwrap();
        assert_eq!(first.len(), 50);
        assert_eq!(first[0].id, "m119");
        let last = first.last().unwrap();
        let second = c.messages_page(PN, Some(last.ts), Some(last.rowid), 50).unwrap();
        assert_eq!(second[0].id, "m69");
        let tail = second.last().unwrap();
        let third = c.messages_page(PN, Some(tail.ts), Some(tail.rowid), 50).unwrap();
        assert_eq!(third.len(), 20);
        assert_eq!(third.last().unwrap().id, "m0");
    }

    #[test]
    fn same_timestamp_messages_are_not_lost_between_pages() {
        let (c, _d) = cache();
        let msgs: Vec<_> = (0..10).map(|i| msg(&format!("m{i}"), PN, "x", 5000)).collect();
        c.apply_batch(&msgs, &[], None).unwrap();
        let mut seen = HashSet::new();
        let mut before: Option<(i64, i64)> = None;
        loop {
            let page = c.messages_page(PN, before.map(|b| b.0), before.map(|b| b.1), 3).unwrap();
            if page.is_empty() {
                break;
            }
            for m in &page {
                seen.insert(m.id.clone());
            }
            let last = page.last().unwrap();
            before = Some((last.ts, last.rowid));
        }
        assert_eq!(seen.len(), 10);
    }

    #[test]
    fn redelivery_updates_without_duplicating() {
        let (c, _d) = cache();
        c.apply_batch(&[msg("m1", PN, "first", 1)], &[], None).unwrap();
        let mut again = msg("m1", PN, "edited", 1);
        again.status = "read".into();
        c.apply_batch(&[again], &[], None).unwrap();
        let page = c.messages_page(PN, None, None, 10).unwrap();
        assert_eq!(page.len(), 1);
        assert_eq!(page[0].text, "edited");
        assert_eq!(page[0].status, "read");
        assert!(c.search("first", 10).unwrap().is_empty(), "old text must leave the index");
        assert_eq!(c.search("edited", 10).unwrap().len(), 1);
    }

    #[test]
    fn thumb_survives_an_update_without_one() {
        let (c, _d) = cache();
        let mut first = msg("m1", PN, "x", 1);
        first.thumb = Some(vec![1, 2, 3]);
        c.apply_batch(&[first], &[], None).unwrap();
        c.apply_batch(&[msg("m1", PN, "x", 1)], &[], None).unwrap();
        let row = c.messages_page(PN, None, None, 1).unwrap().remove(0);
        assert_eq!(c.media_info(row.rowid).unwrap().unwrap().thumb, Some(vec![1, 2, 3]));
    }

    #[test]
    fn search_finds_prefixes_across_chats_with_highlight() {
        let (c, _d) = cache();
        c.apply_batch(
            &[msg("a", PN, "Meeting at the substation tomorrow", 10), msg("b", GROUP, "the Substation relay failed", 20), msg("c", PN, "nothing relevant", 30)],
            &[],
            None,
        )
        .unwrap();
        let hits = c.search("subst", 10).unwrap();
        assert_eq!(hits.len(), 2);
        assert_eq!(hits[0].chat, GROUP, "newest first");
        assert!(hits[0].snippet.contains('\u{1}') && hits[0].snippet.contains('\u{2}'));
    }

    #[test]
    fn search_ignores_diacritics_and_needs_all_words() {
        let (c, _d) = cache();
        c.apply_batch(&[msg("a", PN, "Café opening soon", 10), msg("b", PN, "opening only", 11)], &[], None).unwrap();
        assert_eq!(c.search("cafe", 10).unwrap().len(), 1);
        assert_eq!(c.search("cafe opening", 10).unwrap().len(), 1);
        assert_eq!(c.search("opening", 10).unwrap().len(), 2);
    }

    #[test]
    fn search_input_cannot_break_out_of_the_query() {
        let (c, _d) = cache();
        c.apply_batch(&[msg("a", PN, "hello world", 10)], &[], None).unwrap();
        for evil in ["\"", "hello\" OR \"x", "NEAR(", "text:hello", "*", "^", "-hello", "a'; DROP TABLE messages; --", "(((", ""] {
            assert!(c.search(evil, 10).is_ok(), "{evil} broke search");
        }
        assert_eq!(c.search("hello", 10).unwrap().len(), 1, "table intact");
    }

    #[test]
    fn deleted_messages_leave_results_and_show_as_deleted() {
        let (c, _d) = cache();
        c.apply_batch(&[msg("a", PN, "secret words", 10)], &[], None).unwrap();
        let mut gone = msg("a", PN, "", 10);
        gone.revoked = true;
        c.apply_batch(&[gone], &[], None).unwrap();
        assert!(c.search("secret", 10).unwrap().is_empty());
        assert_eq!(c.chat_list().unwrap()[0].preview, "This message was deleted");
    }

    fn contact(jid: &str, full: &str, push: &str) -> WireContact {
        WireContact { jid: jid.into(), full_name: full.into(), push_name: push.into(), ..Default::default() }
    }

    /// A made-up person with a saved name at their number, and only a profile name at their hidden ID.
    fn with_saved_person() -> (Cache, tempfile::TempDir) {
        let (c, d) = cache();
        c.set_contacts(
            &[contact("15551110000@s.whatsapp.net", "Dana Saved", ""), contact("777777777@lid", "", "Dana")],
            &[("777777777@lid".to_string(), "15551110000@s.whatsapp.net".to_string())],
        )
        .unwrap();
        (c, d)
    }

    #[test]
    fn a_saved_name_beats_a_profile_name_across_addresses() {
        let (c, _d) = with_saved_person();
        assert_eq!(c.person_name("777777777@lid", "777777777", "Dana", GROUP), "Dana Saved");
    }

    #[test]
    fn a_message_deleted_for_me_leaves_this_pc() {
        let (c, _d) = cache();
        let mut newest = msg("b", GROUP, "findme newest", 20);
        newest.from_me = false;
        c.apply_batch(&[msg("a", GROUP, "findme older", 10), newest], &[], None).unwrap();
        assert_eq!(c.search("findme", 10).unwrap().len(), 2);
        let hide = WireMsg { chat: GROUP.into(), id: "b".into(), hidden: true, ..Default::default() };
        let touched = c.apply_batch(&[hide.clone()], &[], None).unwrap();
        assert!(touched.contains(&GROUP.to_string()), "the chat row is rebuilt");
        let page = c.messages_page(GROUP, None, None, 10).unwrap();
        assert_eq!(page.iter().map(|m| m.id.as_str()).collect::<Vec<_>>(), vec!["a"]);
        assert_eq!(c.search("findme", 10).unwrap().len(), 1, "gone from search too");
        let row = c.chat_list().unwrap().into_iter().find(|r| r.jid == GROUP).unwrap();
        assert_eq!(row.preview, "findme older", "the preview moves to the message before it");
        c.apply_batch(&[hide], &[], None).unwrap();
        assert_eq!(c.messages_page(GROUP, None, None, 10).unwrap().len(), 1, "hiding again changes nothing");
    }

    #[test]
    fn mentions_show_the_best_name() {
        let (c, _d) = with_saved_person();
        c.set_contacts(
            &[contact("15551110000@s.whatsapp.net", "Dana Saved", ""), contact("777777777@lid", "", "Dana"), contact("888888888@lid", "", "Only Profile")],
            &[
                ("777777777@lid".to_string(), "15551110000@s.whatsapp.net".to_string()),
                ("666666666@lid".to_string(), "15552220000@s.whatsapp.net".to_string()),
            ],
        )
        .unwrap();
        assert_eq!(c.mention_name("777777777").as_deref(), Some("Dana Saved"), "hidden ID to saved name");
        assert_eq!(c.mention_name("15551110000").as_deref(), Some("Dana Saved"), "by number");
        assert_eq!(c.mention_name("888888888").as_deref(), Some("Only Profile"));
        assert_eq!(c.mention_name("666666666").as_deref(), Some("+15552220000"), "the number, with no name");
        assert_eq!(c.mention_name("555555555"), None);
        assert_eq!(
            c.with_mention_names("Hi @777777777, @555555555 and @15551110000. mail a@777777777 @77777 x@ @7777777770000000000000"),
            "Hi @Dana Saved, @555555555 and @Dana Saved. mail a@777777777 @77777 x@ @7777777770000000000000"
        );
        c.set_own(vec!["11111111@lid".into()]).unwrap();
        assert_eq!(c.with_mention_names("@11111111"), "@You");
    }

    #[test]
    fn stored_messages_keep_the_raw_text_and_the_row_carries_the_names() {
        let (c, _d) = with_saved_person();
        c.apply_batch(&[msg("a", GROUP, "Ask @777777777 about it", 10)], &[], None).unwrap();
        let row = c.chat_list().unwrap().into_iter().find(|r| r.jid == GROUP).unwrap();
        assert_eq!(row.preview, "Ask @Dana Saved about it");
        let page = c.messages_page(GROUP, None, None, 10).unwrap();
        assert_eq!(page[0].text, "Ask @777777777 about it");
        assert_eq!(page[0].mentions.get("777777777").map(String::as_str), Some("Dana Saved"));
    }

    #[test]
    fn searching_a_name_finds_their_mentions() {
        let (c, _d) = with_saved_person();
        c.apply_batch(&[msg("a", GROUP, "Ask @777777777 about it", 10), msg("b", GROUP, "Nothing here", 11)], &[], None).unwrap();
        let hits = c.search("dana saved", 10).unwrap();
        assert_eq!(hits.len(), 1, "{hits:?}");
        assert!(hits[0].snippet.contains("@Dana Saved"), "{:?}", hits[0].snippet);
        assert!(!hits[0].snippet.contains("777777777"), "{:?}", hits[0].snippet);
    }

    #[test]
    fn a_person_is_one_address_however_their_messages_arrived() {
        let (c, _d) = cache();
        c.set_contacts(&[contact("888888888@lid", "", "Only Profile")], &[("777777777@lid".to_string(), "15551110000@s.whatsapp.net".to_string())]).unwrap();
        // By hidden ID and by number, in a group.
        assert_eq!(c.person_of(GROUP, false, "777777777", "777777777@lid"), "15551110000@s.whatsapp.net");
        assert_eq!(c.person_of(GROUP, false, "15551110000", "15551110000@s.whatsapp.net"), "15551110000@s.whatsapp.net");
        // Only the old sender column to go on: a number, a mapped hidden ID, and an unmapped one.
        assert_eq!(c.person_of(GROUP, false, "15552220000", ""), "15552220000@s.whatsapp.net");
        assert_eq!(c.person_of(GROUP, false, "777777777", ""), "15551110000@s.whatsapp.net");
        assert_eq!(c.person_of(GROUP, false, "888888888", ""), "888888888@lid");
        // The group's own ID says nothing about who wrote it, and the user isn't one of the colours.
        assert_eq!(c.person_of(GROUP, false, GROUP.split('@').next().unwrap(), ""), "");
        assert_eq!(c.person_of(GROUP, false, "", ""), "");
        assert_eq!(c.person_of(GROUP, true, "15559990000", "15559990000@s.whatsapp.net"), "");
        // In a one-to-one chat, the chat says who, even with no sender on the row.
        assert_eq!(c.person_of(PN, false, "", ""), PN);
    }

    #[test]
    fn everyone_who_wrote_in_a_chat_is_listed_once() {
        let (c, _d) = cache();
        c.set_contacts(&[], &[("777777777@lid".to_string(), "15551110000@s.whatsapp.net".to_string())]).unwrap();
        c.set_own(vec!["15559990000@s.whatsapp.net".into()]).unwrap();
        let from = |id: &str, sender: &str, jid: &str, ts: i64| {
            let mut m = msg(id, GROUP, "made up", ts);
            m.sender = sender.into();
            m.sender_jid = jid.into();
            m
        };
        let mut mine = from("e", "15559990000", "15559990000@s.whatsapp.net", 50);
        mine.from_me = true;
        c.apply_batch(
            &[
                from("a", "777777777", "777777777@lid", 10),
                from("b", "15551110000", "15551110000@s.whatsapp.net", 20),
                from("c", "15553330000", "15553330000@s.whatsapp.net", 30),
                from("d", GROUP.split('@').next().unwrap(), "", 40),
                mine,
            ],
            &[],
            None,
        )
        .unwrap();
        assert_eq!(c.chat_senders(GROUP).unwrap(), vec!["15551110000@s.whatsapp.net", "15553330000@s.whatsapp.net"]);
        let rows = c.messages_page(GROUP, None, None, 10).unwrap();
        let people: Vec<&str> = rows.iter().map(|r| r.person.as_str()).collect();
        assert_eq!(people, vec!["", "", "15553330000@s.whatsapp.net", "15551110000@s.whatsapp.net", "15551110000@s.whatsapp.net"]);
    }

    #[test]
    fn a_member_carries_the_address_their_messages_do() {
        let (c, _d) = cache();
        c.set_contacts(&[], &[("777777777@lid".to_string(), "15551110000@s.whatsapp.net".to_string())]).unwrap();
        let member = |jid: &str, phone: &str, lid: &str| WireMember { jid: jid.into(), phone: phone.into(), lid: lid.into(), admin: false };
        assert_eq!(c.member_row(&member("777777777@lid", "", "")).person, "15551110000@s.whatsapp.net");
        let both = c.member_row(&member("555@lid", "15554440000@s.whatsapp.net", "555@lid"));
        assert_eq!((both.person.as_str(), both.lid.as_str()), ("15554440000@s.whatsapp.net", "555@lid"));
        assert_eq!(c.member_row(&member("666@lid", "", "")).person, "666@lid");
    }

    #[test]
    fn group_members_are_named_like_senders() {
        let (c, _d) = with_saved_person();
        c.set_own(vec!["15559990000@s.whatsapp.net".into(), "11111@lid".into()]).unwrap();
        let member = |jid: &str, phone: &str, lid: &str| WireMember { jid: jid.into(), phone: phone.into(), lid: lid.into(), admin: false };
        assert_eq!(c.member_row(&member("777777777@lid", "", "")).name, "Dana Saved");
        assert_eq!(c.member_row(&member("777777777@lid", "15551110000@s.whatsapp.net", "777777777@lid")).name, "Dana Saved");
        assert_eq!(c.member_row(&member("15553330000@s.whatsapp.net", "", "")).name, "+15553330000");
        assert_eq!(c.member_row(&member("444444444@lid", "", "")).name, "Unknown member");
        assert!(c.member_row(&member("11111@lid", "", "")).you);
        assert!(!c.member_row(&member("777777777@lid", "", "")).you);
    }

    #[test]
    fn a_chat_is_known_only_once_the_list_would_show_it() {
        let (c, _d) = cache();
        assert!(!c.has_chat(PN));
        c.apply_batch(&[msg("a", PN, "hello", 10)], &[], None).unwrap();
        assert!(c.has_chat(PN));
        assert!(!c.has_chat(LID));
    }

    #[test]
    fn lid_and_phone_chats_merge_when_alias_arrives() {
        let (c, _d) = cache();
        c.apply_batch(&[msg("a", PN, "by number", 10), msg("b", LID, "by lid", 20)], &[], None).unwrap();
        assert_eq!(c.chat_list().unwrap().len(), 2, "not merged yet");
        c.set_contacts(
            &[WireContact { jid: PN.into(), full_name: "Sample Person".into(), ..Default::default() }],
            &[(LID.to_string(), PN.to_string())],
        )
        .unwrap();
        let list = c.chat_list().unwrap();
        assert_eq!(list.len(), 1, "one person, one chat: {list:?}");
        assert_eq!(list[0].jid, PN);
        assert_eq!(list[0].name, "Sample Person");
        assert_eq!(list[0].preview, "by lid");
        let page = c.messages_page(PN, None, None, 10).unwrap();
        assert_eq!(page.len(), 2);
        assert_eq!(page[0].raw_chat, LID, "keeps its own address for media and receipts");
        c.apply_batch(&[msg("c", LID, "later", 30)], &[], None).unwrap();
        assert_eq!(c.chat_list().unwrap().len(), 1, "new LID messages go to the same chat");
    }

    #[test]
    fn contact_named_under_lid_names_the_phone_chat() {
        let (c, _d) = cache();
        c.apply_batch(&[msg("a", PN, "x", 10)], &[], None).unwrap();
        c.set_contacts(
            &[WireContact { jid: LID.into(), full_name: "Only On Lid".into(), ..Default::default() }],
            &[(LID.to_string(), PN.to_string())],
        )
        .unwrap();
        assert_eq!(c.chat_list().unwrap()[0].name, "Only On Lid");
    }

    #[test]
    fn unknown_people_fall_back_to_their_number_and_groups_to_their_name() {
        let (c, _d) = cache();
        c.apply_batch(
            &[msg("a", PN, "x", 10), msg("g", GROUP, "y", 20)],
            &[
                WireChat { jid: PN.into(), name: "15550000001".into(), last_ms: 10, last_read_ms: 0 },
                WireChat { jid: GROUP.into(), name: "Bingo Night".into(), last_ms: 20, last_read_ms: 0 },
            ],
            None,
        )
        .unwrap();
        let list = c.chat_list().unwrap();
        assert_eq!(list.iter().find(|x| x.jid == PN).unwrap().name, "+15550000001");
        let group = list.iter().find(|x| x.jid == GROUP).unwrap();
        assert_eq!(group.name, "Bingo Night");
        assert!(group.is_group);
    }

    #[test]
    fn old_group_rows_with_the_group_as_sender_show_no_name() {
        let (c, _d) = cache();
        let mut old = msg("a", GROUP, "x", 10);
        old.sender = GROUP.split('@').next().unwrap().into();
        c.apply_batch(&[old], &[], None).unwrap();
        assert_eq!(c.messages_page(GROUP, None, None, 1).unwrap()[0].sender_name, "");
    }

    #[test]
    fn group_sender_uses_contact_then_push_name() {
        let (c, _d) = cache();
        let mut named = msg("a", GROUP, "x", 10);
        named.sender = "15550000001".into();
        named.sender_jid = PN.into();
        let mut pushed = msg("b", GROUP, "y", 11);
        pushed.sender = "42".into();
        pushed.sender_jid = "42@lid".into();
        pushed.push_name = "Pushy".into();
        c.apply_batch(&[named, pushed], &[], None).unwrap();
        c.set_contacts(&[WireContact { jid: PN.into(), full_name: "Sample Person".into(), ..Default::default() }], &[]).unwrap();
        let page = c.messages_page(GROUP, None, None, 5).unwrap();
        assert_eq!(page.iter().find(|m| m.id == "a").unwrap().sender_name, "Sample Person");
        assert_eq!(page.iter().find(|m| m.id == "b").unwrap().sender_name, "Pushy");
    }

    #[test]
    fn quoting_my_own_message_says_you() {
        let (c, _d) = cache();
        let mut m = msg("a", PN, "reply", 10);
        m.quoted_id = "q".into();
        m.quoted_sender = "15559990000@s.whatsapp.net".into();
        c.apply_batch(&[m], &[], None).unwrap();
        assert_eq!(c.messages_page(PN, None, None, 1).unwrap()[0].quoted_name, "+15559990000");
        c.set_own(vec!["15559990000@s.whatsapp.net".into(), "11111@lid".into()]).unwrap();
        assert_eq!(c.messages_page(PN, None, None, 1).unwrap()[0].quoted_name, "You");
    }

    #[test]
    fn unread_counts_follow_the_read_marker() {
        let (c, _d) = cache();
        let mut mine = msg("m", PN, "mine", 40);
        mine.from_me = true;
        c.apply_batch(
            &[msg("a", PN, "1", 10), msg("b", PN, "2", 20), msg("c", PN, "3", 30), mine],
            &[WireChat { jid: PN.into(), name: "".into(), last_ms: 40, last_read_ms: 15 }],
            None,
        )
        .unwrap();
        assert_eq!(c.chat_list().unwrap()[0].unread, 2, "my own message never counts");
        let batches = c.read_batches(PN).unwrap();
        assert_eq!(batches.len(), 1);
        assert_eq!(batches[0].up_to_ms, 30);
        assert_eq!(batches[0].messages.len(), 2);
        c.mark_read_local(PN, 30).unwrap();
        assert_eq!(c.chat_list().unwrap()[0].unread, 0);
        assert!(c.read_batches(PN).unwrap().is_empty());
        assert_eq!(c.total_unread(), 0);
        c.apply_batch(&[], &[WireChat { jid: PN.into(), name: "".into(), last_ms: 40, last_read_ms: 5 }], None).unwrap();
        assert_eq!(c.chat_list().unwrap()[0].unread, 0, "an older marker can't bring back unread");
    }

    #[test]
    fn read_on_another_device_clears_unread_here() {
        let (c, _d) = cache();
        c.apply_batch(&[msg("a", PN, "1", 10)], &[], None).unwrap();
        assert_eq!(c.chat_list().unwrap()[0].unread, 1);
        c.apply_batch(&[], &[WireChat { jid: PN.into(), last_read_ms: 10, ..Default::default() }], None).unwrap();
        assert_eq!(c.chat_list().unwrap()[0].unread, 0);
    }

    #[test]
    fn read_batches_split_merged_chat_by_address() {
        let (c, _d) = cache();
        c.apply_batch(&[msg("a", PN, "1", 10), msg("b", LID, "2", 20)], &[], None).unwrap();
        c.set_contacts(&[], &[(LID.to_string(), PN.to_string())]).unwrap();
        let batches = c.read_batches(PN).unwrap();
        assert_eq!(batches.len(), 2);
        let chats: HashSet<_> = batches.iter().map(|b| b.chat.clone()).collect();
        assert!(chats.contains(PN) && chats.contains(LID));
    }

    #[test]
    fn around_centers_on_the_message() {
        let (c, _d) = cache();
        let msgs: Vec<_> = (0..100).map(|i| msg(&format!("m{i}"), PN, "x", 1000 + i)).collect();
        c.apply_batch(&msgs, &[], None).unwrap();
        let target = c.search("x", 100).unwrap().into_iter().find(|h| h.ts == 1050).unwrap();
        let around = c.messages_around(target.rowid, 40).unwrap();
        assert!(around.iter().any(|m| m.rowid == target.rowid));
        assert!(around.first().unwrap().ts < 1050 && around.last().unwrap().ts > 1050);
        assert!(around.windows(2).all(|w| w[0].ts <= w[1].ts), "oldest first");
    }

    #[test]
    fn sort_items_respect_skip_and_snippet_choice() {
        let (c, _d) = cache();
        c.apply_batch(&[msg("a", PN, "hello", 10), msg("g", GROUP, "group text", 20)], &[], None).unwrap();
        c.set_contacts(&[WireContact { jid: "7@s.whatsapp.net".into(), full_name: "No Chat Yet".into(), ..Default::default() }], &[]).unwrap();
        let mut skip = HashSet::new();
        let names_only = c.sort_items(false, &skip, false).unwrap();
        assert_eq!(names_only.len(), 2);
        assert!(names_only.iter().all(|i| i.snippets.is_empty()), "no message text unless the user chose it");
        let with_text = c.sort_items(true, &skip, false).unwrap();
        assert!(with_text.iter().any(|i| i.snippets == vec!["hello".to_string()]));
        assert_eq!(c.sort_items(false, &skip, true).unwrap().len(), 3);
        skip.insert(PN.to_string());
        assert!(!c.sort_items(false, &skip, false).unwrap().iter().any(|i| i.jid == PN));
    }

    #[test]
    fn wipe_clears_all_message_data() {
        let (c, d) = cache();
        c.apply_batch(&[msg("a", PN, "private words", 10)], &[], Some(5)).unwrap();
        // The saved copy of contact cards goes too.
        c.meta_set("cards", r#"{"cards":[{"display":"Made-up Card","notes":"card secret"}]}"#).unwrap();
        c.wipe().unwrap();
        assert_eq!(c.cursor(), 0, "a later sign-in must fetch everything again");
        assert!(c.chat_list().unwrap().is_empty());
        assert!(c.search("private", 10).unwrap().is_empty());
        assert!(c.meta_get("cards").unwrap().is_none(), "contact cards must be erased");
        let raw = std::fs::read(d.path().join("c.db")).unwrap();
        assert!(!raw.windows(13).any(|w| w == b"private words"), "text must be gone from the file");
        assert!(!raw.windows(11).any(|w| w == b"card secret"), "card details must be gone from the file");
    }

    #[test]
    fn cursor_only_moves_with_a_committed_batch() {
        let (c, _d) = cache();
        c.apply_batch(&[msg("a", PN, "x", 1)], &[], Some(10)).unwrap();
        c.apply_batch(&[msg("b", PN, "y", 2)], &[], None).unwrap();
        assert_eq!(c.cursor(), 10, "a live delta without a cursor must not advance it");
    }

    #[test]
    fn stays_fast_with_a_big_history() {
        let (c, _d) = cache();
        let chats: Vec<String> = (0..300).map(|i| format!("1555{i:07}@s.whatsapp.net")).collect();
        let words = ["relay", "transformer", "substation", "breaker", "protection", "scheme", "invoice", "dinner"];
        for batch in 0..20 {
            let msgs: Vec<_> = (0..2000i64)
                .map(|i| {
                    let n = batch as i64 * 2000 + i;
                    msg(
                        &format!("m{n}"),
                        &chats[(n % 300) as usize],
                        &format!("{} {} message number {n}", words[(n % 8) as usize], words[((n / 3) % 8) as usize]),
                        1_700_000_000_000 + n,
                    )
                })
                .collect();
            c.apply_batch(&msgs, &[], None).unwrap();
        }
        let t = Instant::now();
        let hits = c.search("transformer relay", 50).unwrap();
        let search_ms = t.elapsed().as_millis();
        assert!(!hits.is_empty());
        let t = Instant::now();
        let list = c.chat_list().unwrap();
        let list_ms = t.elapsed().as_millis();
        assert_eq!(list.len(), 300);
        let t = Instant::now();
        let page = c.messages_page(&chats[0], None, None, 60).unwrap();
        let page_ms = t.elapsed().as_millis();
        assert!(!page.is_empty());
        eprintln!("40k messages: search {search_ms} ms, chat list {list_ms} ms, page {page_ms} ms");
        assert!(search_ms < 150 && list_ms < 50 && page_ms < 50, "search {search_ms} list {list_ms} page {page_ms}");
    }
}
