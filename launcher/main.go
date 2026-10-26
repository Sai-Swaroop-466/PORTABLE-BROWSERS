package main

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
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

type release struct {
	Tag        string `json:"tag_name"`
	Draft      bool   `json:"draft"`
	Prerelease bool   `json:"prerelease"`
	Assets     []struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
	} `json:"assets"`
}

func readTrim(p string) string {
	b, _ := os.ReadFile(p)
	return strings.TrimSpace(string(b))
}

func checkForUpdate() {
	if Repo == "" || exists(filepath.Join(data, "no-update")) {
		return
	}
	stamp := filepath.Join(stage, "last_check")
	if fi, err := os.Stat(stamp); err == nil && time.Since(fi.ModTime()) < 3*time.Hour {
		return
	}
	client := &http.Client{Timeout: 30 * time.Minute}
	req, _ := http.NewRequest("GET", "https://api.github.com/repos/"+Repo+"/releases?per_page=30", nil)
	req.Header.Set("User-Agent", "portable-launcher")
	resp, err := client.Do(req)
	if err != nil || resp.StatusCode != 200 {
		logf("release check failed: %v", err)
		return
	}
	var rels []release
	err = json.NewDecoder(resp.Body).Decode(&rels)
	resp.Body.Close()
	if err != nil {
		return
	}
	os.WriteFile(stamp, []byte(time.Now().String()), 0o644)

	var rel *release
	for i := range rels {
		if !rels[i].Draft && strings.HasPrefix(rels[i].Tag, App+"-") {
			rel = &rels[i]
			break
		}
	}
	if rel == nil {
		return
	}
	if rel.Tag == readTrim(filepath.Join(root, "App", "version.txt")) ||
		rel.Tag == readTrim(filepath.Join(stage, "App.new", "version.txt")) {
		return
	}
	var zipURL, shaURL string
	for _, a := range rel.Assets {
		if strings.HasSuffix(a.Name, ".zip") {
			zipURL = a.URL
		} else if strings.HasSuffix(a.Name, ".zip.sha256") {
			shaURL = a.URL
		}
	}
	if zipURL == "" || shaURL == "" {
		return
	}
	logf("new version %s found, downloading", rel.Tag)

	// expected hash
	r2, err := client.Get(shaURL)
	if err != nil {
		return
	}
	sb, _ := io.ReadAll(r2.Body)
	r2.Body.Close()
	fields := strings.Fields(string(sb))
	if len(fields) == 0 {
		return
	}
	want := strings.ToLower(fields[0])

	// download + hash
	pkg := filepath.Join(stage, "pkg.zip")
	r3, err := client.Get(zipURL)
	if err != nil || r3.StatusCode != 200 {
		logf("download failed: %v", err)
		return
	}
	out, err := os.Create(pkg)
	if err != nil {
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

	// extract App/ only
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
	logf("%s staged; will apply on next launch", rel.Tag)
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
