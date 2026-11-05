package main

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Set at build time:
//   -X main.App=zen|vivaldi   -X main.Repo=owner/repo
var (
	App  = "zen"
	Repo = ""
)

const zenPrefs = `user_pref("app.update.auto", false);
user_pref("app.update.enabled", false);
user_pref("app.update.service.enabled", false);
user_pref("browser.shell.checkDefaultBrowser", false);
user_pref("browser.cache.disk.enable", false);
user_pref("browser.cache.memory.enable", true);
user_pref("browser.cache.memory.capacity", 262144);
user_pref("datareporting.healthreport.uploadEnabled", false);
user_pref("toolkit.telemetry.enabled", false);
`

var root, data, stage string

func logf(f string, a ...any) {
	fh, err := os.OpenFile(filepath.Join(data, "update.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer fh.Close()
	fmt.Fprintf(fh, "%s %s\n", time.Now().Format("2006-01-02 15:04:05"), fmt.Sprintf(f, a...))
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

func main() {
	self, err := os.Executable()
	if err != nil {
		os.Exit(1)
	}
	root = filepath.Dir(self)
	data = filepath.Join(root, "Data")
	stage = filepath.Join(data, "update")
	profile := filepath.Join(data, "profile")
	temp := filepath.Join(data, "temp")
	cache := filepath.Join(data, "cache")
	for _, d := range []string{profile, temp, cache, stage} {
		os.MkdirAll(d, 0o755)
	}
	os.Setenv("TEMP", temp)
	os.Setenv("TMP", temp)

	// 1) If an update was downloaded last time, swap it in now (only App/ is touched).
	applyPending()

	// 2) Start the browser.
	var bin string
	var args []string
	switch App {
	case "vivaldi":
		bin = filepath.Join(root, "App", "vivaldi.exe")
		args = []string{
			"--user-data-dir=" + profile,
			"--disk-cache-dir=" + cache,
			"--disable-encryption-win",
			"--disable-encryption",
			"--disable-machine-id",
			"--password-store=basic",
			"--disable-component-update",
			"--disable-breakpad",
			"--no-report-upload",
			"--disable-logging",
			"--disable-features=FlashDeprecationWarning,DefaultBrowserPrompt",
			"--no-default-browser-check",
			"--vivaldi-standalone",
		}
	default:
		bin = filepath.Join(root, "App", "zen.exe")
		up := filepath.Join(profile, "user.js")
		if !exists(up) {
			os.WriteFile(up, []byte(zenPrefs), 0o644)
		}
		args = []string{"--profile", profile, "--no-remote"}
	}
	args = append(args, os.Args[1:]...)
	cmd := exec.Command(bin, args...)
	cmd.Dir = filepath.Dir(bin)
	if err := cmd.Start(); err != nil {
		logf("start failed: %v", err)
		os.Exit(1)
	}

	// 3) In the background, look for a newer build and stage it for next launch.
	func() {
		defer func() { recover() }()
		checkForUpdate()
	}()
	os.RemoveAll(filepath.Join(root, "App.old"))
}

func applyPending() {
	newApp := filepath.Join(stage, "App.new")
	if !exists(filepath.Join(newApp, "version.txt")) {
		return
	}
	appDir := filepath.Join(root, "App")
	old := filepath.Join(root, "App.old")
	os.RemoveAll(old)
	if err := os.Rename(appDir, old); err != nil { // browser still running -> files locked
		logf("apply skipped (App in use): %v", err)
		return
	}
	if err := os.Rename(newApp, appDir); err != nil {
		os.Rename(old, appDir)
		logf("apply failed, rolled back: %v", err)
		return
	}
	logf("update applied")
}

func readTrim(p string) string {
	b, _ := os.ReadFile(p)
	return strings.TrimSpace(string(b))
}

func assetName() string {
	if App == "vivaldi" {
		return "VivaldiPortable-windows-x64.zip"
	}
	return "ZenPortable-windows-x64.zip"
}

var tagRe = regexp.MustCompile(`/releases/tag/([^"<\s]+)`)

// Reads the public releases feed (no API, no rate limit) and returns the newest tag for this app.
func latestTag() (string, error) {
	c := &http.Client{Timeout: 20 * time.Second}
	resp, err := c.Get("https://github.com/" + Repo + "/releases.atom")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("feed status %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	for _, m := range tagRe.FindAllStringSubmatch(string(body), -1) {
		if strings.HasPrefix(m[1], App+"-") {
			return m[1], nil
		}
	}
	return "", fmt.Errorf("no %s release in feed", App)
}

func checkForUpdate() {
	if Repo == "" || exists(filepath.Join(data, "no-update")) {
		return
	}
	cur := readTrim(filepath.Join(root, "App", "version.txt"))
	tag, err := latestTag()
	if err != nil {
		logf("release check failed: %v", err)
		return
	}
	logf("checked: latest=%s installed=%s", tag, cur)
	if tag == cur || tag == readTrim(filepath.Join(stage, "App.new", "version.txt")) {
		return
	}

	// only one launcher may download at a time (stale locks expire after 30 min)
	lock := filepath.Join(stage, "lock")
	if fi, err := os.Stat(lock); err == nil && time.Since(fi.ModTime()) > 30*time.Minute {
		os.Remove(lock)
	}
	lf, err := os.OpenFile(lock, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	lf.Close()
	defer os.Remove(lock)

	client := &http.Client{Timeout: 30 * time.Minute}
	zipURL := "https://github.com/" + Repo + "/releases/download/" + tag + "/" + assetName()
	logf("new version %s found, downloading", tag)

	r2, err := client.Get(zipURL + ".sha256")
	if err != nil || r2.StatusCode != 200 {
		logf("hash file download failed: %v", err)
		return
	}
	sb, _ := io.ReadAll(io.LimitReader(r2.Body, 4096))
	r2.Body.Close()
	fields := strings.Fields(string(sb))
	if len(fields) == 0 {
		return
	}
	want := strings.ToLower(fields[0])

	pkg := filepath.Join(stage, "pkg.zip")
	r3, err := client.Get(zipURL)
	if err != nil || r3.StatusCode != 200 {
		logf("download failed: %v", err)
		return
	}
	out, err := os.Create(pkg)
	if err != nil {
		r3.Body.Close()
		return
	}
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(out, h), r3.Body)
	r3.Body.Close()
	out.Close()
	if err != nil || hex.EncodeToString(h.Sum(nil)) != want {
		logf("download/hash mismatch: %v", err)
		os.Remove(pkg)
		return
	}

	tmp := filepath.Join(stage, "App.tmp")
	if err := extractApp(pkg, tmp); err != nil || !exists(filepath.Join(tmp, "version.txt")) {
		logf("extract failed: %v", err)
		os.RemoveAll(tmp)
		os.Remove(pkg)
		return
	}
	newApp := filepath.Join(stage, "App.new")
	os.RemoveAll(newApp)
	if err := os.Rename(tmp, newApp); err != nil {
		return
	}
	os.Remove(pkg)
	logf("%s staged; will apply on next launch", tag)
}

func extractApp(zipPath, dest string) error {
	r, err := zip.OpenReader(zipPath)
	if err != nil {
		return err
	}
	defer r.Close()
	os.RemoveAll(dest)
	clean := filepath.Clean(dest) + string(os.PathSeparator)
	for _, f := range r.File {
		parts := strings.Split(f.Name, "/")
		if len(parts) < 3 || parts[1] != "App" {
			continue
		}
		rel := filepath.Join(parts[2:]...)
		if rel == "" {
			continue
		}
		target := filepath.Join(dest, rel)
		if !strings.HasPrefix(target, clean) {
			continue
		}
		if f.FileInfo().IsDir() {
			os.MkdirAll(target, 0o755)
			continue
		}
		os.MkdirAll(filepath.Dir(target), 0o755)
		rc, err := f.Open()
		if err != nil {
			return err
		}
		o, err := os.Create(target)
		if err != nil {
			rc.Close()
			return err
		}
		_, err = io.Copy(o, rc)
		o.Close()
		rc.Close()
		if err != nil {
			return err
		}
	}
	return nil
}
