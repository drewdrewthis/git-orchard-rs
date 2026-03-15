/// Opens `url` in the system default browser. Fire-and-forget; errors are silently ignored.
pub fn open_url(url: &str) {
    let _ = open::that(url);
}
