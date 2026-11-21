package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"tailscale.com/tsnet"
)

const uiAddr = "127.0.0.1:8099"

type Config struct {
	AuthKey string `json:"authKey,omitempty"`
	Target  string `json:"target"`     // tailnet host or IP of Jellyfin
	Port    int    `json:"port"`       // port on Jellyfin and on this machine
	Running bool   `json:"running"`    // auto-start on launch
}

type App struct {
	mu      sync.Mutex
	cfg     Config
	cfgPath string
	dir     string
	srv     *tsnet.Server
	ln      net.Listener
	err     string
	up      bool
}

func main() {
	install := flag.Bool("install", false, "install launch-at-login entry and exit")
	noBrowser := flag.Bool("no-browser", false, "don't open the browser on start")
	flag.Parse()

	if *install {
		if err := installAutostart(); err != nil {
			log.Fatal(err)
		}
		fmt.Println("Installed. It will start at next login.")
		return
	}

	base, err := os.UserConfigDir()
	if err != nil {
		log.Fatal(err)
	}
	dir := filepath.Join(base, "jellyfin-proxy")
	os.MkdirAll(dir, 0o700)

	a := &App{dir: dir, cfgPath: filepath.Join(dir, "config.json")}
	a.cfg = Config{Port: 8096}
	if b, err := os.ReadFile(a.cfgPath); err == nil {
		json.Unmarshal(b, &a.cfg)
	}
	if a.cfg.Port == 0 {
		a.cfg.Port = 8096
	}
	if a.cfg.Running && a.cfg.Target != "" {
		go a.start()
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", a.page)
	mux.HandleFunc("/status", a.status)
	mux.HandleFunc("/start", a.handleStart)
	mux.HandleFunc("/stop", a.handleStop)

	ln, err := net.Listen("tcp", uiAddr)
	if err != nil {
		log.Fatalf("UI port in use (is it already running?): %v", err)
	}
	if !*noBrowser {
		go func() { time.Sleep(300 * time.Millisecond); openBrowser("http://" + uiAddr) }()
	}
	log.Printf("UI at http://%s", uiAddr)
	log.Fatal(http.Serve(ln, mux))
}

func (a *App) save() {
	b, _ := json.MarshalIndent(a.cfg, "", "  ")
	os.WriteFile(a.cfgPath, b, 0o600)
}

func (a *App) start() {
	a.mu.Lock()
	if a.srv != nil {
		a.mu.Unlock()
		return
	}
	a.err = ""
	a.up = false
	s := &tsnet.Server{
		Hostname: "jellyfin-proxy",
		Dir:      filepath.Join(a.dir, "ts"),
		AuthKey:  a.cfg.AuthKey,
		Logf:     func(string, ...any) {},
	}
	a.srv = s
	port := a.cfg.Port
	target := net.JoinHostPort(a.cfg.Target, fmt.Sprint(port))
	a.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if _, err := s.Up(ctx); err != nil {
		a.fail("Tailscale connect failed: " + err.Error())
		return
	}

	ln, err := net.Listen("tcp", fmt.Sprintf("0.0.0.0:%d", port))
	if err != nil {
		a.fail("Can't listen on port: " + err.Error())
		return
	}

	a.mu.Lock()
	a.ln = ln
	a.up = true
	a.cfg.AuthKey = "" // no longer needed once the node state is saved
	a.cfg.Running = true
	a.save()
	a.mu.Unlock()

	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			defer c.Close()
			dctx, dc := context.WithTimeout(context.Background(), 15*time.Second)
			defer dc()
			up, err := s.Dial(dctx, "tcp", target)
			if err != nil {
				return
			}
			defer up.Close()
			done := make(chan struct{}, 2)
			go func() { io.Copy(up, c); done <- struct{}{} }()
			go func() { io.Copy(c, up); done <- struct{}{} }()
			<-done
		}()
	}
}

func (a *App) fail(msg string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.err = msg
	if a.srv != nil {
		a.srv.Close()
		a.srv = nil
	}
}

func (a *App) stop() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.ln != nil {
		a.ln.Close()
		a.ln = nil
	}
	if a.srv != nil {
		a.srv.Close()
		a.srv = nil
	}
	a.up = false
	a.cfg.Running = false
	a.save()
}

