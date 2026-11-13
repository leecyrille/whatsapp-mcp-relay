//! Media: a size-limited local cache, a custom protocol that serves cached files to the UI, and
//! resumable transfers in both directions.
//!
//! Files come from other people and are untrusted. The UI only ever shows them in image, video and
//! audio elements, and opening one in another program is limited to file types that can't run code.

use crate::net::NetError;
use crate::AppState;
use base64::Engine;
use serde_json::{json, Value};
use std::collections::HashMap;
use std::path::{Path, PathBuf};
use std::sync::Arc;
use std::time::{Duration, Instant};
use tauri::http::{header, Request, Response, StatusCode};
use tauri::{AppHandle, Emitter, Manager};
use tokio::io::{AsyncReadExt, AsyncSeekExt, AsyncWriteExt};

const CHUNK: usize = 32 << 20; // Under Cloudflare's 100 MB limit per request.
const MAX_UPLOAD: u64 = 2 << 30;
const RANGE_LIMIT: u64 = 4 << 20;
const WHOLE_FILE_LIMIT: u64 = 64 << 20;
const DEFAULT_CACHE_BYTES: u64 = 5 << 30;

/// File types safe to hand to the default program. Everything else can only be saved, so a message
/// can't get a script or program run with one click.
const OPENABLE: &[&str] = &[
    "pdf", "txt", "csv", "jpg", "jpeg", "png", "gif", "webp", "heic", "bmp", "mp4", "mov", "mkv", "avi", "webm", "mp3",
    "m4a", "aac", "ogg", "opus", "wav", "flac", "docx", "xlsx", "pptx",
];

/// Marks a file as having come from the internet. That turns on Office's Protected View and
/// SmartScreen, which is what those programs rely on to treat a stranger's file with suspicion.
pub fn mark_from_internet(path: &Path) -> std::io::Result<()> {
    let mut stream = path.as_os_str().to_os_string();
    stream.push(":Zone.Identifier");
    std::fs::write(stream, "[ZoneTransfer]\r\nZoneId=3\r\n")
}

pub fn can_open(filename: &str) -> bool {
    Path::new(filename)
        .extension()
        .and_then(|e| e.to_str())
        .map(|e| OPENABLE.contains(&e.to_ascii_lowercase().as_str()))
        .unwrap_or(false)
}

/// Right-to-left override characters can make "report<RLO>gpj.exe" read as "reportexe.jpg".
fn is_bidi_control(c: char) -> bool {
    matches!(c, '\u{200e}' | '\u{200f}' | '\u{202a}'..='\u{202e}' | '\u{2066}'..='\u{2069}')
}

