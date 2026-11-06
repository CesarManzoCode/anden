package main

// syntheticImage covers what a rootfs comparison has to get right. The lower
// layer is gzip and the upper one plain tar, so both layer formats are used.
func syntheticImage() image {
	lower := []entry{
		dirEntry("bin", 0o755),
		textEntry("bin/tool", 0o755, "#!/bin/sh\n"),
		textEntry("bin/setuid", 0o4755, "setuid\n"),
		textEntry("bin/setgid", 0o2755, "setgid\n"),
		dirEntry("sticky", 0o1777),
		dirEntry("private", 0o700),
		textEntry("private/secret", 0o600, "secret\n"),
		dirEntry("etc", 0o755),
		textEntry("etc/config", 0o644, "base\n"),
		hardlinkEntry("etc/config.hard", "etc/config", 0o644),
		symlinkEntry("etc/config.link", "config"),
		symlinkEntry("dangling", "/nonexistent"),
		textEntry("etc/replaced", 0o644, "lower\n"),
		dirEntry("data", 0o755),
		textEntry("data/x", 0o644, "removed by a whiteout\n"),
		textEntry("data/kept", 0o644, "kept\n"),
		dirEntry("gone", 0o755),
		textEntry("gone/child", 0o644, "removed with its directory\n"),
		dirEntry("opaque", 0o755),
		textEntry("opaque/old", 0o644, "hidden by the opaque marker\n"),
	}
	upper := []entry{
		dirEntry("data", 0o755),
		textEntry("data/.wh.x", 0o644, ""),
		textEntry(".wh.gone", 0o644, ""),
		dirEntry("opaque", 0o755),
		textEntry("opaque/.wh..wh..opq", 0o644, ""),
		textEntry("opaque/new", 0o644, "new\n"),
		dirEntry("etc", 0o755),
		textEntry("etc/replaced", 0o644, "upper\n"),
		textEntry("added", 0o640, "added\n"),
	}
	return image{
		layers: []layerSpec{{entries: lower, compress: true}, {entries: upper}},
		cmd:    []string{"/bin/tool"},
		env:    []string{"PATH=/bin"},
	}
}