func (a *App) handleStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", 405)
		return
	}
	var in struct {
		AuthKey string `json:"authKey"`
		Target  string `json:"target"`
		Port    int    `json:"port"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	in.Target = strings.TrimSpace(in.Target)
	if in.Target == "" {
		http.Error(w, "target required", 400)
		return
	}
	if in.Port == 0 {
		in.Port = 8096
	}
	a.mu.Lock()
	a.cfg.Target = in.Target
	a.cfg.Port = in.Port
	if k := strings.TrimSpace(in.AuthKey); k != "" {
		a.cfg.AuthKey = k
	}
	a.save()
	a.mu.Unlock()
	go a.start()
	w.WriteHeader(204)
}

func (a *App) handleStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", 405)
		return
	}
	a.stop()
	w.WriteHeader(204)
}

func (a *App) status(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	state := "stopped"
	if a.srv != nil {
		state = "connecting"
	}
	if a.up {
		state = "running"
	}
	out := map[string]any{
		"state":  state,
		"error":  a.err,
		"target": a.cfg.Target,
		"port":   a.cfg.Port,
		"hasKey": a.cfg.AuthKey != "",
		"lan":    lanAddrs(a.cfg.Port),
		"path":   "",
	}
	srv := a.srv
	target := a.cfg.Target
	a.mu.Unlock()

	if srv != nil && state == "running" {
		out["path"] = pathTo(srv, target)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

// pathTo reports "direct", "relay" or "unknown" for the peer matching target.
func pathTo(s *tsnet.Server, target string) string {
	lc, err := s.LocalClient()
	if err != nil {
		return "unknown"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	st, err := lc.Status(ctx)
	if err != nil {
		return "unknown"
	}
	t := strings.ToLower(target)
	for _, p := range st.Peer {
		match := strings.HasPrefix(strings.ToLower(p.HostName), strings.Split(t, ".")[0])
		for _, ip := range p.TailscaleIPs {
			if ip.String() == t {
				match = true
			}
		}
		if match {
			if p.CurAddr != "" {
				return "direct"
			}
			if p.Relay != "" {
				return "relay"
			}
		}
	}
	return "unknown"
}

func lanAddrs(port int) []string {
	var out []string
	addrs, _ := net.InterfaceAddrs()
	for _, ad := range addrs {
		ipn, ok := ad.(*net.IPNet)
		if !ok || ipn.IP.IsLoopback() || ipn.IP.To4() == nil {
			continue
		}
		ip := ipn.IP
		if ip[0] == 100 && ip[1] >= 64 && ip[1] <= 127 { // skip tailscale CGNAT
			continue
		}
		if ip.IsLinkLocalUnicast() {
			continue
		}
		out = append(out, fmt.Sprintf("http://%s:%d", ip, port))
	}
	return out
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	cmd.Start()
}

func installAutostart() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	home, _ := os.UserHomeDir()
	switch runtime.GOOS {
	case "darwin":
		p := filepath.Join(home, "Library/LaunchAgents/com.jellyfin-proxy.plist")
		os.MkdirAll(filepath.Dir(p), 0o755)
		plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>Label</key><string>com.jellyfin-proxy</string>
<key>ProgramArguments</key><array><string>%s</string><string>--no-browser</string></array>
<key>RunAtLoad</key><true/>
<key>KeepAlive</key><true/>
</dict></plist>`, exe)
		if err := os.WriteFile(p, []byte(plist), 0o644); err != nil {
			return err
		}
		return exec.Command("launchctl", "load", p).Run()
	case "windows":
		return exec.Command("reg", "add", `HKCU\Software\Microsoft\Windows\CurrentVersion\Run`,
			"/v", "JellyfinProxy", "/t", "REG_SZ", "/d", `"`+exe+`" --no-browser`, "/f").Run()
	default:
		p := filepath.Join(home, ".config/systemd/user/jellyfin-proxy.service")
		os.MkdirAll(filepath.Dir(p), 0o755)
		unit := fmt.Sprintf(`[Unit]
Description=Jellyfin Tailscale proxy
After=network-online.target

[Service]
ExecStart=%s --no-browser
Restart=always

[Install]
WantedBy=default.target
`, exe)
		if err := os.WriteFile(p, []byte(unit), 0o644); err != nil {
			return err
		}
		exec.Command("systemctl", "--user", "daemon-reload").Run()
		return exec.Command("systemctl", "--user", "enable", "--now", "jellyfin-proxy.service").Run()
	}
}

func (a *App) page(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	io.WriteString(w, pageHTML)
}

const pageHTML = `<!doctype html>
<html><head><meta charset="utf-8"><title>Jellyfin Proxy</title>
<meta name="viewport" content="width=device-width,initial-scale=1">
<style>
:root{color-scheme:light dark;font-family:system-ui,sans-serif}
body{max-width:480px;margin:40px auto;padding:0 16px}
label{display:block;margin:12px 0 4px;font-size:.9em;opacity:.8}
input{width:100%;box-sizing:border-box;padding:8px;font-size:1em}
button{margin-top:16px;padding:10px 18px;font-size:1em;cursor:pointer}
.dot{display:inline-block;width:12px;height:12px;border-radius:50%;margin-right:8px;background:#888}
.running .dot{background:#2ecc71}.connecting .dot{background:#f1c40f}
.err{color:#e74c3c;margin-top:12px}
.addr{font-family:monospace;font-size:1.2em;margin:4px 0}
</style></head><body>
<h2><span id="s" class="stopped"><span class="dot"></span><span id="st">Stopped</span></span></h2>
<div id="info"></div>
<div id="err" class="err"></div>
<div id="form">
<label>Auth key (first run only)</label><input id="key" type="password" placeholder="tskey-auth-...">
<label>Jellyfin tailnet hostname or IP</label><input id="target" placeholder="media-server">
<label>Port</label><input id="port" value="8096">
</div>
<button id="btn"></button>
<script>
let state="stopped",filled=false;
async function poll(){
  const j=await (await fetch('/status')).json();
  state=j.state;
  document.getElementById('s').className=state;
  document.getElementById('st').textContent=
    state=='running'?'Connected'+(j.path&&j.path!='unknown'?' ('+j.path+')':''):state=='connecting'?'Connecting...':'Stopped';
  document.getElementById('err').textContent=j.error||'';
  if(!filled){document.getElementById('target').value=j.target||'';document.getElementById('port').value=j.port||8096;filled=true}
  document.getElementById('form').style.display=state=='stopped'?'block':'none';
  document.getElementById('btn').textContent=state=='stopped'?'Start':'Stop';
  document.getElementById('info').innerHTML=state=='running'
    ?'<p>Enter this on the TV:</p>'+(j.lan.map(a=>'<div class="addr">'+a+'</div>').join('')||'<div>No LAN address found</div>'):'';
}
document.getElementById('btn').onclick=async()=>{
  if(state=='stopped'){
    await fetch('/start',{method:'POST',body:JSON.stringify({
      authKey:document.getElementById('key').value,
      target:document.getElementById('target').value,
      port:parseInt(document.getElementById('port').value)||8096})});
  }else await fetch('/stop',{method:'POST'});
  poll();
};
poll();setInterval(poll,2000);
</script></body></html>`