/// Strips anything path-like or awkward from a name a stranger chose.
pub fn safe_name(name: &str) -> String {
    let base = name.replace('\\', "/");
    let base = base.rsplit('/').next().unwrap_or("").trim().trim_matches('.');
    let cleaned: String = base
        .chars()
        .map(|c| if c.is_control() || is_bidi_control(c) || r#"<>:"|?*"#.contains(c) { '_' } else { c })
        .take(180)
        .collect();
    if cleaned.is_empty() {
        "file".into()
    } else {
        cleaned
    }
}

pub fn file_path(st: &AppState, rowid: i64) -> PathBuf {
    st.media_dir.join(format!("{rowid}.bin"))
}

fn query_escape(s: &str) -> String {
    url::form_urlencoded::byte_serialize(s.as_bytes()).collect()
}

/// Makes sure a message's file is cached, downloading it if not. Concurrent requests for one file
/// share one download. A broken connection resumes from where it stopped.
pub async fn ensure_cached(app: &AppHandle, st: &Arc<AppState>, rowid: i64) -> Result<PathBuf, String> {
    let path = file_path(st, rowid);
    if tokio::fs::metadata(&path).await.is_ok() {
        touch(&path);
        return Ok(path);
    }
    let gate = {
        let mut gates = st.download_gates.lock().unwrap();
        gates.entry(rowid).or_default().clone()
    };
    let _held = gate.lock().await;
    if tokio::fs::metadata(&path).await.is_ok() {
        return Ok(path);
    }
    let result = download(app, st, rowid, &path).await;
    st.download_gates.lock().unwrap().remove(&rowid);
    match &result {
        Ok(_) => {
            let _ = app.emit("media-state", json!({"rowid": rowid, "state": "ready"}));
            let (dir, limit) = (st.media_dir.clone(), cache_limit(st));
            tauri::async_runtime::spawn_blocking(move || prune(&dir, limit));
        }
        Err(e) => {
            let _ = app.emit("media-state", json!({"rowid": rowid, "state": "failed", "error": e}));
        }
    }
    result
}

async fn download(app: &AppHandle, st: &Arc<AppState>, rowid: i64, path: &Path) -> Result<PathBuf, String> {
    let info = st.cache.media_info(rowid)?.ok_or("That message is gone.")?;
    let api = format!("/api/media?chat={}&id={}", query_escape(&info.raw_chat), query_escape(&info.id));
    let part = path.with_extension("part");
    let mut attempt = 0;
    let started = Instant::now();
    loop {
        let have = tokio::fs::metadata(&part).await.map(|m| m.len()).unwrap_or(0);
        let mut req = st
            .relay
            .long_request(reqwest::Method::GET, &api)
            .map_err(|e| e.message())?;
        if have > 0 {
            req = req.header(reqwest::header::RANGE, format!("bytes={have}-"));
        }
        let resp = match st.relay.send(req).await {
            Ok(r) => r,
            Err(NetError::NeedLogin) => {
                crate::net::need_login(app, st);
                return Err(NetError::NeedLogin.message());
            }
            Err(e) => {
                attempt += 1;
                if attempt > 5 {
                    return Err(e.message());
                }
                tokio::time::sleep(Duration::from_secs(2)).await;
                continue;
            }
        };
        let status = resp.status().as_u16();
        match status {
            202 => {
                // The home PC is still fetching it from WhatsApp.
                if started.elapsed() > Duration::from_secs(25 * 60) {
                    return Err("That took too long. Try again.".into());
                }
                let _ = app.emit("media-state", json!({"rowid": rowid, "state": "preparing"}));
                tokio::time::sleep(Duration::from_millis(800)).await;
            }
            200 | 206 => {
                let total = if status == 206 {
                    resp.headers()
                        .get(reqwest::header::CONTENT_RANGE)
                        .and_then(|v| v.to_str().ok())
                        .and_then(|v| v.rsplit('/').next())
                        .and_then(|v| v.parse::<u64>().ok())
                        .unwrap_or(0)
                } else {
                    resp.content_length().unwrap_or(0)
                };
                let mut file = tokio::fs::OpenOptions::new()
                    .create(true)
                    .write(true)
                    .truncate(status == 200)
                    .open(&part)
                    .await
                    .map_err(|e| e.to_string())?;
                let mut written = if status == 206 { have } else { 0 };
                if status == 206 {
                    file.seek(std::io::SeekFrom::Start(have)).await.map_err(|e| e.to_string())?;
                }
                use futures_util::StreamExt;
                let mut stream = resp.bytes_stream();
                let mut last_emit = Instant::now();
                let mut broke = false;
                loop {
                    match tokio::time::timeout(Duration::from_secs(60), stream.next()).await {
                        Ok(Some(Ok(bytes))) => {
                            file.write_all(&bytes).await.map_err(|e| e.to_string())?;
                            written += bytes.len() as u64;
                            if last_emit.elapsed() > Duration::from_millis(200) {
                                last_emit = Instant::now();
                                let _ = app.emit(
                                    "media-state",
                                    json!({"rowid": rowid, "state": "downloading", "done": written, "total": total}),
                                );
                            }
                        }
                        Ok(None) => break,
                        _ => {
                            broke = true;
                            break;
                        }
                    }
                }
                file.flush().await.map_err(|e| e.to_string())?;
                drop(file);
                if broke || (total > 0 && written < total) {
                    attempt += 1;
                    if attempt > 8 {
                        return Err("The download kept dropping. Try again.".into());
                    }
                    tokio::time::sleep(Duration::from_secs(1)).await;
                    continue;
                }
                tokio::fs::rename(&part, path).await.map_err(|e| e.to_string())?;
                return Ok(path.to_path_buf());
            }
            416 => {
                // The partial file doesn't match. Start over.
                let _ = tokio::fs::remove_file(&part).await;
                attempt += 1;
                if attempt > 3 {
                    return Err("Download failed.".into());
                }
            }
            _ => {
                let body: Value = resp.json().await.unwrap_or(Value::Null);
                let msg = body.get("error").and_then(Value::as_str).unwrap_or("That file isn't available.");
                return Err(msg.to_string());
            }
        }
    }
}

fn touch(path: &Path) {
    if let Ok(file) = std::fs::OpenOptions::new().write(true).open(path) {
        let _ = file.set_modified(std::time::SystemTime::now());
    }
}

fn cache_limit(st: &AppState) -> u64 {
    st.cache
        .meta_get("setting_media_cache_gb")
        .ok()
        .flatten()
        .and_then(|v| v.parse::<u64>().ok())
        .map(|gb| gb << 30)
        .unwrap_or(DEFAULT_CACHE_BYTES)
}

/// Removes the least recently used files until the cache is under its limit.
pub fn prune(dir: &Path, limit: u64) {
    let Ok(entries) = std::fs::read_dir(dir) else { return };
    let mut files: Vec<(PathBuf, u64, std::time::SystemTime)> = entries
        .flatten()
        .filter_map(|e| {
            let meta = e.metadata().ok()?;
            if !meta.is_file() {
                return None;
            }
            Some((e.path(), meta.len(), meta.modified().ok()?))
        })
        .collect();
    let mut total: u64 = files.iter().map(|f| f.1).sum();
    if total <= limit {
        return;
    }
    files.sort_by_key(|f| f.2);
    for (path, size, _) in files {
        if total <= limit {
            break;
        }
        if std::fs::remove_file(&path).is_ok() {
            total = total.saturating_sub(size);
        }
    }
}

pub fn cache_bytes(dir: &Path) -> u64 {
    std::fs::read_dir(dir)
        .map(|entries| entries.flatten().filter_map(|e| e.metadata().ok()).map(|m| m.len()).sum())
        .unwrap_or(0)
}

fn respond(status: StatusCode, body: Vec<u8>, content_type: &str) -> Response<Vec<u8>> {
    Response::builder()
        .status(status)
        .header(header::CONTENT_TYPE, content_type)
        .header(header::ACCESS_CONTROL_ALLOW_ORIGIN, "*")
        .header("X-Content-Type-Options", "nosniff")
        .body(body)
        .unwrap()
}

/// The document viewer's own rules. `sandbox allow-scripts` gives it an origin of its own that matches nothing, so it
/// can't reach the app, its commands or its storage, open windows or navigate the app. It can only load its own
/// code, show what it draws, and talk to the app by message. A document's bytes arrive by message, never by URL.
pub const VIEWER_CSP: &str = "sandbox allow-scripts; default-src 'none'; script-src http://wamedia.localhost/assets/; \
    worker-src blob:; style-src 'unsafe-inline' http://wamedia.localhost/assets/; img-src blob: data:; \
    font-src data: http://wamedia.localhost/assets/; connect-src http://wamedia.localhost/assets/; frame-src 'none'; \
    object-src 'none'; base-uri 'none'; form-action 'none'";

/// Serves the document viewer and its code from what's built into the app. Nothing on disk, and nothing else.
fn viewer_asset(app: &AppHandle, kind: &str, arg: &str) -> Response<Vec<u8>> {
    let name = if kind == "viewer" {
        "viewer.html".to_string()
    } else {
        let ok = !arg.is_empty()
            && arg.chars().all(|c| c.is_ascii_alphanumeric() || "-_.".contains(c))
            && !arg.contains("..")
            && [".js", ".mjs", ".css", ".wasm", ".woff2", ".woff", ".ttf"].iter().any(|ext| arg.ends_with(ext));
        if !ok {
            return respond(StatusCode::NOT_FOUND, vec![], "text/plain");
        }
        format!("assets/{arg}")
    };
    let Some(asset) = app.asset_resolver().get(name.clone()) else {
        return respond(StatusCode::NOT_FOUND, vec![], "text/plain");
    };
    let mime = if name.ends_with(".html") {
        "text/html; charset=utf-8".to_string()
    } else if name.ends_with(".js") || name.ends_with(".mjs") {
        "text/javascript".to_string()
    } else {
        asset.mime_type().to_string()
    };
    Response::builder()
        .status(StatusCode::OK)
        .header(header::CONTENT_TYPE, mime)
        .header(header::ACCESS_CONTROL_ALLOW_ORIGIN, "*")
        .header("Cross-Origin-Resource-Policy", "cross-origin")
        .header("X-Content-Type-Options", "nosniff")
        .header("Content-Security-Policy", VIEWER_CSP)
        .header("Referrer-Policy", "no-referrer")
        .body(asset.bytes().to_vec())
        .unwrap()
}

/// Only types the UI can show are served with their own type. Anything else, including an HTML or SVG
/// file claiming to be a photo, goes out as plain bytes, which no web view will run.
fn serve_type(mime: &str) -> String {
    let base = mime.split(';').next().unwrap_or("").trim().to_ascii_lowercase();
    let safe = base.starts_with("video/")
        || base.starts_with("audio/")
        || matches!(base.as_str(), "image/jpeg" | "image/png" | "image/gif" | "image/webp" | "image/bmp");
    if safe {
        base
    } else {
        "application/octet-stream".into()
    }
}

/// Handles http://wamedia.localhost/thumb/{rowid}, /file/{rowid} and /local/{token}.
const AVATAR_MISS_KEEP: std::time::Duration = std::time::Duration::from_secs(600);
const AVATAR_MAX_BYTES: usize = 2 << 20;
const JPEG_START: &[u8] = &[0xFF, 0xD8, 0xFF];

pub fn valid_jid(jid: &str) -> bool {
    !jid.is_empty() && jid.len() <= 80 && jid.chars().all(|c| c.is_ascii_alphanumeric() || "@.:_-".contains(c))
}

fn avatar_stem(jid: &str) -> String {
    jid.bytes().map(|b| format!("{b:02x}")).collect()
}

fn local_avatar(dir: &std::path::Path, stem: &str) -> Option<PathBuf> {
    let prefix = format!("{stem}-");
    std::fs::read_dir(dir)
        .ok()?
        .flatten()
        .map(|e| e.path())
        .filter(|p| p.file_name().and_then(|n| n.to_str()).is_some_and(|n| n.starts_with(&prefix) && n.ends_with(".jpg")))
        .max()
}

fn remove_local_avatars(dir: &std::path::Path, stem: &str, keep: Option<&std::path::Path>) {
    let prefix = format!("{stem}-");
    for entry in std::fs::read_dir(dir).into_iter().flatten().flatten() {
        let path = entry.path();
        let ours = path.file_name().and_then(|n| n.to_str()).is_some_and(|n| n.starts_with(&prefix));
        if ours && keep != Some(path.as_path()) {
            let _ = std::fs::remove_file(path);
        }
    }
}

fn serve_avatar(path: &std::path::Path) -> Option<Response<Vec<u8>>> {
    let bytes = std::fs::read(path).ok()?;
    bytes.starts_with(JPEG_START).then(|| {
        Response::builder()
            .status(StatusCode::OK)
            .header(header::CONTENT_TYPE, "image/jpeg")
            .header(header::ACCESS_CONTROL_ALLOW_ORIGIN, "*")
            .header("X-Content-Type-Options", "nosniff")
            .body(bytes)
            .unwrap()
    })
}

/// A chat's picture. Kept locally per version, so it works offline and costs the home PC nothing once
/// fetched. Anything other than a JPEG is refused, since the home PC always re-encodes to one.
async fn avatar(st: &Arc<AppState>, jid: &str, version: i64) -> Response<Vec<u8>> {
    // Having no picture is normal, so it isn't reported as an error. The image falls back to initials.
    let missing = || respond(StatusCode::NO_CONTENT, vec![], "text/plain");
    if !valid_jid(jid) {
        return respond(StatusCode::BAD_REQUEST, vec![], "text/plain");
    }
    let dir = st.media_dir.join("avatars");
    if std::fs::create_dir_all(&dir).is_err() {
        return missing();
    }
    let stem = avatar_stem(jid);
    if version > 0 {
        if let Some(found) = serve_avatar(&dir.join(format!("{stem}-{version}.jpg"))) {
            return found;
        }
    } else if let Some(found) = local_avatar(&dir, &stem).and_then(|p| serve_avatar(&p)) {
        return found;
    }
    if let Some((seen, at)) = st.avatar_misses.lock().unwrap().get(jid) {
        if *seen == version && at.elapsed() < AVATAR_MISS_KEEP {
            return missing();
        }
    }
    let query: String = url::form_urlencoded::byte_serialize(jid.as_bytes()).collect();
    let attempt = match st.relay.request(reqwest::Method::GET, &format!("/api/picture?jid={query}")) {
        Ok(req) => st.relay.send(req).await.ok(),
        Err(_) => None,
    };
    let Some(resp) = attempt else {
        // Offline, so whatever's saved is better than nothing.
        return local_avatar(&dir, &stem).and_then(|p| serve_avatar(&p)).unwrap_or_else(missing);
    };
    match resp.status().as_u16() {
        200 => {
            let given = resp
                .headers()
                .get("X-Picture-Version")
                .and_then(|v| v.to_str().ok())
                .and_then(|v| v.parse::<i64>().ok())
                .unwrap_or(version);
            let Ok(bytes) = resp.bytes().await else { return missing() };
            if bytes.len() > AVATAR_MAX_BYTES || !bytes.starts_with(JPEG_START) {
                return missing();
            }
            let path = dir.join(format!("{stem}-{given}.jpg"));
            if std::fs::write(&path, &bytes).is_ok() {
                remove_local_avatars(&dir, &stem, Some(&path));
            }
            serve_avatar(&path).unwrap_or_else(missing)
        }
        204 => {
            // The picture was taken away, so a saved copy shouldn't come back.
            remove_local_avatars(&dir, &stem, None);
            st.avatar_misses.lock().unwrap().insert(jid.to_string(), (version, std::time::Instant::now()));
            missing()
        }
        _ => {
            // Being looked up. The home PC announces it when it arrives.
            st.avatar_misses.lock().unwrap().insert(jid.to_string(), (version, std::time::Instant::now()));
            local_avatar(&dir, &stem).and_then(|p| serve_avatar(&p)).unwrap_or_else(missing)
        }
    }
}

pub async fn handle(app: AppHandle, request: Request<Vec<u8>>) -> Response<Vec<u8>> {
    let st = app.state::<Arc<AppState>>().inner().clone();
    let path = request.uri().path().to_string();
    let range = request
        .headers()
        .get(header::RANGE)
        .and_then(|v| v.to_str().ok())
        .map(str::to_string);
    let mut parts = path.trim_start_matches('/').splitn(2, '/');
    let kind = parts.next().unwrap_or("");
    let arg = parts.next().unwrap_or("");
    match kind {
        "viewer" | "assets" => viewer_asset(&app, kind, arg),
        "thumb" => {
            let Ok(rowid) = arg.parse::<i64>() else {
                return respond(StatusCode::BAD_REQUEST, vec![], "text/plain");
            };
            match st.cache.media_info(rowid) {
                Ok(Some(info)) if info.thumb.is_some() => {
                    let thumb = info.thumb.unwrap();
                    // Stickers carry PNG thumbnails and everything else JPEG; browsers sniff images.
                    respond(StatusCode::OK, thumb, "image/jpeg")
                }
                _ => respond(StatusCode::NOT_FOUND, vec![], "text/plain"),
            }
        }
        "file" => {
            let Ok(rowid) = arg.parse::<i64>() else {
                return respond(StatusCode::BAD_REQUEST, vec![], "text/plain");
            };
            let mime = st
                .cache
                .media_info(rowid)
                .ok()
                .flatten()
                .map(|i| i.mime)
                .unwrap_or_default();
            match ensure_cached(&app, &st, rowid).await {
                Ok(file) => serve_file(&file, &serve_type(&mime), range).await,
                Err(e) => respond(StatusCode::BAD_GATEWAY, e.into_bytes(), "text/plain"),
            }
        }
        "avatar" => {
            let version = request
                .uri()
                .query()
                .and_then(|q| q.strip_prefix("v="))
                .and_then(|v| v.parse::<i64>().ok())
                .unwrap_or(0);
            avatar(&st, arg, version).await
        }
        "local" => {
            let picked = st.picked.lock().unwrap().get(arg).cloned();
            match picked {
                Some(file) => {
                    let mime = local_mime(&file);
                    serve_file(&file, &mime, range).await
                }
                None => respond(StatusCode::NOT_FOUND, vec![], "text/plain"),
            }
        }
        _ => respond(StatusCode::NOT_FOUND, vec![], "text/plain"),
    }
}

fn local_mime(path: &Path) -> String {
    let ext = path.extension().and_then(|e| e.to_str()).unwrap_or("").to_ascii_lowercase();
    match ext.as_str() {
        "jpg" | "jpeg" => "image/jpeg",
        "png" => "image/png",
        "gif" => "image/gif",
        "webp" => "image/webp",
        "mp4" | "m4v" => "video/mp4",
        "webm" => "video/webm",
        "mov" => "video/quicktime",
        "mp3" => "audio/mpeg",
        "m4a" => "audio/mp4",
        "ogg" | "opus" => "audio/ogg",
        _ => "application/octet-stream",
    }
    .to_string()
}

/// Serves a file, or a slice of it. Slices are capped so a long video never loads into memory whole;
/// the player just asks for the next piece.
async fn serve_file(path: &Path, content_type: &str, range: Option<String>) -> Response<Vec<u8>> {
    let mut file = match tokio::fs::File::open(path).await {
        Ok(f) => f,
        Err(_) => return respond(StatusCode::NOT_FOUND, vec![], "text/plain"),
    };
    let total = match file.metadata().await {
        Ok(m) => m.len(),
        Err(_) => return respond(StatusCode::INTERNAL_SERVER_ERROR, vec![], "text/plain"),
    };
    if let Some((start, end)) = range.as_deref().and_then(|r| parse_range(r, total)) {
        let len = (end - start + 1).min(RANGE_LIMIT);
        let mut buf = vec![0u8; len as usize];
        if file.seek(std::io::SeekFrom::Start(start)).await.is_err() || file.read_exact(&mut buf).await.is_err() {
            return respond(StatusCode::INTERNAL_SERVER_ERROR, vec![], "text/plain");
        }
        return Response::builder()
            .status(StatusCode::PARTIAL_CONTENT)
            .header(header::CONTENT_TYPE, content_type)
            .header(header::ACCEPT_RANGES, "bytes")
            .header(header::CONTENT_RANGE, format!("bytes {}-{}/{}", start, start + len - 1, total))
            .header(header::ACCESS_CONTROL_ALLOW_ORIGIN, "*")
            .header("X-Content-Type-Options", "nosniff")
            .body(buf)
            .unwrap();
    }
    if range.is_some() {
        return Response::builder()
            .status(StatusCode::RANGE_NOT_SATISFIABLE)
            .header(header::CONTENT_RANGE, format!("bytes */{total}"))
            .body(vec![])
            .unwrap();
    }
    if total > RANGE_LIMIT && (content_type.starts_with("video/") || content_type.starts_with("audio/")) {
        let mut buf = vec![0u8; RANGE_LIMIT as usize];
        if file.read_exact(&mut buf).await.is_err() {
            return respond(StatusCode::INTERNAL_SERVER_ERROR, vec![], "text/plain");
        }
        return Response::builder()
            .status(StatusCode::PARTIAL_CONTENT)
            .header(header::CONTENT_TYPE, content_type)
            .header(header::ACCEPT_RANGES, "bytes")
            .header(header::CONTENT_RANGE, format!("bytes 0-{}/{}", RANGE_LIMIT - 1, total))
            .header(header::ACCESS_CONTROL_ALLOW_ORIGIN, "*")
            .body(buf)
            .unwrap();
    }
    // A file served whole goes into memory, so a stranger's oversized "photo" stops here.
    if total > WHOLE_FILE_LIMIT {
        return respond(StatusCode::PAYLOAD_TOO_LARGE, b"Too big to show here. Save it instead.".to_vec(), "text/plain");
    }
    let mut buf = Vec::with_capacity(total as usize);
    if file.read_to_end(&mut buf).await.is_err() {
        return respond(StatusCode::INTERNAL_SERVER_ERROR, vec![], "text/plain");
    }
    let mut resp = respond(StatusCode::OK, buf, content_type);
    resp.headers_mut()
        .insert(header::ACCEPT_RANGES, "bytes".parse().unwrap());
    resp
}

pub fn parse_range(header: &str, total: u64) -> Option<(u64, u64)> {
    if total == 0 {
        return None;
    }
    let spec = header.strip_prefix("bytes=")?;
    let (a, b) = spec.split_once(',').map(|(first, _)| first).unwrap_or(spec).split_once('-')?;
    let (start, end) = if a.is_empty() {
        let n: u64 = b.parse().ok()?;
        if n == 0 {
            return None;
        }
        (total.saturating_sub(n), total - 1)
    } else {
        let start: u64 = a.parse().ok()?;
        let end = if b.is_empty() { total - 1 } else { b.parse::<u64>().ok()?.min(total - 1) };
        (start, end)
    };
    if start > end || start >= total {
        return None;
    }
    Some((start, end))
}

/// What the UI needs to know about a file the user picked. It only ever gets the token, so UI code can't
/// choose which file on the disk gets sent.
pub fn register_picked(st: &AppState, path: PathBuf) -> Option<Value> {
    let meta = std::fs::metadata(&path).ok()?;
    if !meta.is_file() {
        return None;
    }
    let token = crate::random_id();
    let name = path.file_name()?.to_string_lossy().to_string();
    let ext = path.extension().and_then(|e| e.to_str()).unwrap_or("").to_ascii_lowercase();
    let kind = match ext.as_str() {
        "jpg" | "jpeg" | "png" | "webp" | "gif" => "image",
        "mp4" | "m4v" => "video",
        "mp3" | "m4a" | "aac" | "ogg" | "opus" => "audio",
        _ => "document",
    };
    let value = json!({"token": token, "name": name, "size": meta.len(), "kind": kind});
    st.picked.lock().unwrap().insert(token, path);
    Some(value)
}

pub struct SendRequest {
    pub chat: String,
    pub token: String,
    pub kind: String,
    pub caption: String,
    pub thumb_b64: Option<String>,
    pub width: u32,
    pub height: u32,
    pub seconds: u32,
    pub quote: Option<Value>,
    pub client_id: String,
    /// Addresses of the people the caption mentions.
    pub mentions: Vec<String>,
}

/// Uploads a file in chunks, so it fits Cloudflare's per-request limit and a dropped connection only
/// costs the current chunk. The final result arrives as a send-result event.
pub async fn send_file(app: AppHandle, st: Arc<AppState>, req: SendRequest) -> Result<(), String> {
    let path = st
        .picked
        .lock()
        .unwrap()
        .get(&req.token)
        .cloned()
        .ok_or("That file is no longer selected.")?;
    let size = tokio::fs::metadata(&path).await.map_err(|_| "Can't read that file.")?.len();
    if size == 0 {
        return Err("That file is empty.".into());
    }
    if size > MAX_UPLOAD {
        return Err("WhatsApp's limit is 2 GB per file.".into());
    }
    let upload_id = crate::random_id();
    let progress = |done: u64| {
        let _ = app.emit(
            "send-progress",
            json!({"client_id": req.client_id, "done": done, "total": size}),
        );
    };
    let mut file = tokio::fs::File::open(&path).await.map_err(|_| "Can't read that file.")?;
    let mut offset: u64 = 0;
    let mut buf = vec![0u8; CHUNK];
    let mut failures = 0;
    while offset < size {
        file.seek(std::io::SeekFrom::Start(offset)).await.map_err(|e| e.to_string())?;
        let want = ((size - offset) as usize).min(CHUNK);
        file.read_exact(&mut buf[..want]).await.map_err(|_| "Can't read that file.")?;
        let api = format!("/api/upload/{upload_id}?offset={offset}");
        let body = buf[..want].to_vec();
        let attempt = st
            .relay
            .long_request(reqwest::Method::PUT, &api)
            .map_err(|e| e.message())?
            .timeout(Duration::from_secs(600))
            .body(body);
        match st.relay.send(attempt).await {
            Ok(resp) if resp.status() == 200 => {
                offset += want as u64;
                failures = 0;
                progress(offset);
            }
            Ok(resp) if resp.status() == 409 => {
                // The home PC has a different amount than expected, so continue from its count.
                let doc: Value = resp.json().await.unwrap_or(Value::Null);
                offset = doc.get("size").and_then(Value::as_u64).unwrap_or(0);
            }
            Ok(resp) => {
                let doc: Value = resp.json().await.unwrap_or(Value::Null);
                return Err(doc.get("error").and_then(Value::as_str).unwrap_or("Upload failed.").to_string());
            }
            Err(NetError::NeedLogin) => {
                crate::net::need_login(&app, &st);
                return Err(NetError::NeedLogin.message());
            }
            Err(_) => {
                failures += 1;
                if failures > 5 {
                    return Err("The upload kept dropping. Try again.".into());
                }
                tokio::time::sleep(Duration::from_secs(2)).await;
                if let Ok(doc) = st.relay.get(&format!("/api/upload/{upload_id}")).await {
                    offset = doc.get("size").and_then(Value::as_u64).unwrap_or(offset);
                }
            }
        }
    }
    let name = path.file_name().map(|n| n.to_string_lossy().to_string()).unwrap_or_else(|| "file".into());
    let thumb = req
        .thumb_b64
        .as_deref()
        .and_then(|b| base64::engine::general_purpose::STANDARD.decode(b).ok())
        .map(|b| base64::engine::general_purpose::STANDARD.encode(b));
    let mut meta = json!({
        "chat": req.chat, "kind": req.kind, "filename": name, "caption": req.caption, "client_id": req.client_id,
        "thumb": thumb, "width": req.width, "height": req.height, "seconds": req.seconds, "quote": req.quote,
    });
    if !req.mentions.is_empty() {
        meta["mentions"] = json!(req.mentions);
    }
    st.relay
        .post(&format!("/api/upload/{upload_id}/finish"), &meta)
        .await
        .map_err(|e| e.message())?;
    st.picked.lock().unwrap().remove(&req.token);
    Ok(())
}

#[allow(dead_code)]
pub type GateMap = HashMap<i64, Arc<tokio::sync::Mutex<()>>>;

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn avatar_addresses_cant_escape_the_folder() {
        assert!(valid_jid("15550000001@s.whatsapp.net"));
        assert!(valid_jid("120363000000000001@g.us"));
        for bad in ["", "../../secret", "..\\secret", "a/b@g.us", "x y@g.us", "a%2fb"] {
            assert!(!valid_jid(bad), "{bad}");
        }
        assert!(avatar_stem("a@b").chars().all(|c| c.is_ascii_hexdigit()));
    }

    #[test]
    fn only_a_jpeg_is_ever_served_as_a_picture() {
        let dir = tempfile::tempdir().unwrap();
        let bad = dir.path().join("a-1.jpg");
        std::fs::write(&bad, b"<svg onload='x'/>").unwrap();
        assert!(serve_avatar(&bad).is_none());
        let good = dir.path().join("a-2.jpg");
        std::fs::write(&good, [0xFF, 0xD8, 0xFF, 0xE0]).unwrap();
        let resp = serve_avatar(&good).unwrap();
        assert_eq!(resp.headers().get(header::CONTENT_TYPE).unwrap(), "image/jpeg");
        assert_eq!(resp.headers().get("X-Content-Type-Options").unwrap(), "nosniff");
    }

    #[test]
    fn a_new_picture_replaces_the_saved_one() {
        let dir = tempfile::tempdir().unwrap();
        std::fs::write(dir.path().join("ab-1.jpg"), [0xFF]).unwrap();
        std::fs::write(dir.path().join("ab-2.jpg"), [0xFF]).unwrap();
        std::fs::write(dir.path().join("cd-1.jpg"), [0xFF]).unwrap();
        let keep = dir.path().join("ab-2.jpg");
        remove_local_avatars(dir.path(), "ab", Some(&keep));
        assert!(!dir.path().join("ab-1.jpg").exists() && keep.exists() && dir.path().join("cd-1.jpg").exists());
        remove_local_avatars(dir.path(), "ab", None);
        assert!(!keep.exists());
    }

    #[test]
    fn range_parsing() {
        assert_eq!(parse_range("bytes=0-99", 1000), Some((0, 99)));
        assert_eq!(parse_range("bytes=500-", 1000), Some((500, 999)));
        assert_eq!(parse_range("bytes=-100", 1000), Some((900, 999)));
        assert_eq!(parse_range("bytes=0-99999", 1000), Some((0, 999)));
        assert_eq!(parse_range("bytes=2000-3000", 1000), None);
        assert_eq!(parse_range("bytes=9-3", 1000), None);
        assert_eq!(parse_range("items=0-1", 1000), None);
        assert_eq!(parse_range("bytes=abc", 1000), None);
        assert_eq!(parse_range("bytes=0-1", 0), None);
    }

    #[test]
    fn names_from_strangers_are_cleaned() {
        assert_eq!(safe_name(r"..\..\Windows\evil.exe"), "evil.exe");
        assert_eq!(safe_name("../../etc/passwd"), "passwd");
        assert_eq!(safe_name("a:b*c?.txt"), "a_b_c_.txt");
        assert_eq!(safe_name("..."), "file");
        assert_eq!(safe_name(""), "file");
        assert_eq!(safe_name("name\u{202e}gpj.exe"), "name_gpj.exe");
    }

    #[test]
    fn programs_and_scripts_cannot_be_opened_directly() {
        for bad in ["a.exe", "a.bat", "a.cmd", "a.ps1", "a.vbs", "a.js", "a.lnk", "a.msi", "a.scr", "a.hta", "a.html", "a.svg", "noext", "a.pdf.exe", "a.doc", "a.xls", "a.ppt", "a.rtf", "a.docm", "a.xlsm"] {
            assert!(!can_open(bad), "{bad} must not open");
        }
        for good in ["a.pdf", "A.JPG", "report.docx", "clip.mp4"] {
            assert!(can_open(good), "{good} should open");
        }
    }

    #[test]
    fn received_files_get_the_internet_mark() {
        let dir = tempfile::tempdir().unwrap();
        let file = dir.path().join("report.docx");
        std::fs::write(&file, b"x").unwrap();
        mark_from_internet(&file).unwrap();
        let mark = std::fs::read_to_string(format!("{}:Zone.Identifier", file.display())).unwrap();
        assert!(mark.contains("ZoneId=3"));
        assert_eq!(std::fs::read(&file).unwrap(), b"x", "the file itself is untouched");
    }

    #[test]
    fn active_content_is_never_served_as_itself() {
        for bad in ["text/html", "image/svg+xml", "application/javascript", "text/xml", "application/xhtml+xml", ""] {
            assert_eq!(serve_type(bad), "application/octet-stream", "{bad}");
        }
        assert_eq!(serve_type("image/jpeg"), "image/jpeg");
        assert_eq!(serve_type("video/mp4"), "video/mp4");
        assert_eq!(serve_type("audio/ogg; codecs=opus"), "audio/ogg");
    }

    #[test]
    fn prune_removes_least_recently_used_first() {
        let dir = tempfile::tempdir().unwrap();
        let old = dir.path().join("old.bin");
        let new = dir.path().join("new.bin");
        std::fs::write(&old, vec![0u8; 100]).unwrap();
        std::fs::write(&new, vec![0u8; 100]).unwrap();
        let file = std::fs::OpenOptions::new().write(true).open(&old).unwrap();
        file.set_modified(std::time::SystemTime::now() - Duration::from_secs(3600)).unwrap();
        drop(file);
        prune(dir.path(), 150);
        assert!(!old.exists());
        assert!(new.exists());
        assert_eq!(cache_bytes(dir.path()), 100);
    }
}
